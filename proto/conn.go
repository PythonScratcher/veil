package proto

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"sync"
	"time"
)

const (
	recData   byte = 0
	recPad    byte = 1
	recStatus byte = 2
	recFin    byte = 3

	MaxData = 16000
	maxPad  = 1024
)

// Conn is an authenticated, encrypted, padded record stream. For UDP targets
// each Write is exactly one datagram and each Read returns exactly one.
type Conn struct {
	r io.Reader
	w io.Writer

	rA, wA  cipher.AEAD
	rCtr    uint64
	wCtr    uint64
	wmu     sync.Mutex
	pending []byte
	sent    int
	finSent bool
	closer  func() error
	once    sync.Once
}

func newConn(r io.Reader, w io.Writer, wA, rA cipher.AEAD, closer func() error) *Conn {
	return &Conn{r: r, w: w, wA: wA, rA: rA, closer: closer}
}

type flusher interface{ Flush() }

func flush(w io.Writer) {
	if f, ok := w.(flusher); ok {
		f.Flush()
	}
}

// padLen shapes record sizes: the first records get heavy random padding to
// blur handshake-ish length patterns, later ones get light random padding.
func (c *Conn) padLen(n int) int {
	c.sent++
	switch {
	case c.sent <= 12:
		return rand.IntN(600)
	case n >= 1300:
		return 0
	case n < 128:
		return rand.IntN(160)
	case rand.IntN(4) == 0:
		return rand.IntN(96)
	}
	return 0
}

// sealRecord must be called with wmu held (or before the Conn is shared).
func (c *Conn) sealRecord(typ byte, data []byte) []byte {
	pad := c.padLen(len(data))
	plen := 3 + len(data) + pad
	plain := make([]byte, plen)
	plain[0] = typ
	binary.BigEndian.PutUint16(plain[1:], uint16(len(data)))
	copy(plain[3:], data)
	var lb [2]byte
	binary.BigEndian.PutUint16(lb[:], uint16(plen))
	out := make([]byte, 0, 18+plen+16)
	out = c.wA.Seal(out, nonce(c.wCtr), lb[:], nil)
	c.wCtr++
	out = c.wA.Seal(out, nonce(c.wCtr), plain, nil)
	c.wCtr++
	return out
}

func (c *Conn) writeRecord(typ byte, data []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.finSent {
		return io.ErrClosedPipe
	}
	if typ == recFin {
		c.finSent = true
	}
	if _, err := c.w.Write(c.sealRecord(typ, data)); err != nil {
		return err
	}
	flush(c.w)
	return nil
}

func (c *Conn) readRecord() (byte, []byte, error) {
	var hdr [18]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return 0, nil, err
	}
	lb, err := c.rA.Open(nil, nonce(c.rCtr), hdr[:], nil)
	if err != nil {
		return 0, nil, ErrAuth
	}
	c.rCtr++
	plen := int(binary.BigEndian.Uint16(lb))
	if plen < 3 || plen > 3+MaxData+maxPad+2048 {
		return 0, nil, ErrAuth
	}
	body := make([]byte, plen+16)
	if _, err := io.ReadFull(c.r, body); err != nil {
		return 0, nil, err
	}
	pt, err := c.rA.Open(body[:0], nonce(c.rCtr), body, nil)
	if err != nil {
		return 0, nil, ErrAuth
	}
	c.rCtr++
	dl := int(binary.BigEndian.Uint16(pt[1:3]))
	if 3+dl > len(pt) {
		return 0, nil, ErrAuth
	}
	return pt[0], pt[3 : 3+dl], nil
}

// SetReader swaps the underlying reader (used after the handshake).
func (c *Conn) SetReader(r io.Reader) { c.r = r }

func (c *Conn) Read(p []byte) (int, error) {
	for len(c.pending) == 0 {
		typ, d, err := c.readRecord()
		if err != nil {
			if err == io.ErrUnexpectedEOF {
				err = io.EOF
			}
			return 0, err
		}
		switch typ {
		case recData:
			if len(d) == 0 {
				continue
			}
			c.pending = d
		case recFin:
			return 0, io.EOF
		}
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func (c *Conn) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n := min(len(p), MaxData)
		if err := c.writeRecord(recData, p[:n]); err != nil {
			return total, err
		}
		total += n
		p = p[n:]
	}
	return total, nil
}

// CloseWrite signals end-of-stream to the peer but keeps reading.
func (c *Conn) CloseWrite() error {
	err := c.writeRecord(recFin, nil)
	if errors.Is(err, io.ErrClosedPipe) {
		return nil
	}
	return err
}

func (c *Conn) Close() error {
	var err error
	c.once.Do(func() {
		if c.closer != nil {
			err = c.closer()
		}
	})
	return err
}

// Stream is what Pipe needs from the tunnel side (a *Conn, or a metered wrapper).
type Stream interface {
	io.ReadWriter
	CloseWrite() error
	Close() error
}

// Pipe shuttles bytes between a local connection and a tunnel Conn with
// proper half-close semantics, then closes both.
func Pipe(local net.Conn, rc Stream) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(rc, local)
		rc.CloseWrite()
	}()
	go func() {
		defer wg.Done()
		io.Copy(local, rc)
		if cw, ok := local.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			local.SetReadDeadline(time.Now())
		}
	}()
	wg.Wait()
	local.Close()
	rc.Close()
}
