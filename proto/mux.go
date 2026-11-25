package proto

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Mux: many logical streams over one carrier (an encrypted *Conn). The carrier
// itself can ride one bidirectional HTTP/2 stream or the "split" transport.
//
//	frame = type(1) | stream id(4) | len(2) | payload
const (
	mOpen   byte = 1 // payload: target
	mData   byte = 2
	mFin    byte = 3
	mRst    byte = 4
	mWin    byte = 5 // payload: credit u32
	mOpenOK byte = 6 // payload: status
	mPad    byte = 7 // keepalive / cover traffic, ignored

	muxHdr     = 7
	initWin    = 256 << 10
	maxFrame   = 15000
	MaxStreams = 120
)

var (
	ErrSessionClosed = errors.New("veil: carrier closed")
	ErrStreamReset   = errors.New("veil: stream reset")
)

type Session struct {
	c      io.ReadWriteCloser
	client bool
	OnOpen func(*MuxStream, Target) // server side: called in its own goroutine

	// OnReject fires on the client side whenever the server declines a stream open with a
	// non-OK status, before OpenStream returns its error. Lets the caller notice an account-wide
	// problem (disabled/expired/over quota) instead of treating every rejection as per-destination.
	OnReject func(code byte)

	wmu     sync.Mutex
	mu      sync.Mutex
	streams map[uint32]*MuxStream
	nextID  uint32
	err     error
	done    chan struct{}
	once    sync.Once

	Created time.Time
	Bytes   atomic.Int64
}

func NewSession(c io.ReadWriteCloser, client bool) *Session {
	s := &Session{c: c, client: client, streams: map[uint32]*MuxStream{}, nextID: 1, done: make(chan struct{}), Created: time.Now()}
	if !client {
		s.nextID = 2
	}
	return s
}

func (s *Session) Done() <-chan struct{} { return s.done }
func (s *Session) Err() error            { s.mu.Lock(); defer s.mu.Unlock(); return s.err }
func (s *Session) NumStreams() int       { s.mu.Lock(); defer s.mu.Unlock(); return len(s.streams) }
func (s *Session) Closed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *Session) Close() { s.fail(ErrSessionClosed) }

func (s *Session) fail(err error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.err = err
		ss := make([]*MuxStream, 0, len(s.streams))
		for _, st := range s.streams {
			ss = append(ss, st)
		}
		s.mu.Unlock()
		close(s.done)
		s.c.Close()
		for _, st := range ss {
			st.fail(err)
		}
	})
}

func (s *Session) writeFrame(t byte, id uint32, payload []byte) error {
	buf := make([]byte, muxHdr+len(payload))
	buf[0] = t
	binary.BigEndian.PutUint32(buf[1:], id)
	binary.BigEndian.PutUint16(buf[5:], uint16(len(payload)))
	copy(buf[muxHdr:], payload)
	s.wmu.Lock()
	_, err := s.c.Write(buf)
	s.wmu.Unlock()
	if err != nil {
		s.fail(err)
	}
	return err
}

// Run reads frames until the carrier dies. Call it in a goroutine (client) or inline (server).
func (s *Session) Run() error {
	var hdr [muxHdr]byte
	for {
		if _, err := io.ReadFull(s.c, hdr[:]); err != nil {
			s.fail(err)
			return err
		}
		t, id, n := hdr[0], binary.BigEndian.Uint32(hdr[1:]), int(binary.BigEndian.Uint16(hdr[5:]))
		var p []byte
		if n > 0 {
			p = make([]byte, n)
			if _, err := io.ReadFull(s.c, p); err != nil {
				s.fail(err)
				return err
			}
		}
		s.Bytes.Add(int64(n + muxHdr))
		s.mu.Lock()
		st := s.streams[id]
		s.mu.Unlock()
		switch t {
		case mOpen:
			if s.client || st != nil || id%2 == 0 {
				continue
			}
			tgt, _, err := parseTarget(p)
			if err != nil || s.OnOpen == nil {
				s.writeFrame(mOpenOK, id, []byte{StatusFailed})
				continue
			}
			s.mu.Lock()
			if len(s.streams) >= 4*MaxStreams {
				s.mu.Unlock()
				s.writeFrame(mOpenOK, id, []byte{StatusBusy})
				continue
			}
			ns := newStream(s, id)
			ns.udp = tgt.Net == NetUDP
			s.streams[id] = ns
			s.mu.Unlock()
			go s.OnOpen(ns, tgt)
		case mOpenOK:
			if st != nil && len(p) > 0 {
				st.openStatus = p[0]
				st.openOnce.Do(func() { close(st.opened) })
			}
		case mData:
			if st != nil {
				st.onData(p)
			}
		case mFin:
			if st != nil {
				st.onFin()
			}
		case mRst:
			if st != nil {
				st.fail(ErrStreamReset)
				s.remove(st)
			}
		case mWin:
			if st != nil && len(p) >= 4 {
				st.wmu.Lock()
				st.sendWin += int(binary.BigEndian.Uint32(p))
				st.wcond.Broadcast()
				st.wmu.Unlock()
			}
		}
	}
}

