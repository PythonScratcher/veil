package proto

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func pairSessions(t *testing.T) (cli, srv *Session) {
	a, b := net.Pipe()
	cli, srv = NewSession(a, true), NewSession(b, false)
	srv.OnOpen = func(st *MuxStream, tg Target) {
		if tg.Host == "refuse.test" {
			st.Accept(StatusFailed)
			return
		}
		st.Accept(StatusOK)
		go func() { io.Copy(st, st); st.CloseWrite() }() // echo
	}
	go cli.Run()
	go srv.Run()
	t.Cleanup(func() { cli.Close(); srv.Close() })
	return
}

func TestMuxEchoManyStreamsFlowControl(t *testing.T) {
	cli, _ := pairSessions(t)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, err := cli.OpenStream(context.Background(), Target{NetTCP, "echo.test", 80})
			if err != nil {
				t.Error(err)
				return
			}
			size := 1000 + i*40000 // up to ~1.6MB, well beyond the 256KB window
			msg := make([]byte, size)
			rand.Read(msg)
			go func() { st.Write(msg); st.CloseWrite() }()
			got, err := io.ReadAll(st)
			if err != nil || !bytes.Equal(got, msg) {
				t.Errorf("stream %d mismatch (%d/%d bytes, err=%v)", i, len(got), size, err)
			}
			st.Close()
		}(i)
	}
	wg.Wait()
	time.Sleep(50 * time.Millisecond)
	if n := cli.NumStreams(); n != 0 {
		t.Fatalf("leaked %d streams", n)
	}
}

func TestMuxRefusedAndReset(t *testing.T) {
	cli, srv := pairSessions(t)
	if _, err := cli.OpenStream(context.Background(), Target{NetTCP, "refuse.test", 1}); err == nil {
		t.Fatal("expected refusal")
	}
	st, err := cli.OpenStream(context.Background(), Target{NetTCP, "x.test", 1})
	if err != nil {
		t.Fatal(err)
	}
	st.Write([]byte("hi"))
	buf := make([]byte, 2)
	io.ReadFull(st, buf)
	// server drops the whole carrier: client streams must error, not hang
	srv.Close()
	done := make(chan error, 1)
	go func() { _, err := st.Read(make([]byte, 10)); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error after carrier death")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("read hung after carrier death")
	}
}

func TestTargetCodec(t *testing.T) {
	for _, tg := range []Target{{NetTCP, "example.org", 443}, {NetUDP, "1.2.3.4", 53}, {NetTCP, "2001:db8::1", 8080}} {
		got, n, err := parseTarget(appendTarget(nil, tg))
		if err != nil || got != tg || n != len(appendTarget(nil, tg)) {
			t.Fatalf("codec %v -> %v %v", tg, got, err)
		}
	}
}

func TestMuxUDPKeepsDatagramBoundaries(t *testing.T) {
	cli, _ := pairSessions(t)
	st, err := cli.OpenStream(context.Background(), Target{NetUDP, "dns.test", 53})
	if err != nil {
		t.Fatal(err)
	}
	// the echo server above copies with io.Copy, so datagrams may merge on the way back;
	// here we only assert the send side never splits one datagram into two frames.
	big := make([]byte, 20000) // over maxFrame: must be dropped, not split
	n, err := st.Write(big)
	if err != nil || n != len(big) {
		t.Fatalf("oversize datagram write = %d, %v", n, err)
	}
	small := []byte("ping")
	st.Write(small)
	buf := make([]byte, 100)
	k, _ := st.Read(buf)
	if string(buf[:k]) != "ping" {
		t.Fatalf("got %q (oversize datagram leaked through?)", buf[:k])
	}
}
