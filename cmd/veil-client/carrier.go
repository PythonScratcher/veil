package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	utls "github.com/refraction-networking/utls"

	"veil/proto"
)

// Transport multiplexes every tunneled connection over a "carrier": one
// encrypted session that rides a Chrome-fingerprinted HTTP/2 connection.
//
//	stream: the carrier is one long bidirectional POST
//	split:  the carrier's download is one long POST response and its upload is
//	        a stream of many short POSTs (CDN-friendly: no request is both long and bidirectional)
//
// Carriers are rotated on a jittered timer so no single TLS connection lives
// (or carries) unusually long, and every carrier emits cover traffic: pad
// frames inside, and update-site page fetches next to it.
type Transport struct {
	serverPub, psk []byte
	sni, path      string
	mode           string
	frag           string
	rotate         time.Duration
	cover          bool
	dialAddr       string
	caPool         *x509.CertPool
	insecureTLS    bool
	certFallback   atomic.Bool
	wsAddr, wsDial string // plain-HTTP carrier address (and where to actually connect)
	carrierMode    string // tls (default) | ws | poll | quic
	pollHC         *http.Client
	pollOnce       sync.Once
	dialer         *net.Dialer

	Up, Down atomic.Int64 // payload bytes through the tunnel (for the GUI)
	closed   bool

	// limits holds the most recent AccountLimits the server reported (expiry, data cap/usage,
	// rate limit), refreshed on every successful handshake - including carrier rotations, so the
	// GUI's view of them stays current without a separate query. nil until the first handshake.
	limits atomic.Pointer[proto.AccountLimits]

	// onAccountIssue fires once when the server rejects a stream for an account-wide reason
	// (disabled/expired/over quota) rather than a per-destination one. Set via OnAccountIssue.
	onAccountIssue func(msg string)
	acctOnce       sync.Once

	mu      sync.Mutex
	cur     *carrier
	dialing chan struct{}
	dialErr error

	// life spans the whole Transport (cancelled only by Close), independent of any one caller's
	// context. The actual carrier dial runs on a child of life, not of whichever caller happened
	// to be first in line to perform it - otherwise, when many callers pile up waiting for a
	// carrier at once (e.g. a burst of new TUN connections) and the unlucky one that ends up
	// doing the dial has an ordinary short-lived context of its own, that caller giving up would
	// cancel the dial for every other caller too, tearing down the one shared carrier and, with
	// it, every other stream riding on it - including whatever Chrome happened to be doing at the
	// time (seen as ERR_NETWORK_CHANGED, repeatedly, since the next burst does it again).
	life       context.Context
	lifeCancel context.CancelFunc
}

type carrier struct {
	sess     *proto.Session
	pool     *h2Pool
	expires  time.Time
	stop     chan struct{}
	stopOnce sync.Once
}

// OnAccountIssue registers a callback that fires at most once per Transport, the first time the
// server declines a connection because of the key itself (disabled, expired, or out of data) -
// as opposed to a particular destination being blocked or slow. The caller should treat this as
// fatal: the account problem won't clear up by retrying, so continuing to hold the tunnel open is
// pointless.
func (t *Transport) OnAccountIssue(f func(msg string)) { t.onAccountIssue = f }

// Limits returns the most recent account limits the server reported, or nil if none have been
// received yet (e.g. not connected, or talking to an older server build that doesn't send them).
func (t *Transport) Limits() *proto.AccountLimits { return t.limits.Load() }

