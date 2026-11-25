package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"veil/proto"
)

func dialTCP(ctx context.Context, t *Transport, host string, port uint16) (proto.Stream, error) {
	return t.Dial(ctx, proto.Target{Net: proto.NetTCP, Host: host, Port: port})
}

// ---------------------------------------------------------------- SOCKS5

// startSOCKS binds addr now (so a busy port is reported immediately) and serves until ctx ends.
func startSOCKS(ctx context.Context, t *Transport, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	log.Printf("SOCKS5 proxy on %s", addr)
	go func() { <-ctx.Done(); ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handleSOCKS(t, c)
		}
	}()
	return nil
}

func handleSOCKS(t *Transport, c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(15 * time.Second))
	var h [2]byte
	if _, err := io.ReadFull(c, h[:]); err != nil || h[0] != 5 {
		return
	}
	io.CopyN(io.Discard, c, int64(h[1]))
	c.Write([]byte{5, 0})
	var r [4]byte
	if _, err := io.ReadFull(c, r[:]); err != nil || r[0] != 5 {
		return
	}
	if r[1] != 1 { // CONNECT only
		c.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	var host string
	switch r[3] {
	case 1:
		var b [4]byte
		io.ReadFull(c, b[:])
		host = net.IP(b[:]).String()
	case 4:
		var b [16]byte
		io.ReadFull(c, b[:])
		host = net.IP(b[:]).String()
	case 3:
		var l [1]byte
		io.ReadFull(c, l[:])
		b := make([]byte, l[0])
		io.ReadFull(c, b)
		host = string(b)
	default:
		return
	}
	var pb [2]byte
	if _, err := io.ReadFull(c, pb[:]); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(pb[:])
	rc, err := dialTCP(context.Background(), t, host, port)
	if err != nil {
		log.Printf("socks %s:%d: %v", host, port, err)
		c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	c.SetDeadline(time.Time{})
	proto.Pipe(c, rc)
}

// ---------------------------------------------------------------- HTTP proxy

func startHTTPProxy(ctx context.Context, t *Transport, addr string) error {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, a string) (net.Conn, error) {
			host, ps, err := net.SplitHostPort(a)
			if err != nil {
				return nil, err
			}
			p, _ := strconv.Atoi(ps)
			rc, err := dialTCP(ctx, t, host, uint16(p))
			if err != nil {
				return nil, err
			}
			return connAdapter{rc}, nil
		},
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	log.Printf("HTTP proxy on %s", addr)
	srv := &http.Server{ReadHeaderTimeout: 20 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	go func() { <-ctx.Done(); srv.Close() }()
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			host, ps, err := net.SplitHostPort(r.Host)
			if err != nil {
				http.Error(w, "bad host", 400)
				return
			}
			p, _ := strconv.Atoi(ps)
			rc, err := dialTCP(r.Context(), t, host, uint16(p))
			if err != nil {
				http.Error(w, err.Error(), 502)
				return
			}
			hj, ok := w.(http.Hijacker)
			if !ok {
				rc.Close()
				return
			}
			c, _, err := hj.Hijack()
			if err != nil {
				rc.Close()
				return
			}
			c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
			proto.Pipe(c, rc)
			return
		}
		if !r.URL.IsAbs() {
			http.Error(w, "proxy only", 400)
			return
		}
		r.RequestURI = ""
		r.Header.Del("Proxy-Connection")
		resp, err := tr.RoundTrip(r)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	})
	go srv.Serve(ln)
	return nil
}

// connAdapter gives a proto.Stream the net.Conn surface http.Transport wants.
type connAdapter struct{ proto.Stream }

func (connAdapter) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (connAdapter) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (connAdapter) SetDeadline(time.Time) error      { return nil }
func (connAdapter) SetReadDeadline(time.Time) error  { return nil }
func (connAdapter) SetWriteDeadline(time.Time) error { return nil }

var _ = errors.New
var _ = fmt.Sprint
var _ = strings.TrimSpace
