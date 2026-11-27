package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"
	"veil/proto"
)

// logRing keeps the last lines of log output for the GUI.
type logRing struct {
	mu    sync.Mutex
	lines []string
}

func (r *logRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		r.lines = append(r.lines, l)
	}
	if len(r.lines) > 300 {
		r.lines = r.lines[len(r.lines)-300:]
	}
	return len(p), nil
}

func (r *logRing) Snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

type Engine struct {
	mu          sync.Mutex
	state       string // disconnected | connecting | connected | error
	errMsg      string
	mode        string
	since       time.Time
	t           *Transport
	cancel      context.CancelFunc
	done        chan struct{}
	proxy       string             // system proxy address currently applied, if any
	stateF      string             // sys proxy restore file
	acctErr     string             // set by an account-issue callback; takes priority when the run exits
	watchCancel context.CancelFunc // stops the "is the key usable again?" re-checker
}

func NewEngine(stateFile string) *Engine {
	return &Engine{state: "disconnected", stateF: stateFile}
}

type EngineStatus struct {
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
	Mode    string `json:"mode"`
	Since   int64  `json:"since,omitempty"`
	Up      int64  `json:"up"`
	Down    int64  `json:"down"`
	Carrier bool   `json:"carrier"`

	// Limits mirrors the server's AccountLimits for the connected key, so the GUI can show time
	// left, data left and speed cap. nil until the first successful handshake reports them.
	Limits *proto.AccountLimits `json:"limits,omitempty"`
}

func (e *Engine) Status() EngineStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := EngineStatus{State: e.state, Error: e.errMsg, Mode: e.mode}
	if e.state == "connected" || e.state == "connecting" {
		s.Since = e.since.Unix()
	}
	if e.t != nil {
		s.Up, s.Down, s.Carrier = e.t.Up.Load(), e.t.Down.Load(), e.t.Alive()
		s.Limits = e.t.Limits()
	}
	return s
}

// RefreshLimits asks the server for the connected key's current limits right now.
func (e *Engine) RefreshLimits() error {
	e.mu.Lock()
	t := e.t
	e.mu.Unlock()
	if t == nil {
		return errors.New("not connected")
	}
	return t.RefreshLimits(context.Background())
}

func (e *Engine) Running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state == "connecting" || e.state == "connected"
}

// Busy is true while a connect, a session or a teardown is in progress.
func (e *Engine) Busy() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state == "connecting" || e.state == "connected" || e.state == "disconnecting"
}

func (e *Engine) set(state, msg string) {
	e.mu.Lock()
	e.state, e.errMsg = state, msg
	e.mu.Unlock()
}