func (s *Session) remove(st *MuxStream) {
	s.mu.Lock()
	delete(s.streams, st.id)
	s.mu.Unlock()
}

// OpenStream asks the peer to connect to t. Returns once the peer reports the dial result.
func (s *Session) OpenStream(ctx context.Context, t Target) (*MuxStream, error) {
	s.mu.Lock()
	if s.err != nil {
		s.mu.Unlock()
		return nil, s.err
	}
	if len(s.streams) >= MaxStreams {
		s.mu.Unlock()
		return nil, errors.New("veil: carrier full")
	}
	id := s.nextID
	s.nextID += 2
	st := newStream(s, id)
	st.udp = t.Net == NetUDP
	st.opened = make(chan struct{})
	s.streams[id] = st
	s.mu.Unlock()
	if err := s.writeFrame(mOpen, id, appendTarget(nil, t)); err != nil {
		return nil, err
	}
	select {
	case <-st.opened:
	case <-ctx.Done():
		st.Close()
		return nil, ctx.Err()
	case <-s.done:
		return nil, s.Err()
	}
	if st.openStatus != StatusOK {
		s.remove(st)
		if s.OnReject != nil {
			s.OnReject(st.openStatus)
		}
		return nil, statusError(st.openStatus)
	}
	return st, nil
}

func statusError(c byte) error {
	switch c {
	case StatusNotAllowed:
		return errors.New("destination not allowed by server")
	case StatusTimeout:
		return errors.New("destination timed out")
	case StatusDisabled:
		return errors.New("this key has been disabled by the server admin")
	case StatusExpired:
		return errors.New("this key has expired")
	case StatusQuota:
		return errors.New("data cap reached for this key")
	case StatusBusy:
		return errors.New("server busy")
	}
	return errors.New("destination connect failed")
}

// KeepAlive sends small random pad frames so an idle carrier is never silent.
// With cover=true it also emits occasional larger bursts to blur idle/active patterns.
func (s *Session) KeepAlive(cover bool) {
	go func() {
		for {
			wait := 12*time.Second + time.Duration(rand.Int64N(int64(14*time.Second)))
			if cover {
				wait = 2*time.Second + time.Duration(rand.Int64N(int64(18*time.Second)))
			}
			select {
			case <-s.done:
				return
			case <-time.After(wait):
			}
			n := 24 + rand.IntN(120)
			if cover && rand.IntN(4) == 0 {
				n = 400 + rand.IntN(2400)
			}
			pad := make([]byte, n)
			s.writeFrame(mPad, 0, pad[:min(n, maxFrame)])
		}
	}()
}

// ---------------------------------------------------------------- stream

type MuxStream struct {
	s  *Session
	id uint32

	rmu    sync.Mutex
	rcond  *sync.Cond
	chunks [][]byte
	rsize  int
	rfin   bool
	rerr   error
	udp    bool
	cons   int

	wmu     sync.Mutex
	wcond   *sync.Cond
	sendWin int
	wfin    bool

	opened     chan struct{}
	openOnce   sync.Once
	openStatus byte
	closeOnce  sync.Once
}

func newStream(s *Session, id uint32) *MuxStream {
	st := &MuxStream{s: s, id: id, sendWin: initWin, opened: make(chan struct{})}
	st.rcond = sync.NewCond(&st.rmu)
	st.wcond = sync.NewCond(&st.wmu)
	return st
}

// Accept (server side) reports the dial result to the peer.
func (st *MuxStream) Accept(status byte) error {
	err := st.s.writeFrame(mOpenOK, st.id, []byte{status})
	if status != StatusOK {
		st.s.remove(st)
		st.fail(ErrStreamReset)
	}
	return err
}

func (st *MuxStream) onData(p []byte) {
	st.rmu.Lock()
	if st.rsize+len(p) > 2*initWin { // peer ignored flow control
		st.rmu.Unlock()
		st.reset()
		return
	}
	st.chunks = append(st.chunks, p)
	st.rsize += len(p)
	st.rcond.Broadcast()
	st.rmu.Unlock()
}

func (st *MuxStream) onFin() {
	st.rmu.Lock()
	st.rfin = true
	st.rcond.Broadcast()
	st.rmu.Unlock()
	st.maybeFree()
}

func (st *MuxStream) maybeFree() {
	st.rmu.Lock()
	r := st.rfin
	st.rmu.Unlock()
	st.wmu.Lock()
	w := st.wfin
	st.wmu.Unlock()
	if r && w {
		st.s.remove(st)
	}
}

func (st *MuxStream) fail(err error) {
	st.rmu.Lock()
	if st.rerr == nil {
		st.rerr = err
	}
	st.rcond.Broadcast()
	st.rmu.Unlock()
	st.wmu.Lock()
	st.wcond.Broadcast()
	st.wmu.Unlock()
	st.openOnce.Do(func() { close(st.opened) })
}

