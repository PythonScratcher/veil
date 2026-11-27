package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"veil/proto"
)

// ---------------------------------------------------------------- split uplink

// reasm turns many independent POST bodies (each tagged with a sequence number)
// back into the single ordered byte stream the secure session reads from.
// Upload requests may arrive out of order and in parallel over HTTP/2.
type reasm struct {
	mu      sync.Mutex
	cond    *sync.Cond
	next    uint64
	pending map[uint64][]byte
	pbytes  int
	cur     []byte
	closed  bool
}

const maxPending = 8 << 20

func newReasm() *reasm {
	r := &reasm{pending: map[uint64][]byte{}}
	r.cond = sync.NewCond(&r.mu)
	return r
}

func (r *reasm) put(seq uint64, b []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Backpressure: hold the HTTP response until there is room (the next-in-line chunk always fits).
	for !r.closed && seq != r.next && r.pbytes+len(b) > maxPending {
		r.cond.Wait()
	}
	if r.closed {
		return io.ErrClosedPipe
	}
	if seq < r.next || r.pending[seq] != nil {
		return nil // duplicate (client retry)
	}
	r.pending[seq] = b
	r.pbytes += len(b)
	r.cond.Broadcast()
	return nil
}

func (r *reasm) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(r.cur) == 0 {
		if b, ok := r.pending[r.next]; ok {
			delete(r.pending, r.next)
			r.pbytes -= len(b)
			r.next++
			r.cur = b
			r.cond.Broadcast()
			continue
		}
		if r.closed {
			return 0, io.EOF
		}
		r.cond.Wait()
	}
	n := copy(p, r.cur)
	r.cur = r.cur[n:]
	return n, nil
}

func (r *reasm) Close() {
	r.mu.Lock()
	r.closed = true
	r.cond.Broadcast()
	r.mu.Unlock()
}

type splitReg struct {
	mu sync.Mutex
	m  map[string]*splitSess
}

type splitSess struct {
	r    *reasm
	user string
	seen time.Time
}

func (g *splitReg) add(id string, s *splitSess) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		g.m = map[string]*splitSess{}
	}
	if _, dup := g.m[id]; dup {
		return false
	}
	g.m[id] = s
	return true
}
func (g *splitReg) get(id string) *splitSess { g.mu.Lock(); defer g.mu.Unlock(); return g.m[id] }
func (g *splitReg) del(id string)            { g.mu.Lock(); delete(g.m, id); g.mu.Unlock() }

// splitUpload handles POST <path>/<sid>: body = seq(8) || bytes.
func (s *server) splitUpload(w http.ResponseWriter, r *http.Request) {
	sid := strings.TrimPrefix(r.URL.Path, s.cfg.Path+"/")
	ss := s.splits.get(sid)
	if ss == nil {
		s.decoy.ServeHTTP(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	b, err := io.ReadAll(r.Body)
	if err != nil || len(b) < 8 {
		s.decoy.ServeHTTP(w, r)
		return
	}
	if err := ss.r.put(binary.BigEndian.Uint64(b), b[8:]); err != nil {
		s.decoy.ServeHTTP(w, r)
		return
	}
	ss.seen = time.Now()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte("{}"))
}

// ---------------------------------------------------------------- carrier session

// serveCarrier runs a multiplexed session for an authenticated client. Every
// logical connection the client opens becomes a stream here.
func (s *server) serveCarrier(a *Account, acc *proto.Accepted, w io.Writer, r *http.Request, ctx context.Context, cancel context.CancelFunc) {
	var up *reasm
	var sid string
	if h := acc.Target.Host; strings.HasPrefix(h, "split-") {
		sid = strings.TrimPrefix(h, "split-")
		if _, err := hex.DecodeString(sid); err != nil || len(sid) != 32 {
			return
		}
		up = newReasm()
		if !s.splits.add(sid, &splitSess{r: up, user: a.rec.Name, seen: time.Now()}) {
			return
		}
		defer s.splits.del(sid)
		defer up.Close()
		acc.SetReader(up)
	}
	closer := r.Body.Close
	if up != nil {
		closer = func() error { up.Close(); return nil }
	}
	rec := a.snapshot()
	limits, _ := json.Marshal(proto.AccountLimits{
		ExpiresAt: rec.ExpiresAt,
		DataCap:   rec.DataCap,
		UsedUp:    rec.UsedUp,
		UsedDown:  rec.UsedDown,
		RateMbps:  rec.RateMbps,
	})
	conn, err := acc.CompleteWithLimits(w, proto.StatusOK, closer, limits)
	if err != nil {
		return
	}
	sess := proto.NewSession(conn, false)
	sess.OnOpen = func(st *proto.MuxStream, t proto.Target) { s.muxStream(ctx, a, st, t) }
	sess.KeepAlive(true)
	go func() { <-ctx.Done(); sess.Close() }()
	go func() { <-sess.Done(); cancel() }()
	sess.Run()
	conn.Close()
}

func (s *server) muxStream(ctx context.Context, a *Account, st *proto.MuxStream, t proto.Target) {
	if code := a.status(time.Now().Unix()); code != proto.StatusOK {
		st.Accept(code)
		return
	}
	if t.Host == "veil.account" { // reserved: report this key's current limits and usage
		rec := a.snapshot()
		b, _ := json.Marshal(proto.AccountLimits{ExpiresAt: rec.ExpiresAt, DataCap: rec.DataCap,
			UsedUp: rec.UsedUp, UsedDown: rec.UsedDown, RateMbps: rec.RateMbps})
		if st.Accept(proto.StatusOK) == nil {
			st.Write(b)
		}
		st.Close()
		return
	}
	dst := t.Addr()
	if dialHook != nil {
		dst = dialHook(dst)
	}
	tc, err := s.dialer.DialContext(ctx, t.NetName(), dst)
	if err != nil {
		code := byte(proto.StatusFailed)
		var ne net.Error
		switch {
		case errors.Is(err, errDenied):
			code = proto.StatusNotAllowed
		case errors.As(err, &ne) && ne.Timeout():
			code = proto.StatusTimeout
		}
		log.Printf("[%s] %s %s: %v", a.rec.Name, t.NetName(), t.Addr(), err)
		st.Accept(code)
		return
	}
	if st.Accept(proto.StatusOK) != nil {
		tc.Close()
		return
	}
	go func() { <-ctx.Done(); tc.Close() }()
	m := &metered{s: st, a: a}
	if t.Net == proto.NetUDP {
		relayUDP(m, tc)
	} else {
		proto.Pipe(tc, m)
	}
	st.Close()
}