// Start connects using cfg. mode is "proxy" or "tun".
func (e *Engine) Start(cfg Config, mode string) error {
	e.mu.Lock()
	if e.state == "disconnecting" {
		e.mu.Unlock()
		return errors.New("still disconnecting, try again in a moment")
	}
	if e.state == "connecting" || e.state == "connected" {
		e.mu.Unlock()
		return errors.New("already connected")
	}
	if e.watchCancel != nil {
		e.watchCancel()
		e.watchCancel = nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel, e.done = cancel, make(chan struct{})
	e.state, e.errMsg, e.mode, e.since, e.t = "connecting", "", mode, time.Now(), nil
	done := e.done
	e.mu.Unlock()
	go func() {
		defer close(done)
		err := e.run(ctx, cancel, cfg, mode)
		cancel()
		e.mu.Lock()
		if e.proxy != "" {
			restoreSystemProxy(e.stateF)
			e.proxy = ""
		}
		switch {
		case e.acctErr != "":
			e.state, e.errMsg = "error", e.acctErr+" - checking again every 2 minutes"
			log.Printf("stopped: %s (will re-check every 2 minutes)", e.acctErr)
			e.acctErr = ""
			wctx, wc := context.WithCancel(context.Background())
			e.watchCancel = wc
			go e.resumeWatch(wctx, cfg, mode)
		case err != nil && !errors.Is(err, context.Canceled):
			e.state, e.errMsg = "error", err.Error()
			log.Printf("stopped: %v", err)
		default:
			e.state, e.errMsg = "disconnected", ""
		}
		e.mu.Unlock()
	}()
	return nil
}

func (e *Engine) run(ctx context.Context, cancel context.CancelFunc, cfg Config, mode string) error {
	pub, err := base64.StdEncoding.DecodeString(cfg.ServerPub)
	if err != nil || len(pub) != 32 {
		return errors.New("this key is damaged (bad server_pub); import it again")
	}
	psk, err := base64.StdEncoding.DecodeString(cfg.PSK)
	if err != nil {
		return errors.New("this key is damaged (bad psk); import it again")
	}
	t, err := NewTransport(&cfg, pub, psk)
	if err != nil {
		return err
	}
	t.OnAccountIssue(func(msg string) {
		e.mu.Lock()
		e.acctErr = msg
		e.mu.Unlock()
		log.Printf("account issue: %s - disconnecting", msg)
		cancel()
	})
	e.mu.Lock()
	e.t = t
	e.mu.Unlock()
	defer t.Close()
	go func() { <-ctx.Done(); t.Close() }() // kill every stream at once so teardown never waits on traffic

	cctx, cc := context.WithTimeout(ctx, 30*time.Second)
	err = t.Start(cctx)
	cc()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if proto.IsAccountError(err) {
			return errors.New(err.Error())
		}
		return fmt.Errorf("cannot reach server: %w", err)
	}
	go t.Supervise(ctx)
	go t.WatchLimits(ctx)
	log.Printf("connected to %s (%s transport, frag=%s, mode=%s)", cfg.Server, t.mode, t.frag, mode)

	socks, httpA := cfg.Socks, cfg.HTTP
	if mode == "proxy" {
		if socks == "" {
			socks = "127.0.0.1:1080"
		}
		if httpA == "" {
			httpA = "127.0.0.1:8080"
		}
		if err := startSOCKS(ctx, t, socks); err != nil {
			return fmt.Errorf("SOCKS port: %w", err)
		}
		if err := startHTTPProxy(ctx, t, httpA); err != nil {
			return fmt.Errorf("HTTP proxy port: %w", err)
		}
		if cfg.SysProxy {
			if err := setSystemProxy(httpA, e.stateF); err != nil {
				log.Printf("could not set system proxy: %v", err)
			} else {
				e.mu.Lock()
				e.proxy = httpA
				e.mu.Unlock()
				log.Printf("Windows system proxy set to %s", httpA)
			}
		}
	}
	e.set("connected", "")
	e.mu.Lock()
	e.since = time.Now()
	e.mu.Unlock()
	if mode == "tun" {
		if err := runTUN(ctx, t, &cfg); err != nil && ctx.Err() == nil {
			return err
		}
		return ctx.Err()
	}
	<-ctx.Done()
	return ctx.Err()
}

var resumeEvery = 2 * time.Minute

// resumeWatch runs after the server refused this key (disabled, expired, data cap reached). It
// tries a handshake every 2 minutes; as soon as the server accepts the key again (the admin
// extended it, raised the cap or re-enabled it) the connection is restarted automatically.
func (e *Engine) resumeWatch(ctx context.Context, cfg Config, mode string) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(resumeEvery):
		}
		if e.probeAccount(ctx, cfg) {
			log.Printf("the server accepts this key again - reconnecting")
			e.Start(cfg, mode)
			return
		}
	}
}

// probeAccount does one handshake and reports whether the server accepted the key.
func (e *Engine) probeAccount(ctx context.Context, cfg Config) bool {
	pub, err := base64.StdEncoding.DecodeString(cfg.ServerPub)
	if err != nil || len(pub) != 32 {
		return false
	}
	psk, err := base64.StdEncoding.DecodeString(cfg.PSK)
	if err != nil {
		return false
	}
	t, err := NewTransport(&cfg, pub, psk)
	if err != nil {
		return false
	}
	defer t.Close()
	cctx, cc := context.WithTimeout(ctx, 30*time.Second)
	defer cc()
	return t.Start(cctx) == nil
}

// Stop begins disconnecting (state "disconnecting" is visible at once) and waits for teardown.
func (e *Engine) Stop() {
	e.mu.Lock()
	if e.watchCancel != nil { // user chose to disconnect: stop re-checking the key
		e.watchCancel()
		e.watchCancel = nil
		if e.state == "error" {
			e.state, e.errMsg = "disconnected", ""
		}
	}
	c, d := e.cancel, e.done
	if e.state == "connecting" || e.state == "connected" {
		e.state = "disconnecting"
	}
	e.mu.Unlock()
	if c == nil {
		return
	}
	c()
	select {
	case <-d:
	case <-time.After(10 * time.Second):
	}
}

var _ = io.Discard
