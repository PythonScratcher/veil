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
	"testing"
)

func TestWSRoundTrip(t *testing.T) {
	priv, _ := ecdh.X25519().GenerateKey(rand.Reader)
	srvConn := make(chan net.Conn, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := WSServerUpgrade(w, r, priv)
		if err == ErrNotWebSocket {
			w.WriteHeader(404)
			return
		}
		if err != nil {
			t.Error(err)
			return
		}
		srvConn <- c
		io.Copy(c, c) // echo
	}))
	defer ts.Close()
	// non-upgrade request gets the normal path
	if r, _ := http.Get(ts.URL + "/x"); r.StatusCode != 404 {
		t.Fatal("expected 404")
	}
	raw, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c, err := WSClient(context.Background(), raw, "example.com", "/p/ws", priv.PublicKey().Bytes(), "UA")
	if err != nil {
		t.Fatal(err)
	}
	msg := make([]byte, 100000) // spans many records
	rand.Read(msg)
	go c.Write(msg)
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatal("echo mismatch")
	}
	c.Close()
}
