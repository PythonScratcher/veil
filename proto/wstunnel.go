package proto

// A plain-HTTP carrier: an ordinary WebSocket upgrade over port 80, with every frame payload sealed
// by its own ChaCha20-Poly1305 layer. The multiplexed HTTP/2 session normally run inside TLS runs
// inside this instead, so nothing but the upgrade request itself is readable on the wire. Use it
// where HTTPS is blocked by default but plain HTTP is not. The server is still authenticated by the
// inner Veil handshake; this layer only hides the carrier from passive inspection.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// wsRecord is the largest plaintext carried by one WebSocket frame.
const wsRecord = 16 * 1024

// ErrNotWebSocket means the request is not a WebSocket upgrade (serve the decoy site instead).
var ErrNotWebSocket = errors.New("not a websocket upgrade")

func wsAccept(key string) string {
	h := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

type wsConn struct {
	c      net.Conn
	br     *bufio.Reader
	client bool
	rc     recCodec
	rn, wn uint64
	last   atomic.Int64 // unix nano of the last write (for idle dummy records)
	stop   chan struct{}
	wmu    sync.Mutex
	rbuf   []byte
	rerr   error
}

// recCodec seals and opens the sealed records both plain-HTTP carriers (WebSocket and long-poll)
// use. Plaintext is len(2) || data || random padding, so record sizes do not mirror the traffic, and
// a record with len 0 is an idle "dummy" that keeps the connection busy-looking.
type recCodec struct{ rd, wr cipher.AEAD }

func (rc *recCodec) sealAt(seq uint64, data, pad []byte) []byte {
	pt := make([]byte, 0, 2+len(data)+len(pad))
	pt = binary.BigEndian.AppendUint16(pt, uint16(len(data)))
	pt = append(pt, data...)
	pt = append(pt, pad...)
	return rc.wr.Seal(nil, wsNonce(seq), pt, nil)
}

func (rc *recCodec) openAt(seq uint64, ct []byte) ([]byte, error) {
	pt, err := rc.rd.Open(nil, wsNonce(seq), ct, nil)
	if err != nil || len(pt) < 2 {
		return nil, errors.New("bad record")
	}
	n := int(binary.BigEndian.Uint16(pt))
	if n > len(pt)-2 {
		return nil, errors.New("bad record")
	}
	return pt[2 : 2+n], nil
}

// randPad returns 0-127 random bytes (more for tiny records, so small writes do not stand out).
func randPad(dataLen int) []byte {
	var b [1]byte
	rand.Read(b[:])
	n := int(b[0]) % 128
	if dataLen < 64 {
		n += 64
	}
	p := make([]byte, n)
	rand.Read(p)
	return p
}

func wsNonce(n uint64) []byte {
	var b [12]byte
	binary.BigEndian.PutUint64(b[4:], n)
	return b[:]
}

func wsKeys(shared, ephPub, srvPub []byte) (c2s, s2c cipher.AEAD, err error) {
	salt := append(append([]byte{}, ephPub...), srvPub...)
	k, err := hkdf.Key(sha256.New, shared, salt, "veil-ws-v1", 64)
	if err != nil {
		return nil, nil, err
	}
	if c2s, err = chacha20poly1305.New(k[:32]); err != nil {
		return nil, nil, err
	}
	s2c, err = chacha20poly1305.New(k[32:])
	return
}

// writeFrame sends one FIN frame; the caller holds wmu.
func (w *wsConn) writeFrame(op byte, payload []byte) error {
	n := len(payload)
	buf := make([]byte, 0, n+14)
	buf = append(buf, 0x80|op)
	var mb byte
	if w.client {
		mb = 0x80
	}
	switch {
	case n < 126:
		buf = append(buf, mb|byte(n))
	case n < 65536:
		buf = append(buf, mb|126, byte(n>>8), byte(n))
	default:
		buf = append(buf, mb|127)
		buf = binary.BigEndian.AppendUint64(buf, uint64(n))
	}
	if w.client {
		var key [4]byte
		rand.Read(key[:])
		buf = append(buf, key[:]...)
		start := len(buf)
		buf = append(buf, payload...)
		for i := range payload {
			buf[start+i] ^= key[i%4]
		}
	} else {
		buf = append(buf, payload...)
	}
	_, err := w.c.Write(buf)
	return err
}

func (w *wsConn) readFrame() (op byte, fin bool, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(w.br, h[:]); err != nil {
		return
	}
	fin, op = h[0]&0x80 != 0, h[0]&0x0f
	masked := h[1]&0x80 != 0
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(w.br, b[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(w.br, b[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > 1<<20 {
		err = errors.New("ws: frame too large")
		return
	}
	var mk [4]byte
	if masked {
		if _, err = io.ReadFull(w.br, mk[:]); err != nil {
			return
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(w.br, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mk[i%4]
		}
	}
	return
}

// readMessage returns the next complete data message, answering pings and treating close as EOF.
func (w *wsConn) readMessage() ([]byte, error) {
	var msg []byte
	for {
		op, fin, p, err := w.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case 0x8:
			return nil, io.EOF
		case 0x9:
			w.wmu.Lock()
			w.writeFrame(0xA, p)
			w.wmu.Unlock()
		case 0xA:
		case 0x0, 0x1, 0x2:
			msg = append(msg, p...)
			if len(msg) > 1<<20 {
				return nil, errors.New("ws: message too large")
			}
			if fin {
				return msg, nil
			}
		default:
			return nil, errors.New("ws: unsupported frame")
		}
	}
}

func (w *wsConn) Read(p []byte) (int, error) {
	for len(w.rbuf) == 0 {
		if w.rerr != nil {
			return 0, w.rerr
		}
		msg, err := w.readMessage()
		if err != nil {
			w.rerr = err
			return 0, err
		}
		pt, err := w.rc.openAt(w.rn, msg)
		if err != nil {
			w.rerr = errors.New("ws: bad record")
			return 0, w.rerr
		}
		w.rn++
		w.rbuf = pt // empty for dummy records: loop again
	}
	n := copy(p, w.rbuf)
	w.rbuf = w.rbuf[n:]
	return n, nil
}

func (w *wsConn) Write(p []byte) (int, error) {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	sent := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > wsRecord {
			chunk = chunk[:wsRecord]
		}
		ct := w.rc.sealAt(w.wn, chunk, randPad(len(chunk)))
		w.wn++
		if err := w.writeFrame(0x2, ct); err != nil {
			return sent, err
		}
		sent += len(chunk)
		w.last.Store(time.Now().UnixNano())
		p = p[len(chunk):]
	}
	return sent, nil
}

// idleLoop sends an empty record every 8-30s of silence, so a quiet tunnel still shows some
// activity and idle-timeout proxies do not drop it.
func (w *wsConn) idleLoop() {
	for {
		var b [1]byte
		rand.Read(b[:])
		d := time.Duration(8+int(b[0])%23) * time.Second
		select {
		case <-w.stop:
			return
		case <-time.After(d):
		}
		if time.Since(time.Unix(0, w.last.Load())) < d {
			continue
		}
		w.wmu.Lock()
		ct := w.rc.sealAt(w.wn, nil, randPad(0))
		w.wn++
		err := w.writeFrame(0x2, ct)
		w.wmu.Unlock()
		if err != nil {
			return
		}
	}
}

func (w *wsConn) Close() error {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	w.wmu.Lock()
	w.writeFrame(0x8, []byte{0x03, 0xe8})
	w.wmu.Unlock()
	return w.c.Close()
}
func (w *wsConn) LocalAddr() net.Addr                { return w.c.LocalAddr() }
func (w *wsConn) RemoteAddr() net.Addr               { return w.c.RemoteAddr() }
func (w *wsConn) SetDeadline(t time.Time) error      { return w.c.SetDeadline(t) }
func (w *wsConn) SetReadDeadline(t time.Time) error  { return w.c.SetReadDeadline(t) }
func (w *wsConn) SetWriteDeadline(t time.Time) error { return w.c.SetWriteDeadline(t) }

// WSClient performs the WebSocket upgrade on an already-dialed plain TCP connection and sets up the
// sealed record layer against the server's static public key.
func WSClient(ctx context.Context, c net.Conn, host, path string, serverPub []byte, ua string) (net.Conn, error) {
	spub, err := ecdh.X25519().NewPublicKey(serverPub)
	if err != nil {
		return nil, err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			c.Close()
		case <-done:
		}
	}()
	c.SetDeadline(time.Now().Add(15 * time.Second))
	defer c.SetDeadline(time.Time{})

	var kb [16]byte
	rand.Read(kb[:])
	key := base64.StdEncoding.EncodeToString(kb[:])
	var req bytes.Buffer
	fmt.Fprintf(&req, "GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nOrigin: http://%s\r\nAccept-Encoding: gzip, deflate\r\nAccept-Language: en-US,en;q=0.9\r\nCache-Control: no-cache\r\nPragma: no-cache\r\n\r\n", path, host, ua, key, host)
	if _, err := c.Write(req.Bytes()); err != nil {
		return nil, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 101 || resp.Header.Get("Sec-WebSocket-Accept") != wsAccept(key) {
		return nil, fmt.Errorf("websocket upgrade refused (HTTP %d)", resp.StatusCode)
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
	w := &wsConn{c: c, br: br, client: true, rc: recCodec{rd: s2c, wr: c2s}, stop: make(chan struct{})}
	pad := make([]byte, int(kb[0])%64)
	rand.Read(pad)
	w.wmu.Lock()
	err = w.writeFrame(0x2, append(eph.PublicKey().Bytes(), pad...))
	w.wmu.Unlock()
	if err != nil {
		return nil, err
	}
	go w.idleLoop()
	return w, nil
}

// WSServerUpgrade takes over an HTTP/1.1 connection that asked for a WebSocket upgrade and returns
// a net.Conn carrying the sealed stream. It returns ErrNotWebSocket (nothing written) for any
// request that is not an upgrade, so the caller can serve its decoy site instead.
func WSServerUpgrade(w http.ResponseWriter, r *http.Request, priv *ecdh.PrivateKey) (net.Conn, error) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if r.Method != http.MethodGet || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		!strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") ||
		r.Header.Get("Sec-WebSocket-Version") != "13" || key == "" {
		return nil, ErrNotWebSocket
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, ErrNotWebSocket
	}
	c, brw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	c.SetDeadline(time.Now().Add(10 * time.Second))
	_, err = fmt.Fprintf(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", wsAccept(key))
	if err != nil {
		c.Close()
		return nil, err
	}
	wc := &wsConn{c: c, br: brw.Reader, stop: make(chan struct{})}
	first, err := wc.readMessage()
	if err != nil || len(first) < 32 {
		c.Close()
		return nil, errors.New("ws: bad first message")
	}
	cpub, err := ecdh.X25519().NewPublicKey(first[:32])
	if err != nil {
		c.Close()
		return nil, err
	}
	shared, err := priv.ECDH(cpub)
	if err != nil {
		c.Close()
		return nil, err
	}
	c2s, s2c, err := wsKeys(shared, first[:32], priv.PublicKey().Bytes())
	if err != nil {
		c.Close()
		return nil, err
	}
	wc.rc = recCodec{rd: c2s, wr: s2c}
	c.SetDeadline(time.Time{})
	go wc.idleLoop()
	return wc, nil
}