func NewTransport(cfg *Config, serverPub, psk []byte) (*Transport, error) {
	host, port, err := net.SplitHostPort(cfg.Server)
	if err != nil {
		return nil, err
	}
	t := &Transport{serverPub: serverPub, psk: psk, sni: cfg.SNI, path: cfg.Path, mode: cfg.Transport, frag: cfg.Frag, cover: cfg.Cover == nil || *cfg.Cover}
	if t.sni == "" {
		t.sni = host
	}
	switch t.mode {
	case "":
		t.mode = "stream"
	case "stream", "split":
	default:
		return nil, fmt.Errorf("transport must be \"stream\" or \"split\", got %q", t.mode)
	}
	switch t.frag {
	case "":
		t.frag = "both"
	case "none", "tcp", "record", "both":
	default:
		return nil, fmt.Errorf("frag must be none|tcp|record|both, got %q", t.frag)
	}
	switch {
	case cfg.Rotate == 0:
		t.rotate = 20 * time.Minute
	case cfg.Rotate > 0:
		t.rotate = time.Duration(cfg.Rotate) * time.Minute
	}
	t.insecureTLS = cfg.InsecureTLS
	switch cfg.Carrier {
	case "", "tls":
		t.carrierMode = "tls"
	case "ws", "poll", "quic":
		t.carrierMode = cfg.Carrier
	default:
		return nil, fmt.Errorf("carrier must be tls|ws|poll|quic, got %q", cfg.Carrier)
	}
	if t.carrierMode == "ws" || t.carrierMode == "poll" {
		// plain-HTTP address: explicit ws_addr, else the server host on port 80
		t.wsAddr = cfg.WSAddr
		if t.wsAddr == "" {
			t.wsAddr = net.JoinHostPort(host, "80")
		}
		wh, wp, err := net.SplitHostPort(t.wsAddr)
		if err != nil {
			return nil, fmt.Errorf("ws_addr: %w", err)
		}
		t.wsDial = t.wsAddr
		if cfg.ConnectIP != "" {
			t.wsDial = net.JoinHostPort(cfg.ConnectIP, wp)
		}
		_ = wh
	}
	t.dialAddr = cfg.Server
	if cfg.ConnectIP != "" {
		t.dialAddr = net.JoinHostPort(cfg.ConnectIP, port)
	}
	if cfg.CAFile != "" || cfg.CAPEM != "" {
		pem := []byte(cfg.CAPEM)
		if cfg.CAFile != "" {
			b, err := os.ReadFile(cfg.CAFile)
			if err != nil {
				return nil, err
			}
			pem = append(pem, b...)
		}
		t.caPool = x509.NewCertPool()
		if !t.caPool.AppendCertsFromPEM(pem) {
			return nil, errors.New("ca_pem/ca_file: no valid certificate")
		}
	}
	t.dialer = &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	t.life, t.lifeCancel = context.WithCancel(context.Background())
	return t, nil
}

func (t *Transport) newPool() *h2Pool {
	return &h2Pool{dial: t.dialConn}
}

func (t *Transport) dialTLS(ctx context.Context) (net.Conn, error) {
	// The outer certificate is only a disguise (the inner handshake authenticates the server),
	// so if verification fails - typically a DPI box swapping the cert - retry without it.
	attempt := func(skip bool) (*utls.UConn, net.Conn, error) {
		raw, err := t.dialer.DialContext(ctx, "tcp", t.dialAddr)
		if err != nil {
			return nil, nil, err
		}
		uc := utls.UClient(newFragConn(raw, t.frag, t.sni), &utls.Config{ServerName: t.sni, RootCAs: t.caPool, InsecureSkipVerify: skip}, utls.HelloChrome_Auto)
		if err := uc.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, nil, err
		}
		return uc, raw, nil
	}
	uc, raw, err := attempt(t.insecureTLS || t.certFallback.Load())
	if err != nil && !t.insecureTLS && !t.certFallback.Load() && strings.Contains(err.Error(), "certificate") {
		if uc, raw, err = attempt(true); err == nil {
			t.certFallback.Store(true)
		}
	}
	if err != nil {
		return nil, err
	}
	if uc.ConnectionState().NegotiatedProtocol != "h2" {
		raw.Close()
		return nil, errors.New("server did not negotiate h2")
	}
	return uc, nil
}

