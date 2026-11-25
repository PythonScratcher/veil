package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"veil/proto"
)

// UserRec is the persisted form of one access key.
type UserRec struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Note       string  `json:"note,omitempty"`
	PSK        string  `json:"psk"`
	Enabled    bool    `json:"enabled"`
	Created    int64   `json:"created"`
	ExpiresAt  int64   `json:"expires_at"`     // unix seconds, 0 = never
	DataCap    int64   `json:"data_cap_bytes"` // total (up+down), 0 = unlimited
	UsedUp     int64   `json:"used_up"`
	UsedDown   int64   `json:"used_down"`
	RateMbps   float64 `json:"rate_mbps"`   // per direction, 0 = unlimited
	MaxDevices int     `json:"max_devices"` // distinct client IPs at once, 0 = unlimited
	ResetDays  int     `json:"reset_days"`  // auto-zero usage every N days, 0 = never
	LastReset  int64   `json:"last_reset"`
	LastSeen   int64   `json:"last_seen"`
}

// Account is a live key: persisted fields plus hot-path atomics, limiters
// and the set of currently open tunnels.
type Account struct {
	mu  sync.Mutex // guards rec's non-hot fields
	rec UserRec
	psk []byte

	enabled    atomic.Bool
	expires    atomic.Int64
	capBytes   atomic.Int64
	maxDevices atomic.Int32
	up, down   atomic.Int64
	lastSeen   atomic.Int64
	speedUp    atomic.Int64
	speedDown  atomic.Int64
	prevUp     int64
	prevDown   int64

	upLim, downLim *rate.Limiter

	cmu      sync.Mutex
	conns    map[uint64]func()
	connIP   map[uint64]string
	ipCount  map[string]int
	nextConn uint64
}

func newAccount(r UserRec) (*Account, error) {
	psk, err := base64.StdEncoding.DecodeString(r.PSK)
	if err != nil || len(psk) < 16 {
		return nil, errors.New("bad psk for " + r.Name)
	}
	a := &Account{rec: r, psk: psk, conns: map[uint64]func(){}, connIP: map[uint64]string{}, ipCount: map[string]int{},
		upLim: rate.NewLimiter(rate.Inf, 1<<16), downLim: rate.NewLimiter(rate.Inf, 1<<16)}
	a.up.Store(r.UsedUp)
	a.down.Store(r.UsedDown)
	a.lastSeen.Store(r.LastSeen)
	a.prevUp, a.prevDown = r.UsedUp, r.UsedDown // baseline for history deltas: counters loaded from disk are not new traffic
	a.apply()
	return a, nil
}

// apply pushes rec's editable fields into the hot-path atomics. Caller holds mu (or owns a).
func (a *Account) apply() {
	a.enabled.Store(a.rec.Enabled)
	a.expires.Store(a.rec.ExpiresAt)
	a.capBytes.Store(a.rec.DataCap)
	a.maxDevices.Store(int32(a.rec.MaxDevices))
	lim, burst := rate.Inf, 1<<16
	if a.rec.RateMbps > 0 {
		bps := a.rec.RateMbps * 125000
		lim = rate.Limit(bps)
		burst = max(1<<16, int(bps/8))
	}
	for _, l := range []*rate.Limiter{a.upLim, a.downLim} {
		l.SetLimit(lim)
		l.SetBurst(burst)
	}
}

func (a *Account) snapshot() UserRec {
	a.mu.Lock()
	r := a.rec
	a.mu.Unlock()
	r.UsedUp, r.UsedDown, r.LastSeen = a.up.Load(), a.down.Load(), a.lastSeen.Load()
	return r
}

func (a *Account) status(now int64) byte {
	switch {
	case !a.enabled.Load():
		return proto.StatusDisabled
	case a.expires.Load() != 0 && now >= a.expires.Load():
		return proto.StatusExpired
	case a.capBytes.Load() != 0 && a.up.Load()+a.down.Load() >= a.capBytes.Load():
		return proto.StatusQuota
	}
	return proto.StatusOK
}

