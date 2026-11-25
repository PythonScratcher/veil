package main

// A purpose-built HTTP/2 client whose connection preface looks like Chrome's:
// SETTINGS {HEADER_TABLE_SIZE=65536, ENABLE_PUSH=0, INITIAL_WINDOW_SIZE=6291456,
// MAX_HEADER_LIST_SIZE=262144}, connection WINDOW_UPDATE of 15663105, HEADERS
// carrying an exclusive priority (dep 0, weight 256), pseudo-headers in
// :method :authority :scheme :path order, and Chrome's header order.
// Go's own http2.Transport has a very different (and fingerprintable) preface.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

var (
	errConnClosed = errors.New("h2: connection closed")
	errConnFull   = errors.New("h2: too many streams")
)

const (
	chromeInitWin   = 6291456
	chromeConnBoost = 15663105
	ackThreshold    = 32 * 1024
)

type h2Pool struct {
	dial  func(ctx context.Context) (net.Conn, error)
	mu    sync.Mutex
	conns []*h2Conn
}

func (p *h2Pool) open(ctx context.Context, fields []hpack.HeaderField, endStream bool) (*h2Stream, error) {
	for attempt := 0; attempt < 3; attempt++ {
		c, err := p.get(ctx)
		if err != nil {
			return nil, err
		}
		s, err := c.open(fields, endStream)
		if err == errConnClosed || err == errConnFull {
			continue
		}
		return s, err
	}
	return nil, errConnClosed
}

func (p *h2Pool) get(ctx context.Context) (*h2Conn, error) {
	p.mu.Lock()
	live := p.conns[:0]
	var pick *h2Conn
	for _, c := range p.conns {
		if c.dead() {
			continue
		}
		live = append(live, c)
		if pick == nil && c.canTake() {
			pick = c
		}
	}
	p.conns = live
	p.mu.Unlock()
	if pick != nil {
		return pick, nil
	}
	nc, err := p.dial(ctx)
	if err != nil {
		return nil, err
	}
	c, err := newH2Conn(nc)
	if err != nil {
		nc.Close()
		return nil, err
	}
	p.mu.Lock()
	p.conns = append(p.conns, c)
	p.mu.Unlock()
	return c, nil
}

type h2Conn struct {
	nc   net.Conn
	fr   *http2.Framer
	wmu  sync.Mutex
	henc *hpack.Encoder
	hbuf bytes.Buffer

	mu           sync.Mutex
	cond         *sync.Cond
	streams      map[uint32]*h2Stream
	nextID       uint32
	connWin      int64
	peerInitWin  int64
	maxFrame     int
	maxConc      uint32
	closed       bool
	goaway       bool
	err          error
	connConsumed int

	lastRead atomic.Int64
}

func newH2Conn(nc net.Conn) (*h2Conn, error) {
	c := &h2Conn{nc: nc, streams: map[uint32]*h2Stream{}, nextID: 1, connWin: 65535, peerInitWin: 65535, maxFrame: 16384, maxConc: 100}
	c.cond = sync.NewCond(&c.mu)
	c.fr = http2.NewFramer(nc, nc)
	c.fr.ReadMetaHeaders = hpack.NewDecoder(65536, nil)
	c.fr.MaxHeaderListSize = 262144
	c.henc = hpack.NewEncoder(&c.hbuf)
	c.lastRead.Store(time.Now().UnixNano())
	if _, err := io.WriteString(nc, http2.ClientPreface); err != nil {
		return nil, err
	}
	if err := c.fr.WriteSettings(
		http2.Setting{ID: http2.SettingHeaderTableSize, Val: 65536},
		http2.Setting{ID: http2.SettingEnablePush, Val: 0},
		http2.Setting{ID: http2.SettingInitialWindowSize, Val: chromeInitWin},
		http2.Setting{ID: http2.SettingMaxHeaderListSize, Val: 262144},
	); err != nil {
		return nil, err
	}
	if err := c.fr.WriteWindowUpdate(0, chromeConnBoost); err != nil {
		return nil, err
	}
	go c.readLoop()
	go c.keepalive()
	return c, nil
}

func (c *h2Conn) dead() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.closed || c.goaway }
func (c *h2Conn) canTake() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed && !c.goaway && uint32(len(c.streams)) < c.maxConc && c.nextID < 1<<30
}

func (c *h2Conn) fail(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed, c.err = true, err
	ss := make([]*h2Stream, 0, len(c.streams))
	for _, s := range c.streams {
		ss = append(ss, s)
	}
	c.cond.Broadcast()
	c.mu.Unlock()
	for _, s := range ss {
		s.fail(err)
	}
	c.nc.Close()
}

func (c *h2Conn) keepalive() {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for range t.C {
		if c.dead() {
			return
		}
		idle := time.Since(time.Unix(0, c.lastRead.Load()))
		if idle > 75*time.Second {
			c.fail(errors.New("h2: ping timeout"))
			return
		}
		if idle > 30*time.Second {
			c.wmu.Lock()
			c.fr.WritePing(false, [8]byte{1, 2, 3, 4, 5, 6, 7, 8})
			c.wmu.Unlock()
		}
	}
}

