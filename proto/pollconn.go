package proto

// The long-poll carrier: the same sealed records as the WebSocket carrier, but moved by plain
// HTTP/1.1 requests and responses only (no Upgrade header), so it passes proxies that strip or block
// WebSockets. Uplink = POSTs numbered by sequence number (several in flight, any order); downlink =
// a long GET that returns the next buffered records. A GET names the next record it still needs,
// so a dropped response is simply requested again.
//
//	POST <base>/n        ephemeral key (+padding)  -> {"id":"<sid>"}
//	POST <base>/<sid>/u  seq(8) || record          -> {}
//	GET  <base>/<sid>/d?n=<next seq>               -> records: len(4) || record, ...

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	pollWait     = 25 * time.Second
	pollMaxBody  = 256 << 10
	pollChunk    = 32 << 10
	pollMaxQueue = 1 << 20
	pollWorkers  = 4
	pollIdleGC   = 90 * time.Second
	pollMaxSess  = 256
)

type pollAddr string

func (a pollAddr) Network() string { return "http" }
func (a pollAddr) String() string  { return string(a) }

// ---------------------------------------------------------------- client

type pollClient struct {
	hc         *http.Client
	base, host string
	ua, sid    string
	rc         recCodec
	ctx        context.Context
	cancel     context.CancelFunc

	mu     sync.Mutex
	cond   *sync.Cond
	rbuf   []byte // decrypted bytes waiting for Read
	err    error
	closed bool

	wbuf    []byte // bytes waiting to be sent
	wseq    uint64 // next uplink sequence number
	pending int    // uplink records handed to workers but not yet acknowledged
}

// PollClient opens a long-poll session. hc must dial the plain-HTTP address.
func PollClient(ctx context.Context, hc *http.Client, host, base string, serverPub []byte, ua string) (net.Conn, error) {
	spub, err := ecdh.X25519().NewPublicKey(serverPub)
	if err != nil {
		return nil, err
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	shared, err := eph.ECDH(spub)
	if err != nil {
		return nil, err
	}
	c2s, s2c, err := wsKeys(shared, eph.PublicKey().Bytes(), serverPub)
	if err != nil {
		return nil, err
	}
	pad := make([]byte, 16+int(eph.PublicKey().Bytes()[0])%48)
	rand.Read(pad)
	c := &pollClient{hc: hc, base: base, host: host, ua: ua, rc: recCodec{rd: s2c, wr: c2s}}
	c.cond = sync.NewCond(&c.mu)
	body, err := c.do(ctx, http.MethodPost, "/n", append(eph.PublicKey().Bytes(), pad...), true)
	if err != nil {
		return nil, err
	}
	i := bytes.Index(body, []byte(`"id":"`))
	if i < 0 || len(body) < i+6+32 {
		return nil, errors.New("poll: bad session reply")
	}
	c.sid = string(body[i+6 : i+6+32])
	c.ctx, c.cancel = context.WithCancel(context.Background())
	go c.readLoop()
	for i := 0; i < pollWorkers; i++ {
		go c.writeLoop()
	}
	return c, nil
}

func (c *pollClient) do(ctx context.Context, method, suffix string, body []byte, bin bool) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+c.host+c.base+suffix, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Cache-Control", "no-cache")
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, pollMaxBody+4096))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("poll: HTTP %d", resp.StatusCode)
	}
	return b, nil
}

func (c *pollClient) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.cond.Broadcast()
	c.mu.Unlock()
	c.cancel()
}

func (c *pollClient) readLoop() {
	var next uint64
	tries := 0
	for {
		b, err := c.do(c.ctx, http.MethodGet, "/"+c.sid+"/d?n="+strconv.FormatUint(next, 10), nil, false)
		if err == nil {
			err = c.deliver(b, &next)
		}
		if err != nil {
			if c.ctx.Err() != nil {
				return
			}
			if tries++; tries > 3 {
				c.fail(err)
				return
			}
			select {
			case <-c.ctx.Done():
				return
			case <-time.After(time.Duration(tries) * 400 * time.Millisecond):
			}
			continue
		}
		tries = 0
	}
}

// deliver parses records out of one response; a cut-off response leaves next untouched past the
// last whole record, so the next GET simply asks for the rest again.
func (c *pollClient) deliver(b []byte, next *uint64) error {
	for len(b) > 0 {
		if len(b) < 4 {
			return errors.New("poll: truncated response")
		}
		n := int(binary.BigEndian.Uint32(b))
		if n > pollMaxBody || len(b) < 4+n {
			return errors.New("poll: truncated response")
		}
		data, err := c.rc.openAt(*next, b[4:4+n])
		if err != nil {
			c.fail(err)
			return nil
		}
		*next++
		b = b[4+n:]
		if len(data) > 0 {
			c.mu.Lock()
			c.rbuf = append(c.rbuf, data...)
			c.cond.Broadcast()
			c.mu.Unlock()
		}
	}
	return nil
}