func (a *Account) register(ip string, kill func()) (func(), bool) {
	a.cmu.Lock()
	defer a.cmu.Unlock()
	if m := int(a.maxDevices.Load()); m > 0 && a.ipCount[ip] == 0 && len(a.ipCount) >= m {
		return nil, false
	}
	id := a.nextConn
	a.nextConn++
	a.conns[id], a.connIP[id] = kill, ip
	a.ipCount[ip]++
	return func() {
		a.cmu.Lock()
		defer a.cmu.Unlock()
		delete(a.conns, id)
		delete(a.connIP, id)
		if a.ipCount[ip]--; a.ipCount[ip] <= 0 {
			delete(a.ipCount, ip)
		}
	}, true
}

func (a *Account) killAll() {
	a.cmu.Lock()
	ks := make([]func(), 0, len(a.conns))
	for _, k := range a.conns {
		ks = append(ks, k)
	}
	a.cmu.Unlock()
	for _, k := range ks {
		k()
	}
}

func (a *Account) liveInfo() (n int, ips []string) {
	a.cmu.Lock()
	defer a.cmu.Unlock()
	for ip := range a.ipCount {
		ips = append(ips, ip)
	}
	sort.Strings(ips)
	return len(a.conns), ips
}

// wait blocks until the limiter grants n bytes (chunked to the burst size).
func wait(l *rate.Limiter, n int) {
	if l.Limit() == rate.Inf {
		return
	}
	for n > 0 {
		k := min(n, l.Burst())
		if l.WaitN(context.Background(), k) != nil {
			return
		}
		n -= k
	}
}

var errAccount = errors.New("key no longer valid (expired, disabled or data cap reached)")

// metered wraps the tunnel stream: counts bytes, enforces caps, shapes speed.
// Read = client->server ("up"), Write = server->client ("down").
type metered struct {
	s proto.Stream
	a *Account
}

func (m *metered) Read(p []byte) (int, error) {
	if m.a.status(time.Now().Unix()) != proto.StatusOK {
		return 0, errAccount
	}
	n, err := m.s.Read(p)
	if n > 0 {
		m.a.up.Add(int64(n))
		wait(m.a.upLim, n)
	}
	return n, err
}

func (m *metered) Write(p []byte) (int, error) {
	if m.a.status(time.Now().Unix()) != proto.StatusOK {
		return 0, errAccount
	}
	wait(m.a.downLim, len(p))
	n, err := m.s.Write(p)
	m.a.down.Add(int64(n))
	return n, err
}
func (m *metered) CloseWrite() error { return m.s.CloseWrite() }
func (m *metered) Close() error      { return m.s.Close() }

// ---------------------------------------------------------------- store

type Store struct {
	mu    sync.RWMutex
	path  string
	accts map[string]*Account
	auth  atomic.Pointer[[]proto.User]
	dirty atomic.Bool
	hist  *History
}

func openStore(path string, seed []UserCfg) (*Store, error) {
	st := &Store{path: path, accts: map[string]*Account{}, hist: openHistory(filepath.Join(filepath.Dir(path), "history.json"))}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		var recs []UserRec
		if err := json.Unmarshal(raw, &recs); err != nil {
			return nil, err
		}
		for _, r := range recs {
			a, err := newAccount(r)
			if err != nil {
				return nil, err
			}
			st.accts[r.ID] = a
		}
	case os.IsNotExist(err):
		for _, u := range seed { // first run: import users from server.json
			a, err := newAccount(UserRec{ID: newID(), Name: u.Name, PSK: u.PSK, Enabled: true, Created: time.Now().Unix(), LastReset: time.Now().Unix()})
			if err != nil {
				return nil, err
			}
			st.accts[a.rec.ID] = a
		}
		st.dirty.Store(true)
	default:
		return nil, err
	}
	st.rebuildAuth()
	return st, st.save()
}

