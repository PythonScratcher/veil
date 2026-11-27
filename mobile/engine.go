package mobile

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"veil/proto"
)

type Engine struct {
	mu      sync.Mutex
	state   string // disconnected | connecting | connected | disconnecting | error
	errMsg  string
	since   time.Time
	t       *Transport
	cancel  context.CancelFunc
	done    chan struct{}
	acctErr string // set by an account-issue callback; takes priority when the run exits
}

func NewEngine() *Engine { return &Engine{state: "disconnected"} }

type EngineStatus struct {
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
	Since   int64  `json:"since,omitempty"`
	Up      int64  `json:"up"`
	Down    int64  `json:"down"`
	Carrier bool   `json:"carrier"`

	// Limits mirrors the server's AccountLimits for the connected key, so the app can show time
	// left, data left and speed cap. nil until the first successful handshake reports them.
	Limits *proto.AccountLimits `json:"limits,omitempty"`
}

func (e *Engine) Status() EngineStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := EngineStatus{State: e.state, Error: e.errMsg}
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

// Start connects using cfg and the already-established VPN tunnel file descriptor fd (from
// Android's VpnService.Builder.establish()) with the given interface mtu.
func (e *Engine) Start(cfg Config, fd int, mtu int) error {
	e.mu.Lock()
	if e.state == "disconnecting" {
		e.mu.Unlock()
		return errors.New("still disconnecting, try again in a moment")
	}
	if e.state == "connecting" || e.state == "connected" {
		e.mu.Unlock()
		return errors.New("already connected")
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel, e.done = cancel, make(chan struct{})
	e.state, e.errMsg, e.since, e.t = "connecting", "", time.Now(), nil
	done := e.done
	e.mu.Unlock()
	go func() {
		defer close(done)
		err := e.run(ctx, cancel, cfg, fd, mtu)
		cancel()
		e.mu.Lock()
		switch {
		case e.acctErr != "":
			e.state, e.errMsg = "error", e.acctErr
			log.Printf("stopped: %s", e.acctErr)
			e.acctErr = ""
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

func (e *Engine) run(ctx context.Context, cancel context.CancelFunc, cfg Config, fd, mtu int) error {
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
	log.Printf("connected to %s (%s transport, frag=%s)", cfg.Server, t.mode, t.frag)

	e.mu.Lock()
	e.state, e.errMsg, e.since = "connected", "", time.Now()
	e.mu.Unlock()

	dev := newAndroidTUN(fd, mtu)
	if err := runTUN(ctx, t, dev, mtu); err != nil && ctx.Err() == nil {
		return err
	}
	return ctx.Err()
}

// Stop begins disconnecting (state "disconnecting" is visible at once) and waits for teardown.
func (e *Engine) Stop() {
	e.mu.Lock()
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