func (c *pollClient) writeLoop() {
	for {
		c.mu.Lock()
		for len(c.wbuf) == 0 && c.err == nil && !c.closed {
			c.cond.Wait()
		}
		if c.err != nil || c.closed {
			c.mu.Unlock()
			return
		}
		n := len(c.wbuf)
		if n > pollChunk {
			n = pollChunk
		}
		data := append([]byte(nil), c.wbuf[:n]...)
		c.wbuf = c.wbuf[n:]
		seq := c.wseq
		c.wseq++
		c.pending++
		c.cond.Broadcast()
		c.mu.Unlock()

		ct := c.rc.sealAt(seq, data, randPad(len(data)))
		body := binary.BigEndian.AppendUint64(make([]byte, 0, 8+len(ct)), seq)
		body = append(body, ct...)
		var err error
		for try := 0; try < 4; try++ {
			if _, err = c.do(c.ctx, http.MethodPost, "/"+c.sid+"/u", body, true); err == nil || c.ctx.Err() != nil {
				break
			}
			time.Sleep(time.Duration(try+1) * 300 * time.Millisecond)
		}
		c.mu.Lock()
		c.pending--
		c.cond.Broadcast()
		c.mu.Unlock()
		if err != nil && c.ctx.Err() == nil {
			c.fail(err)
			return
		}
	}
}

func (c *pollClient) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.rbuf) == 0 {
		if c.err != nil {
			return 0, c.err
		}
		if c.closed {
			return 0, io.EOF
		}
		c.cond.Wait()
	}
	n := copy(p, c.rbuf)
	c.rbuf = c.rbuf[n:]
	return n, nil
}

func (c *pollClient) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.wbuf) > pollMaxQueue && c.err == nil && !c.closed {
		c.cond.Wait()
	}
	if c.err != nil {
		return 0, c.err
	}
	if c.closed {
		return 0, io.ErrClosedPipe
	}
	c.wbuf = append(c.wbuf, p...)
	c.cond.Broadcast()
	return len(p), nil
}

func (c *pollClient) Close() error {
	c.mu.Lock()
	c.closed = true
	c.cond.Broadcast()
	c.mu.Unlock()
	if c.cancel != nil {
		c.cancel()
	}
	return nil
}
func (c *pollClient) LocalAddr() net.Addr              { return pollAddr("client") }
func (c *pollClient) RemoteAddr() net.Addr             { return pollAddr(c.host) }
func (c *pollClient) SetDeadline(time.Time) error      { return nil }
func (c *pollClient) SetReadDeadline(time.Time) error  { return nil }
func (c *pollClient) SetWriteDeadline(time.Time) error { return nil }

// ---------------------------------------------------------------- server

// PollServer holds the sessions of long-poll carriers and hands each new one to serve.
type PollServer struct {
	priv  *ecdh.PrivateKey
	serve func(net.Conn)
	mu    sync.Mutex
	sess  map[string]*pollSess
}

func NewPollServer(priv *ecdh.PrivateKey, serve func(net.Conn)) *PollServer {
	return &PollServer{priv: priv, serve: serve, sess: map[string]*pollSess{}}
}

type pollSess struct {
	srv    *PollServer
	id     string
	remote string
	rc     recCodec
	sig    chan struct{}

	mu      sync.Mutex
	cond    *sync.Cond
	last    time.Time
	closed  bool
	upNext  uint64            // next uplink seq to hand to Read
	upPend  map[uint64][]byte // uplink records received ahead of upNext (decrypted)
	upBuf   []byte
	dnBase  uint64   // seq of dnRecs[0]
	dnRecs  [][]byte // sealed downlink records not yet acknowledged by the client
	dnBytes int
	wseq    uint64
}

// Handle serves one request whose path (below base) is suffix. It returns false when the request is
// not part of the protocol, so the caller can serve its decoy site.
func (ps *PollServer) Handle(w http.ResponseWriter, r *http.Request, suffix string) bool {
	switch {
	case suffix == "/n" && r.Method == http.MethodPost:
		ps.create(w, r)
		return true
	}
	parts := strings.Split(strings.TrimPrefix(suffix, "/"), "/")
	if len(parts) != 2 {
		return false
	}
	ps.mu.Lock()
	s := ps.sess[parts[0]]
	ps.mu.Unlock()
	if s == nil {
		return false
	}
	switch {
	case parts[1] == "u" && r.Method == http.MethodPost:
		s.upload(w, r)
	case parts[1] == "d" && r.Method == http.MethodGet:
		s.download(w, r)
	default:
		return false
	}
	return true
}