func newID() string { b := make([]byte, 5); rand.Read(b); return hex.EncodeToString(b) }

func (st *Store) rebuildAuth() {
	st.mu.RLock()
	us := make([]proto.User, 0, len(st.accts))
	for id, a := range st.accts {
		us = append(us, proto.User{Name: id, PSK: a.psk})
	}
	st.mu.RUnlock()
	st.auth.Store(&us)
}

func (st *Store) get(id string) *Account {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.accts[id]
}

func (st *Store) list() []*Account {
	st.mu.RLock()
	out := make([]*Account, 0, len(st.accts))
	for _, a := range st.accts {
		out = append(out, a)
	}
	st.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i].rec.Created < out[j].rec.Created || (out[i].rec.Created == out[j].rec.Created && out[i].rec.ID < out[j].rec.ID)
	})
	return out
}

func newPSK() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

func (st *Store) create(r UserRec) (*Account, error) {
	r.ID, r.PSK, r.Created, r.LastReset = newID(), newPSK(), time.Now().Unix(), time.Now().Unix()
	a, err := newAccount(r)
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	st.accts[r.ID] = a
	st.mu.Unlock()
	st.rebuildAuth()
	st.dirty.Store(true)
	return a, nil
}

func (st *Store) remove(id string) bool {
	st.mu.Lock()
	a, ok := st.accts[id]
	delete(st.accts, id)
	st.mu.Unlock()
	if ok {
		st.rebuildAuth()
		st.hist.forget(id)
		a.killAll()
		st.dirty.Store(true)
	}
	return ok
}

func (a *Account) regenKey() {
	a.mu.Lock()
	a.rec.PSK = newPSK()
	a.psk, _ = base64.StdEncoding.DecodeString(a.rec.PSK)
	a.mu.Unlock()
}

func (st *Store) save() error {
	accts := st.list()
	recs := make([]UserRec, len(accts))
	for i, a := range accts {
		recs[i] = a.snapshot()
	}
	j, _ := json.MarshalIndent(recs, "", "  ")
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, j, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, st.path)
}

// run is the enforcement loop: speed sampling, auto-reset, kicking invalid keys, periodic flush.
func (st *Store) run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	n := 0
	for {
		select {
		case <-ctx.Done():
			if err := st.save(); err != nil {
				log.Printf("save users: %v", err)
			}
			if err := st.hist.save(); err != nil {
				log.Printf("save history: %v", err)
			}
			return
		case now := <-tick.C:
			n++
			for _, a := range st.list() {
				a.mu.Lock()
				u, d := a.up.Load(), a.down.Load()
				du, dd := u-a.prevUp, d-a.prevDown
				if du < 0 { // counters were reset by an admin
					du = u
				}
				if dd < 0 {
					dd = d
				}
				a.prevUp, a.prevDown = u, d
				a.speedUp.Store(du)
				a.speedDown.Store(dd)
				st.hist.add(a.rec.ID, du, dd, now.Unix())
				if rd := a.rec.ResetDays; rd > 0 && now.Unix()-a.rec.LastReset >= int64(rd)*86400 {
					a.rec.LastReset += (now.Unix() - a.rec.LastReset) / (int64(rd) * 86400) * int64(rd) * 86400
					a.up.Store(0)
					a.down.Store(0)
					a.prevUp, a.prevDown = 0, 0
				}
				a.mu.Unlock()
				if a.status(now.Unix()) != proto.StatusOK {
					a.killAll()
				}
			}
			if n%10 == 0 {
				if err := st.save(); err != nil {
					log.Printf("save users: %v", err)
				}
			}
			if n%300 == 0 {
				if err := st.hist.save(); err != nil {
					log.Printf("save history: %v", err)
				}
			}
		}
	}
}

var _ = filepath.Join
