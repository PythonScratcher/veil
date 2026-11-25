// Package mobile is the gomobile-bound entry point used by the Android app. It wraps the same
// Transport/engine logic the Windows/Linux client uses (proto handshake, Chrome-fingerprinted
// HTTP/2 carrier, fragmentation, cover traffic, gVisor netstack TUN termination) behind a tiny
// API gomobile can turn into a Java/Kotlin-callable AAR: everything crossing the boundary is a
// plain string, int or error, since gomobile bind cannot export arbitrary Go struct fields,
// generics or channels.
package mobile

import (
	"encoding/json"
	"errors"
	"time"
)

// Client is the single object the Android app holds for the whole life of the app. Create one
// with NewClient, call Start once Android's VpnService has established the tunnel interface and
// handed back its file descriptor, and poll Status from the UI.
type Client struct {
	e *Engine
}

func NewClient() *Client { return &Client{e: NewEngine()} }

// Start parses configJSON - the same client.json shape the Windows app imports from a server key,
// minus the desktop-only fields (local proxy ports, system proxy, NetBounce) - and begins the
// tunnel using fd (the int the VpnService.Builder.establish() ParcelFileDescriptor exposes via
// detachFd()/getFd()) and the interface's mtu. Returns once the first carrier handshake succeeds
// or fails; the tunnel itself keeps running in the background after a successful return, until
// Stop is called or the connection drops on its own (reflected in Status).
func (c *Client) Start(configJSON string, fd int, mtu int) error {
	var cfg Config
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return err
	}
	done := make(chan error, 1)
	if err := c.e.Start(cfg, fd, mtu); err != nil {
		return err
	}
	// Start() above only kicks off the connect; wait here for it to reach connected/error so the
	// Kotlin side's call to Start() behaves like a synchronous "did it connect" the way the
	// Windows GUI's own connect button expects, without needing a second round trip to ask.
	go func() {
		deadline := time.Now().Add(35 * time.Second)
		for time.Now().Before(deadline) {
			st := c.e.Status()
			if st.State == "connected" || st.State == "error" || st.State == "disconnected" {
				if st.State == "error" {
					done <- errors.New(st.Error)
				} else {
					done <- nil
				}
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		done <- errors.New("timed out waiting to connect")
	}()
	return <-done
}

// Stop disconnects and blocks briefly for teardown to finish.
func (c *Client) Stop() { c.e.Stop() }

// Running reports whether a connect is in progress or the tunnel is currently up.
func (c *Client) Running() bool { return c.e.Running() }

// Status returns the current EngineStatus as JSON (state, error, since, up/down byte counters,
// carrier liveness, and account limits when the server has reported them) for the UI to render.
func (c *Client) Status() string {
	b, err := json.Marshal(c.e.Status())
	if err != nil {
		return `{"state":"error","error":"status encode failed"}`
	}
	return string(b)
}