func (ps *PollServer) create(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil || len(b) < 32 {
		http.NotFound(w, r)
		return
	}
	cpub, err := ecdh.X25519().NewPublicKey(b[:32])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	shared, err := ps.priv.ECDH(cpub)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	c2s, s2c, err := wsKeys(shared, b[:32], ps.priv.PublicKey().Bytes())
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ps.mu.Lock()
	if len(ps.sess) >= pollMaxSess {
		ps.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	var idb [16]byte
	rand.Read(idb[:])
	s := &pollSess{srv: ps, id: hex.EncodeToString(idb[:]), remote: r.RemoteAddr, rc: recCodec{rd: c2s, wr: s2c},
		sig: make(chan struct{}, 1), last: time.Now(), upPend: map[uint64][]byte{}}
	s.cond = sync.NewCond(&s.mu)
	ps.sess[s.id] = s
	ps.mu.Unlock()
	go s.gc()
	go ps.serve(s)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, `{"id":"%s"}`, s.id)
}

func (s *pollSess) touch() { s.mu.Lock(); s.last = time.Now(); s.mu.Unlock() }

func (s *pollSess) gc() {
	for {
		time.Sleep(10 * time.Second)
		s.mu.Lock()
		dead := s.closed || time.Since(s.last) > pollIdleGC
		s.mu.Unlock()
		if dead {
			s.Close()
			return
		}
	}
}

func (s *pollSess) upload(w http.ResponseWriter, r *http.Request) {
	s.touch()
	b, err := io.ReadAll(io.LimitReader(r.Body, pollMaxBody))
	if err != nil || len(b) < 9 {
		http.NotFound(w, r)
		return
	}
	seq := binary.BigEndian.Uint64(b)
	data, err := s.rc.openAt(seq, b[8:])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	// backpressure: hold the response while the reader is far behind (the next record always fits)
	for !s.closed && seq != s.upNext && len(s.upPend) > 64 {
		s.cond.Wait()
	}
	if !s.closed && seq >= s.upNext && s.upPend[seq] == nil {
		s.upPend[seq] = append([]byte{}, data...)
		s.cond.Broadcast()
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte("{}"))
}

func (s *pollSess) download(w http.ResponseWriter, r *http.Request) {
	s.touch()
	n, err := strconv.ParseUint(r.URL.Query().Get("n"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	deadline := time.After(pollWait)
	for {
		s.mu.Lock()
		for len(s.dnRecs) > 0 && s.dnBase < n { // acknowledged
			s.dnBytes -= len(s.dnRecs[0])
			s.dnRecs = s.dnRecs[1:]
			s.dnBase++
		}
		s.cond.Broadcast()
		var out []byte
		if n >= s.dnBase {
			for i := int(n - s.dnBase); i < len(s.dnRecs); i++ {
				if len(out) > 0 && len(out)+4+len(s.dnRecs[i]) > pollMaxBody {
					break
				}
				out = binary.BigEndian.AppendUint32(out, uint32(len(s.dnRecs[i])))
				out = append(out, s.dnRecs[i]...)
			}
		}
		closed := s.closed
		s.mu.Unlock()
		if len(out) > 0 || closed {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Cache-Control", "no-store")
			w.Write(out)
			return
		}
		select {
		case <-s.sig:
		case <-deadline:
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Cache-Control", "no-store")
			return
		case <-r.Context().Done():
			return
		}
	}
}

// net.Conn side (what the HTTP/2 server reads and writes)

func (s *pollSess) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.upBuf) == 0 {
		if d, ok := s.upPend[s.upNext]; ok {
			delete(s.upPend, s.upNext)
			s.upNext++
			s.upBuf = d
			s.cond.Broadcast()
			continue
		}
		if s.closed {
			return 0, io.EOF
		}
		s.cond.Wait()
	}
	n := copy(p, s.upBuf)
	s.upBuf = s.upBuf[n:]
	return n, nil
}

func (s *pollSess) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sent := 0
	for len(p) > 0 {
		for !s.closed && s.dnBytes > pollMaxQueue*4 {
			s.cond.Wait()
		}
		if s.closed {
			return sent, io.ErrClosedPipe
		}
		chunk := p
		if len(chunk) > wsRecord {
			chunk = chunk[:wsRecord]
		}
		ct := s.rc.sealAt(s.wseq, chunk, randPad(len(chunk)))
		s.wseq++
		s.dnRecs = append(s.dnRecs, ct)
		s.dnBytes += len(ct)
		select {
		case s.sig <- struct{}{}:
		default:
		}
		sent += len(chunk)
		p = p[len(chunk):]
	}
	return sent, nil
}

func (s *pollSess) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.cond.Broadcast()
	s.mu.Unlock()
	select {
	case s.sig <- struct{}{}:
	default:
	}
	s.srv.mu.Lock()
	delete(s.srv.sess, s.id)
	s.srv.mu.Unlock()
	return nil
}
func (s *pollSess) LocalAddr() net.Addr              { return pollAddr("server") }
func (s *pollSess) RemoteAddr() net.Addr             { return pollAddr(s.remote) }
func (s *pollSess) SetDeadline(time.Time) error      { return nil }
func (s *pollSess) SetReadDeadline(time.Time) error  { return nil }
func (s *pollSess) SetWriteDeadline(time.Time) error { return nil }