func (c *h2Conn) stream(id uint32) *h2Stream {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.streams[id]
}

func (c *h2Conn) readLoop() {
	for {
		f, err := c.fr.ReadFrame()
		if err != nil {
			if me, ok := err.(http2.StreamError); ok {
				if s := c.stream(me.StreamID); s != nil {
					s.fail(me)
				}
				continue
			}
			c.fail(err)
			return
		}
		c.lastRead.Store(time.Now().UnixNano())
		switch f := f.(type) {
		case *http2.MetaHeadersFrame:
			if s := c.stream(f.StreamID); s != nil {
				s.onHeaders(f)
			}
		case *http2.DataFrame:
			d := f.Data()
			s := c.stream(f.StreamID)
			if s == nil {
				c.ackConn(len(d))
				continue
			}
			s.onData(append([]byte(nil), d...), f.StreamEnded())
		case *http2.WindowUpdateFrame:
			c.mu.Lock()
			if f.StreamID == 0 {
				c.connWin += int64(f.Increment)
			} else if s := c.streams[f.StreamID]; s != nil {
				s.sendWin += int64(f.Increment)
			}
			c.cond.Broadcast()
			c.mu.Unlock()
		case *http2.SettingsFrame:
			if f.IsAck() {
				continue
			}
			c.mu.Lock()
			f.ForeachSetting(func(s http2.Setting) error {
				switch s.ID {
				case http2.SettingInitialWindowSize:
					d := int64(s.Val) - c.peerInitWin
					c.peerInitWin = int64(s.Val)
					for _, st := range c.streams {
						st.sendWin += d
					}
				case http2.SettingMaxFrameSize:
					c.maxFrame = int(s.Val)
				case http2.SettingMaxConcurrentStreams:
					c.maxConc = s.Val
				}
				return nil
			})
			c.cond.Broadcast()
			c.mu.Unlock()
			c.wmu.Lock()
			c.fr.WriteSettingsAck()
			c.wmu.Unlock()
		case *http2.PingFrame:
			if !f.IsAck() {
				c.wmu.Lock()
				c.fr.WritePing(true, f.Data)
				c.wmu.Unlock()
			}
		case *http2.GoAwayFrame:
			c.mu.Lock()
			c.goaway = true
			var doomed []*h2Stream
			for id, s := range c.streams {
				if id > f.LastStreamID {
					doomed = append(doomed, s)
				}
			}
			c.mu.Unlock()
			for _, s := range doomed {
				s.fail(errConnClosed)
			}
		case *http2.RSTStreamFrame:
			if s := c.stream(f.StreamID); s != nil {
				s.fail(errors.New("h2: stream reset: " + f.ErrCode.String()))
			}
		}
	}
}

// ackConn returns connection-level receive window credit.
func (c *h2Conn) ackConn(n int) {
	c.mu.Lock()
	c.connConsumed += n
	send := 0
	if c.connConsumed >= ackThreshold {
		send, c.connConsumed = c.connConsumed, 0
	}
	c.mu.Unlock()
	if send > 0 {
		c.wmu.Lock()
		c.fr.WriteWindowUpdate(0, uint32(send))
		c.wmu.Unlock()
	}
}

func (c *h2Conn) open(fields []hpack.HeaderField, endStream bool) (*h2Stream, error) {
	c.wmu.Lock() // held across id allocation + write so stream ids hit the wire in order
	defer c.wmu.Unlock()
	c.mu.Lock()
	if c.closed || c.goaway || c.nextID >= 1<<30 {
		c.mu.Unlock()
		return nil, errConnClosed
	}
	if uint32(len(c.streams)) >= c.maxConc {
		c.mu.Unlock()
		return nil, errConnFull
	}
	id := c.nextID
	c.nextID += 2
	s := &h2Stream{c: c, id: id, sendWin: c.peerInitWin, hdrCh: make(chan struct{})}
	s.rcond = sync.NewCond(&s.rmu)
	c.streams[id] = s
	c.mu.Unlock()

	c.hbuf.Reset()
	for _, f := range fields {
		c.henc.WriteField(f)
	}
	err := c.fr.WriteHeaders(http2.HeadersFrameParam{
		StreamID: id, BlockFragment: c.hbuf.Bytes(), EndStream: endStream, EndHeaders: true,
		Priority: http2.PriorityParam{StreamDep: 0, Exclusive: true, Weight: 255},
	})
	if err != nil {
		c.fail(err)
		return nil, err
	}
	if endStream {
		s.wclosed = true
	}
	return s, nil
}

// ---------------------------------------------------------------- stream

