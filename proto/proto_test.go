package proto

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"io"
	"testing"
)

func setup(t *testing.T) (*ecdh.PrivateKey, []User, []byte) {
	priv, _ := ecdh.X25519().GenerateKey(rand.Reader)
	psk := make([]byte, 32)
	rand.Read(psk)
	other := make([]byte, 32)
	rand.Read(other)
	return priv, []User{{"bob", other}, {"alice", psk}}, psk
}

func TestRoundTrip(t *testing.T) {
	priv, users, psk := setup(t)
	tgt := Target{NetTCP, "example.org", 443}
	h, err := NewClientHello(priv.PublicKey().Bytes(), psk, tgt)
	if err != nil {
		t.Fatal(err)
	}
	acc, err := ServerAccept(priv, users, bytes.NewReader(h.Bytes), NewReplayCache())
	if err != nil {
		t.Fatal(err)
	}
	if acc.Target != tgt || acc.User.Name != "alice" {
		t.Fatalf("got %+v %v", acc.Target, acc.User.Name)
	}
	// wire up a full duplex in memory
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	acc.r = c2sR
	done := make(chan *Conn)
	go func() {
		c, err := acc.Complete(s2cW, StatusOK, nil)
		if err != nil {
			t.Error(err)
		}
		done <- c
	}()
	cc, err := h.Finish(s2cR, c2sW, nil)
	if err != nil {
		t.Fatal(err)
	}
	sc := <-done
	msg := bytes.Repeat([]byte("meow "), 10000) // > MaxData, forces splitting
	go func() { cc.Write(msg); cc.CloseWrite() }()
	got, err := io.ReadAll(sc)
	if err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("mismatch len=%d err=%v", len(got), err)
	}
}

func TestRejects(t *testing.T) {
	priv, users, psk := setup(t)
	tgt := Target{NetTCP, "1.2.3.4", 80}
	h, _ := NewClientHello(priv.PublicKey().Bytes(), psk, tgt)

	rc := NewReplayCache()
	if _, err := ServerAccept(priv, users, bytes.NewReader(h.Bytes), rc); err != nil {
		t.Fatal(err)
	}
	if _, err := ServerAccept(priv, users, bytes.NewReader(h.Bytes), rc); err != ErrReplay {
		t.Fatalf("replay not rejected: %v", err)
	}
	bad := make([]byte, 32)
	rand.Read(bad)
	h2, _ := NewClientHello(priv.PublicKey().Bytes(), bad, tgt)
	if _, err := ServerAccept(priv, users, bytes.NewReader(h2.Bytes), NewReplayCache()); err != ErrAuth {
		t.Fatalf("wrong psk accepted: %v", err)
	}
	wrongSrv, _ := ecdh.X25519().GenerateKey(rand.Reader)
	h3, _ := NewClientHello(wrongSrv.PublicKey().Bytes(), psk, tgt)
	if _, err := ServerAccept(priv, users, bytes.NewReader(h3.Bytes), NewReplayCache()); err != ErrAuth {
		t.Fatalf("wrong server key accepted: %v", err)
	}
	h4, _ := NewClientHello(priv.PublicKey().Bytes(), psk, tgt)
	h4.Bytes[len(h4.Bytes)-1] ^= 1
	if _, err := ServerAccept(priv, users, bytes.NewReader(h4.Bytes), NewReplayCache()); err != ErrAuth {
		t.Fatalf("tampered hello accepted: %v", err)
	}
	junk := make([]byte, 300)
	rand.Read(junk)
	if _, err := ServerAccept(priv, users, bytes.NewReader(junk), NewReplayCache()); err == nil {
		t.Fatal("junk accepted")
	}
}
