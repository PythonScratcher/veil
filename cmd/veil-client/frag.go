package main

import (
	"bytes"
	"encoding/binary"
	"math/rand/v2"
	"net"
	"time"
)

// fragConn disrupts DPI that matches the TLS ClientHello (to read the SNI) by
// changing how the first flight is put on the wire. Both tricks are legal TLS /
// TCP, so the real server is unaffected:
//
//	record: the ClientHello is split into two or three TLS records
//	tcp:    the first record is written in several TCP segments with tiny gaps
//
// Only the first Write (the ClientHello) is touched.
type fragConn struct {
	net.Conn
	mode string // record | tcp | both
	sni  string
	done bool
}

func newFragConn(c net.Conn, mode, sni string) net.Conn {
	if mode == "none" {
		return c
	}
	return &fragConn{Conn: c, mode: mode, sni: sni}
}

func (f *fragConn) Write(p []byte) (int, error) {
	if f.done {
		return f.Conn.Write(p)
	}
	f.done = true
	if len(p) < 60 || p[0] != 0x16 || int(binary.BigEndian.Uint16(p[3:5]))+5 != len(p) {
		return f.Conn.Write(p) // not exactly one handshake record: leave it alone
	}
	body := p[5:]
	// cut points inside the SNI hostname when we can find it
	cut1 := len(body) / 3
	cut2 := 2 * len(body) / 3
	if i := bytes.Index(body, []byte(f.sni)); i > 0 && len(f.sni) >= 4 {
		cut1 = i + 1 + rand.IntN(len(f.sni)/2)
		cut2 = i + len(f.sni) - 1
		if cut2 <= cut1 {
			cut2 = cut1 + 1
		}
	}
	var segs [][]byte
	if f.mode == "record" || f.mode == "both" {
		for _, part := range [][]byte{body[:cut1], body[cut1:cut2], body[cut2:]} {
			h := []byte{0x16, p[1], p[2], 0, 0}
			binary.BigEndian.PutUint16(h[3:], uint16(len(part)))
			segs = append(segs, append(h, part...))
		}
	} else {
		segs = [][]byte{p[:5+cut1], p[5+cut1 : 5+cut2], p[5+cut2:]}
	}
	if f.mode == "record" {
		// one TCP write carrying several records
		if _, err := f.Conn.Write(bytes.Join(segs, nil)); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	// tcp / both: separate segments, small random gaps
	for i, sgm := range segs {
		if i > 0 {
			time.Sleep(time.Duration(2+rand.IntN(18)) * time.Millisecond)
		}
		if _, err := f.Conn.Write(sgm); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}