func (st *MuxStream) Read(p []byte) (int, error) {
	st.rmu.Lock()
	for len(st.chunks) == 0 && !st.rfin && st.rerr == nil {
		st.rcond.Wait()
	}
	if len(st.chunks) > 0 {
		var n int
		if st.udp { // datagram semantics: one chunk per read
			n = copy(p, st.chunks[0])
			st.rsize -= len(st.chunks[0])
			st.chunks = st.chunks[1:]
		} else {
			n = copy(p, st.chunks[0])
			if n == len(st.chunks[0]) {
				st.chunks = st.chunks[1:]
			} else {
				st.chunks[0] = st.chunks[0][n:]
			}
			st.rsize -= n
		}
		st.cons += n
		credit := 0
		if st.cons >= initWin/4 && !st.rfin {
			credit, st.cons = st.cons, 0
		}
		st.rmu.Unlock()
		if credit > 0 {
			var b [4]byte
			binary.BigEndian.PutUint32(b[:], uint32(credit))
			st.s.writeFrame(mWin, st.id, b[:])
		}
		return n, nil
	}
	defer st.rmu.Unlock()
	if st.rfin {
		return 0, io.EOF
	}
	return 0, st.rerr
}

func (st *MuxStream) Write(p []byte) (int, error) {
	if st.udp && len(p) > maxFrame { // datagrams are never split; oversize ones are dropped like on a real network
		return len(p), nil
	}
	total := 0
	for len(p) > 0 {
		st.wmu.Lock()
		for st.sendWin == 0 && !st.wfin && st.s.Err() == nil && st.errNow() == nil {
			st.wcond.Wait()
		}
		if st.wfin {
			st.wmu.Unlock()
			return total, io.ErrClosedPipe
		}
		if err := st.errNow(); err != nil {
			st.wmu.Unlock()
			return total, err
		}
		if err := st.s.Err(); err != nil {
			st.wmu.Unlock()
			return total, err
		}
		n := min(len(p), st.sendWin, maxFrame)
		st.sendWin -= n
		st.wmu.Unlock()
		if err := st.s.writeFrame(mData, st.id, p[:n]); err != nil {
			return total, err
		}
		total += n
		p = p[n:]
	}
	return total, nil
}

func (st *MuxStream) errNow() error {
	st.rmu.Lock()
	defer st.rmu.Unlock()
	return st.rerr
}

func (st *MuxStream) CloseWrite() error {
	st.wmu.Lock()
	if st.wfin {
		st.wmu.Unlock()
		return nil
	}
	st.wfin = true
	st.wcond.Broadcast()
	st.wmu.Unlock()
	err := st.s.writeFrame(mFin, st.id, nil)
	st.maybeFree()
	return err
}

func (st *MuxStream) reset() {
	st.closeOnce.Do(func() {
		st.s.writeFrame(mRst, st.id, []byte{0})
		st.s.remove(st)
		st.fail(ErrStreamReset)
	})
}

// Close ends the stream; unfinished streams are reset.
func (st *MuxStream) Close() error {
	st.rmu.Lock()
	rfin, rerr := st.rfin, st.rerr
	st.rmu.Unlock()
	st.wmu.Lock()
	wfin := st.wfin
	st.wmu.Unlock()
	if rerr == nil && !(rfin && wfin) {
		st.reset()
	} else {
		st.s.remove(st)
	}
	return nil
}

var _ Stream = (*MuxStream)(nil)

// ---------------------------------------------------------------- target encoding

func appendTarget(b []byte, t Target) []byte {
	b = append(b, t.Net)
	if ip := net.ParseIP(t.Host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			b = append(b, 1)
			b = append(b, v4...)
		} else {
			b = append(b, 4)
			b = append(b, ip.To16()...)
		}
	} else {
		b = append(b, 3, byte(len(t.Host)))
		b = append(b, t.Host...)
	}
	return binary.BigEndian.AppendUint16(b, t.Port)
}

func parseTarget(p []byte) (Target, int, error) {
	bad := errors.New("bad target")
	if len(p) < 2 {
		return Target{}, 0, bad
	}
	t := Target{Net: p[0]}
	if t.Net != NetTCP && t.Net != NetUDP {
		return t, 0, bad
	}
	i := 2
	switch p[1] {
	case 1:
		if len(p) < i+4+2 {
			return t, 0, bad
		}
		t.Host = net.IP(p[i : i+4]).String()
		i += 4
	case 4:
		if len(p) < i+16+2 {
			return t, 0, bad
		}
		t.Host = net.IP(p[i : i+16]).String()
		i += 16
	case 3:
		if len(p) < 3 || len(p) < 3+int(p[2])+2 {
			return t, 0, bad
		}
		l := int(p[2])
		t.Host = string(p[3 : 3+l])
		i = 3 + l
	default:
		return t, 0, bad
	}
	t.Port = binary.BigEndian.Uint16(p[i : i+2])
	return t, i + 2, nil
}