// dialConn opens the raw byte pipe the h2 session runs on. The carrier is chosen in the settings:
// "tls" (default) is HTTPS; "ws" is a sealed WebSocket over plain HTTP, for networks that block
// HTTPS by default but let ordinary HTTP through. There is no automatic switching between them.
func (t *Transport) dialConn(ctx context.Context) (net.Conn, error) {
	switch t.carrierMode {
	case "ws":
		return t.dialWS(ctx)
	case "poll":
		return t.dialPoll(ctx)
	case "quic":
		return t.dialQUIC(ctx)
	}
	return t.dialTLS(ctx)
}

func (t *Transport) dialQUIC(ctx context.Context) (net.Conn, error) {
	attempt := func(skip bool) (net.Conn, error) {
		return proto.DialQUIC(ctx, t.dialAddr, &tls.Config{ServerName: t.sni, RootCAs: t.caPool, InsecureSkipVerify: skip})
	}
	skip := t.insecureTLS || t.certFallback.Load()
	c, err := attempt(skip)
	if err != nil && !skip && strings.Contains(err.Error(), "certificate") {
		if c, err = attempt(true); err == nil {
			t.certFallback.Store(true)
		}
	}
	return c, err
}

func (t *Transport) dialPoll(ctx context.Context) (net.Conn, error) {
	t.pollOnce.Do(func() {
		t.pollHC = &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return t.dialer.DialContext(ctx, "tcp", t.wsDial)
			},
			MaxIdleConnsPerHost:   8,
			DisableCompression:    true,
			ResponseHeaderTimeout: 45 * time.Second,
			IdleConnTimeout:       60 * time.Second,
		}}
	})
	return proto.PollClient(ctx, t.pollHC, t.sni, t.path+"/p", t.serverPub, chromeUA)
}

func (t *Transport) dialWS(ctx context.Context) (net.Conn, error) {
	raw, err := t.dialer.DialContext(ctx, "tcp", t.wsDial)
	if err != nil {
		return nil, err
	}
	c, err := proto.WSClient(ctx, raw, t.sni, t.path+"/ws", t.serverPub, chromeUA)
	if err != nil {
		raw.Close()
		return nil, err
	}
	return c, nil
}

// Start dials the first carrier, so bad keys / unreachable servers fail loudly at launch.
func (t *Transport) Start(ctx context.Context) error {
	_, err := t.carrier(ctx)
	return err
}

// Alive reports whether a carrier session is currently up.
func (t *Transport) Alive() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cur != nil && !t.cur.sess.Closed()
}

// Close shuts the tunnel down for good.
func (t *Transport) Close() {
	t.mu.Lock()
	t.closed = true
	c := t.cur
	t.mu.Unlock()
	t.lifeCancel()
	if c != nil {
		c.stopOnce.Do(func() { close(c.stop) })
		c.sess.Close()
	}
}

type counted struct {
	proto.Stream
	t *Transport
}

func (c counted) Read(p []byte) (int, error) {
	n, err := c.Stream.Read(p)
	c.t.Down.Add(int64(n))
	return n, err
}
func (c counted) Write(p []byte) (int, error) {
	n, err := c.Stream.Write(p)
	c.t.Up.Add(int64(n))
	return n, err
}

