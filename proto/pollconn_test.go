package proto

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPollRoundTrip(t *testing.T) {
	priv, _ := ecdh.X25519().GenerateKey(rand.Reader)
	ps := NewPollServer(priv, func(c net.Conn) { io.Copy(c, c); c.Close() })
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if suf, ok := strings.CutPrefix(r.URL.Path, "/p"); ok && ps.Handle(w, r, suf) {
			return
		}
		w.WriteHeader(404)
	}))
	defer ts.Close()
	hc := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 8}}
	c, err := PollClient(context.Background(), hc, ts.Listener.Addr().String(), "/p", priv.PublicKey().Bytes(), "UA")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	msg := make([]byte, 3<<20)
	rand.Read(msg)
	go func() {
		for i := 0; i < len(msg); i += 10000 { // many small writes
			e := i + 10000
			if e > len(msg) {
				e = len(msg)
			}
			c.Write(msg[i:e])
		}
	}()
	got := make([]byte, len(msg))
	start := time.Now()
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatal("echo mismatch")
	}
	t.Logf("3MB echoed in %v", time.Since(start))
	// idle: nothing sent for a while, then another exchange still works
	time.Sleep(1500 * time.Millisecond)
	c.Write([]byte("ping"))
	b := make([]byte, 4)
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "ping" {
		t.Fatal("post-idle echo failed", err)
	}
}