type h2Stream struct {
	c       *h2Conn
	id      uint32
	sendWin int64 // guarded by c.mu

	status  int
	header  http.Header
	hdrCh   chan struct{}
	hdrOnce sync.Once

	rmu      sync.Mutex
	rcond    *sync.Cond
	chunks   [][]byte
	ended    bool
	err      error
	consumed int

	wclosed bool
	cmu     sync.Mutex // guards wclosed + closed
	closed  bool
}

func (s *h2Stream) onHeaders(f *http2.MetaHeadersFrame) {
	if st := f.PseudoValue("status"); st != "" {
		code, _ := strconv.Atoi(st)
		if code >= 200 {
			select {
			case <-s.hdrCh:
			default:
				s.status = code
				s.header = http.Header{}
				for _, fl := range f.RegularFields() {
					s.header.Add(fl.Name, fl.Value)
				}
				s.hdrOnce.Do(func() { close(s.hdrCh) })
			}
		}
	}
	if f.StreamEnded() {
		s.rmu.Lock()
		s.ended = true
		s.rcond.Broadcast()
		s.rmu.Unlock()
		s.hdrOnce.Do(func() { close(s.hdrCh) })
	}
}

func (s *h2Stream) onData(d []byte, end bool) {
	s.rmu.Lock()
	if len(d) > 0 {
		s.chunks = append(s.chunks, d)
	}
	if end {
		s.ended = true
	}
	s.rcond.Broadcast()
	s.rmu.Unlock()
}

func (s *h2Stream) fail(err error) {
	s.rmu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.rcond.Broadcast()
	s.rmu.Unlock()
	s.hdrOnce.Do(func() { close(s.hdrCh) })
	s.c.mu.Lock()
	s.c.cond.Broadcast()
	s.c.mu.Unlock()
}

func (s *h2Stream) waitHeaders(ctx context.Context) error {
	select {
	case <-s.hdrCh:
	case <-ctx.Done():
		return ctx.Err()
	}
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if s.status == 0 {
		if s.err != nil {
			return s.err
		}
		return io.ErrUnexpectedEOF
	}
	return nil
}

func (s *h2Stream) Read(p []byte) (int, error) {
	s.rmu.Lock()
	for len(s.chunks) == 0 && !s.ended && s.err == nil {
		s.rcond.Wait()
	}
	if len(s.chunks) > 0 {
		n := copy(p, s.chunks[0])
		if n == len(s.chunks[0]) {
			s.chunks = s.chunks[1:]
		} else {
			s.chunks[0] = s.chunks[0][n:]
		}
		s.consumed += n
		send := 0
		if s.consumed >= ackThreshold && !s.ended {
			send, s.consumed = s.consumed, 0
		}
		s.rmu.Unlock()
		s.c.ackConn(n)
		if send > 0 {
			s.c.wmu.Lock()
			s.c.fr.WriteWindowUpdate(s.id, uint32(send))
			s.c.wmu.Unlock()
		}
		return n, nil
	}
	defer s.rmu.Unlock()
	if s.ended {
		return 0, io.EOF
	}
	return 0, s.err
}

func (s *h2Stream) Write(p []byte) (int, error) {
	c := s.c
	total := 0
	for len(p) > 0 {
		c.mu.Lock()
		var n int
		for {
			s.rmu.Lock()
			serr := s.err
			s.rmu.Unlock()
			if c.closed {
				e := c.err
				c.mu.Unlock()
				return total, e
			}
			if serr != nil {
				c.mu.Unlock()
				return total, serr
			}
			avail := min(c.connWin, s.sendWin)
			if avail > 0 {
				n = int(min(int64(len(p)), avail, int64(c.maxFrame)))
				c.connWin -= int64(n)
				s.sendWin -= int64(n)
				break
			}
			c.cond.Wait()
		}
		c.mu.Unlock()
		c.wmu.Lock()
		err := c.fr.WriteData(s.id, false, p[:n])
		c.wmu.Unlock()
		if err != nil {
			c.fail(err)
			return total, err
		}
		total += n
		p = p[n:]
	}
	return total, nil
}

// CloseWrite half-closes the request body.
func (s *h2Stream) CloseWrite() error {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	if s.wclosed || s.closed {
		return nil
	}
	s.wclosed = true
	s.c.wmu.Lock()
	defer s.c.wmu.Unlock()
	return s.c.fr.WriteData(s.id, true, nil)
}

// Close tears the stream down (RST_STREAM CANCEL unless both sides finished).
func (s *h2Stream) Close() error {
	s.cmu.Lock()
	if s.closed {
		s.cmu.Unlock()
		return nil
	}
	s.closed = true
	wc := s.wclosed
	s.cmu.Unlock()
	s.rmu.Lock()
	rend := s.ended
	s.rmu.Unlock()
	if !(wc && rend) {
		s.c.wmu.Lock()
		s.c.fr.WriteRSTStream(s.id, http2.ErrCodeCancel)
		s.c.wmu.Unlock()
	}
	s.c.mu.Lock()
	delete(s.c.streams, s.id)
	s.c.mu.Unlock()
	s.fail(io.ErrClosedPipe)
	return nil
}