func (t *Transport) carrier(ctx context.Context) (*carrier, error) {
	for {
		t.mu.Lock()
		if t.closed {
			t.mu.Unlock()
			return nil, errors.New("disconnected")
		}
		c := t.cur
		if c != nil && !c.sess.Closed() && time.Now().Before(c.expires) && c.sess.NumStreams() < proto.MaxStreams-8 {
			t.mu.Unlock()
			return c, nil
		}
		if t.dialing == nil {
			// Nobody dialing yet: kick one off. It runs on t.life (the Transport's own lifetime),
			// never on this particular caller's ctx - several callers (e.g. a burst of new TUN
			// connections, or Supervise redialing after a drop) can be waiting on one dial at
			// once, and if the dial ran on whichever caller happened to start it, that caller
			// alone giving up (its own timeout, its own TCP flow closing) would cancel the dial
			// for every other waiter too - tearing down the one shared carrier, and with it every
			// other stream riding on it, which is exactly what made Chrome see repeated
			// ERR_NETWORK_CHANGED: each burst of connections elected an unlucky short-lived
			// dialer, and its cancellation took the whole tunnel down with it.
			t.dialing = make(chan struct{})
			old := t.cur
			w := t.dialing
			go func() {
				dctx, cancel := context.WithTimeout(t.life, 30*time.Second)
				nc, err := t.dialCarrier(dctx)
				cancel()
				if err != nil && proto.IsAccountError(err) {
					// The server itself is refusing this key - not a reachability problem, and
					// not something a redial will ever fix. This is the path that actually fires
					// when the account is disabled/expires/runs out of data mid-session: the
					// server's own kick loop closes the live carrier outright (it doesn't wait
					// for individual streams to be opened and rejected), so it's the *next
					// reconnect attempt* that reports why.
					t.acctOnce.Do(func() {
						if t.onAccountIssue != nil {
							t.onAccountIssue(err.Error())
						}
					})
				}
				t.mu.Lock()
				if err == nil {
					t.cur = nc
					t.dialErr = nil
					if old != nil {
						go drain(old)
					}
				} else {
					t.dialErr = err
				}
				close(w)
				t.dialing = nil
				t.mu.Unlock()
			}()
		}
		w := t.dialing
		t.mu.Unlock()
		select {
		case <-w:
			t.mu.Lock()
			c, err := t.cur, t.dialErr
			t.mu.Unlock()
			if c != nil && !c.sess.Closed() {
				return c, nil
			}
			if err != nil {
				return nil, err
			}
			continue
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// drain retires an old carrier once its last stream ends (or after 30 minutes).
func drain(c *carrier) {
	deadline := time.Now().Add(30 * time.Minute)
	for time.Now().Before(deadline) && !c.sess.Closed() && c.sess.NumStreams() > 0 {
		time.Sleep(3 * time.Second)
	}
	c.stopOnce.Do(func() { close(c.stop) })
	c.sess.Close()
}

func (t *Transport) dialCarrier(ctx context.Context) (*carrier, error) {
	pool := t.newPool()
	hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	t.warm(hctx, pool) // look like a browser landing on the site first; failure here is fine

	tgt := proto.Target{Net: proto.NetMux, Host: "stream"}
	var sid string
	if t.mode == "split" {
		b := make([]byte, 16)
		rand.Read(b)
		sid = hex.EncodeToString(b)
		tgt.Host = "split-" + sid
	}
	hello, err := proto.NewClientHello(t.serverPub, t.psk, tgt)
	if err != nil {
		return nil, err
	}
	s, err := pool.open(hctx, t.postFields(), false)
	if err != nil {
		return nil, err
	}
	if _, err := s.Write(hello.Bytes); err != nil {
		s.Close()
		return nil, err
	}
	if t.mode == "split" {
		s.CloseWrite() // the request body was only the hello; uploads come as separate POSTs
	}
	if err := s.waitHeaders(hctx); err != nil {
		s.Close()
		return nil, err
	}
	if s.status != 200 || s.header.Get("Content-Type") != "application/octet-stream" {
		s.Close()
		return nil, fmt.Errorf("server replied %d (wrong path/key, or not a veil server)", s.status)
	}
	var (
		conn *proto.Conn
		up   *upWriter
	)
	if t.mode == "split" {
		up = newUpWriter(func(seq uint64, data []byte) error { return t.sendChunk(pool, sid, seq, data) })
		conn, err = hello.Finish(&ctxReader{s, hctx}, up, func() error { up.Close(); return s.Close() })
	} else {
		conn, err = hello.Finish(&ctxReader{s, hctx}, s, s.Close)
	}
	if err != nil {
		s.Close()
		if up != nil {
			up.Close()
		}
		return nil, err
	}
	if hello.Limits != nil {
		t.limits.Store(hello.Limits)
	}
	conn.SetReader(s)
	sess := proto.NewSession(conn, true)
	sess.OnReject = func(code byte) {
		if msg, ok := accountIssueMessage(code); ok {
			t.acctOnce.Do(func() {
				if t.onAccountIssue != nil {
					t.onAccountIssue(msg)
				}
			})
		}
	}
	go sess.Run()
	if up != nil {
		up.onFail = sess.Close
	}
	exp := time.Now().Add(24 * 365 * time.Hour)
	if t.rotate > 0 {
		exp = time.Now().Add(time.Duration(float64(t.rotate) * (0.7 + 0.6*mrand.Float64())))
	}
	log.Printf("carrier up (%s), rotates in %s", t.mode, time.Until(exp).Round(time.Second))
	c := &carrier{sess: sess, pool: pool, expires: exp, stop: make(chan struct{})}
	if t.cover {
		sess.KeepAlive(true)
		go t.coverFetches(c)
	}
	return c, nil
}

// warm = what a browser does when it opens the site: GET the landing page.
func (t *Transport) warm(ctx context.Context, pool *h2Pool) {
	s, err := pool.open(ctx, t.getFields(), true)
	if err != nil {
		return
	}
	defer s.Close()
	if s.waitHeaders(ctx) == nil {
		io.Copy(io.Discard, io.LimitReader(s, 1<<20))
	}
}

// coverFetches makes the connection look like an updater client: now and then
// it polls the (decoy) update endpoints on the same HTTP/2 connection.
func (t *Transport) coverFetches(c *carrier) {
	paths := []string{"/releases.json", "/api/v1/update/check?version=4.8.1", "/status", "/", "/releases/4.8.2"}
	for {
		wait := time.Duration(40+mrand.IntN(140)) * time.Second
		select {
		case <-c.stop:
			return
		case <-c.sess.Done():
			return
		case <-time.After(wait):
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		s, err := c.pool.open(ctx, t.getFieldsFor(paths[mrand.IntN(len(paths))]), true)
		if err == nil {
			if s.waitHeaders(ctx) == nil {
				io.Copy(io.Discard, io.LimitReader(s, 1<<20))
			}
			s.Close()
		}
		cancel()
	}
}

// sendChunk uploads one piece of the split uplink as its own small POST.
func (t *Transport) sendChunk(pool *h2Pool, sid string, seq uint64, data []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	body := make([]byte, 8+len(data))
	putU64(body, seq)
	copy(body[8:], data)
	f := t.postFieldsFor(t.path + "/" + sid)
	f = append(f[:len(f)-1:len(f)-1], hf("content-length", strconv.Itoa(len(body))), f[len(f)-1])
	s, err := pool.open(ctx, f, false)
	if err != nil {
		return err
	}
	defer s.Close()
	if _, err := s.Write(body); err != nil {
		return err
	}
	s.CloseWrite()
	if err := s.waitHeaders(ctx); err != nil {
		return err
	}
	if s.status != 200 {
		return fmt.Errorf("upload rejected (%d)", s.status)
	}
	io.Copy(io.Discard, s)
	return nil
}

// Dial opens a logical connection through the current carrier.
func (t *Transport) Dial(ctx context.Context, tgt proto.Target) (proto.Stream, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		c, err := t.carrier(ctx)
		if err != nil {
			return nil, err
		}
		octx, cancel := context.WithTimeout(ctx, 25*time.Second)
		st, err := c.sess.OpenStream(octx, tgt)
		cancel()
		if err == nil {
			return counted{st, t}, nil
		}
		if !c.sess.Closed() {
			return nil, err // dial failure on the far side: report as is
		}
		lastErr = err // carrier died: loop picks/creates another
	}
	return nil, lastErr
}

type ctxReader struct {
	s   *h2Stream
	ctx context.Context
}

func (r *ctxReader) Read(p []byte) (int, error) {
	stop := context.AfterFunc(r.ctx, func() { r.s.fail(r.ctx.Err()) })
	defer stop()
	return r.s.Read(p)
}

// ---------------------------------------------------------------- split uplink writer

type upWriter struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
	seq    uint64
	send   func(uint64, []byte) error
	sem    chan struct{}
	onFail func()
}

func newUpWriter(send func(uint64, []byte) error) *upWriter {
	u := &upWriter{send: send, sem: make(chan struct{}, 6)}
	u.cond = sync.NewCond(&u.mu)
	go u.loop()
	return u
}

func (u *upWriter) Write(p []byte) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for len(u.buf) >= 256<<10 && !u.closed {
		u.cond.Wait()
	}
	if u.closed {
		return 0, io.ErrClosedPipe
	}
	u.buf = append(u.buf, p...)
	u.cond.Broadcast()
	return len(p), nil
}

func (u *upWriter) loop() {
	for {
		u.mu.Lock()
		for len(u.buf) == 0 && !u.closed {
			u.cond.Wait()
		}
		if u.closed {
			u.mu.Unlock()
			return
		}
		n := min(len(u.buf), 48<<10)
		chunk := append([]byte(nil), u.buf[:n]...)
		u.buf = u.buf[n:]
		seq := u.seq
		u.seq++
		u.cond.Broadcast()
		u.mu.Unlock()
		u.sem <- struct{}{}
		go func() {
			defer func() { <-u.sem }()
			err := u.send(seq, chunk)
			if err != nil {
				err = u.send(seq, chunk) // one retry (server drops duplicates)
			}
			if err != nil {
				u.Close()
				if u.onFail != nil {
					u.onFail()
				}
			}
		}()
	}
}

func (u *upWriter) Close() error {
	u.mu.Lock()
	u.closed = true
	u.cond.Broadcast()
	u.mu.Unlock()
	return nil
}

// accountIssueMessage identifies the status codes that mean "this key itself is the problem" -
// every future connection attempt will fail the same way until the account changes server-side -
// as opposed to StatusFailed/StatusNotAllowed/StatusTimeout/StatusBusy, which are about one
// destination or a momentary server condition and say nothing about the key.
func accountIssueMessage(code byte) (string, bool) {
	switch code {
	case proto.StatusDisabled:
		return "This key has been disabled by the server admin.", true
	case proto.StatusExpired:
		return "This key has expired.", true
	case proto.StatusQuota:
		return "This key has used up its data allowance for this period.", true
	}
	return "", false
}

// Supervise keeps the carrier alive for as long as ctx lives: whenever the current carrier's
// session ends (a network blip, a rotation, or the server's kick loop dropping it because the
// account was disabled/expired/ran out of data), it redials in the background instead of waiting
// for the next real connection attempt to notice. This is what makes an account problem show up
// within a second or two even if nothing was actively browsing at the time.
func (t *Transport) Supervise(ctx context.Context) {
	backoff := time.Second
	for {
		t.mu.Lock()
		c, closed := t.cur, t.closed
		t.mu.Unlock()
		if closed || c == nil {
			return
		}
		select {
		case <-c.sess.Done():
		case <-ctx.Done():
			return
		}
		t.mu.Lock()
		closed = t.closed
		t.mu.Unlock()
		if closed {
			return
		}
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		_, err := t.carrier(cctx)
		cancel()
		if err != nil {
			if ctx.Err() != nil || proto.IsAccountError(err) {
				return // either we're shutting down, or onAccountIssue already fired inside carrier()
			}
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			if backoff < 15*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
	}
}
