package proto

// The QUIC carrier: the same multiplexed HTTP/2 session, run over one bidirectional QUIC stream on
// UDP (normally port 443). To a network it looks like HTTP/3 traffic (ALPN "h3"). It is usually
// faster than TCP on lossy links, but plenty of hostile networks drop UDP, so it is an option the
// user picks, never an automatic fallback.

import (
	"context"
	"crypto/tls"
	"net"
	"time"

	"github.com/quic-go/quic-go"
)

type quicStreamConn struct {
	*quic.Stream
	conn *quic.Conn
}

func (q *quicStreamConn) Close() error {
	q.Stream.CancelRead(0)
	q.Stream.Close()
	// give the final data a moment to flush before tearing the connection down
	go func() {
		time.Sleep(300 * time.Millisecond)
		q.conn.CloseWithError(0, "")
	}()
	return nil
}
func (q *quicStreamConn) LocalAddr() net.Addr  { return q.conn.LocalAddr() }
func (q *quicStreamConn) RemoteAddr() net.Addr { return q.conn.RemoteAddr() }

var quicCfg = &quic.Config{
	MaxIdleTimeout:  60 * time.Second,
	KeepAlivePeriod: 15 * time.Second,
}

// DialQUIC connects to addr over QUIC and returns the single stream as a net.Conn.
func DialQUIC(ctx context.Context, addr string, tc *tls.Config) (net.Conn, error) {
	tc = tc.Clone()
	tc.NextProtos = []string{"h3"}
	conn, err := quic.DialAddr(ctx, addr, tc, quicCfg)
	if err != nil {
		return nil, err
	}
	st, err := conn.OpenStreamSync(ctx)
	if err != nil {
		conn.CloseWithError(0, "")
		return nil, err
	}
	return &quicStreamConn{Stream: st, conn: conn}, nil
}

// ServeQUIC accepts QUIC connections on the UDP address and passes every stream to handle (each
// call runs in its own goroutine). It returns when the listener fails or ctx is cancelled.
func ServeQUIC(ctx context.Context, addr string, tc *tls.Config, handle func(net.Conn)) error {
	tc = tc.Clone()
	tc.NextProtos = []string{"h3"}
	ln, err := quic.ListenAddr(addr, tc, quicCfg)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			return err
		}
		go func() {
			defer conn.CloseWithError(0, "")
			for {
				st, err := conn.AcceptStream(ctx)
				if err != nil {
					return
				}
				go handle(&quicStreamConn{Stream: st, conn: conn})
			}
		}()
	}
}
