// Package proto implements the Veil inner protocol.
//
// Veil rides inside a normal HTTPS/HTTP2 POST stream. Everything below is
// *additional* encryption on top of TLS, so a middlebox that terminates TLS
// with an injected CA (or a CDN that you do not fully trust) still learns
// nothing about the tunnel contents or destinations.
//
//	client                                         server
//	  | E_c || AEAD_k1(len) || AEAD_k1(early)  ->   |   k1 = HKDF(DH(e_c,S), salt=PSK)
//	  | <-  E_s || AEAD_k2(status record)            |   k2 = HKDF(DH(e_c,S)||DH(e_c,E_s), salt=PSK, transcript)
//	  | <-> AEAD records (random padded)       <->   |
//
// S is the server's pinned static X25519 key. PSK is a per-user secret.
package proto

import (
	"bytes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	NetTCP byte = 1
	NetUDP byte = 2
	NetMux byte = 3 // carrier: a multiplexed session instead of a single connection

	StatusOK         byte = 0
	StatusFailed     byte = 1
	StatusNotAllowed byte = 2
	StatusTimeout    byte = 3
	StatusDisabled   byte = 4
	StatusExpired    byte = 5
	StatusQuota      byte = 6
	StatusBusy       byte = 7

	maxSkew = 120 * time.Second
)

var (
	ErrAuth     = errors.New("veil: authentication failed")
	ErrDisabled = errors.New("this key has been disabled by the server admin")
	ErrExpired  = errors.New("this key has expired")
	ErrQuota    = errors.New("data cap reached for this key")
	ErrReplay   = errors.New("veil: replayed hello")
)

type Target struct {
	Net  byte
	Host string
	Port uint16
}

func (t Target) Addr() string { return net.JoinHostPort(t.Host, strconv.Itoa(int(t.Port))) }
func (t Target) NetName() string {
	if t.Net == NetUDP {
		return "udp"
	}
	return "tcp"
}

type User struct {
	Name string
	PSK  []byte
}

func x25519() ecdh.Curve { return ecdh.X25519() }

func newAEAD(key []byte) cipher.AEAD {
	a, err := chacha20poly1305.New(key)
	if err != nil {
		panic(err)
	}
	return a
}

func nonce(ctr uint64) []byte {
	var n [12]byte
	binary.LittleEndian.PutUint64(n[4:], ctr)
	return n[:]
}

func earlyKey(dh, psk []byte) []byte {
	k, err := hkdf.Key(sha256.New, dh, psk, "veil1 early", 32)
	if err != nil {
		panic(err)
	}
	return k
}

func sessionKeys(dh1, dh2, psk, ec, es []byte) (c2s, s2c []byte) {
	prk, err := hkdf.Extract(sha256.New, append(append([]byte{}, dh1...), dh2...), psk)
	if err != nil {
		panic(err)
	}
	tr := append(append([]byte{}, ec...), es...)
	c2s, _ = hkdf.Expand(sha256.New, prk, "veil1 c2s"+string(tr), 32)
	s2c, _ = hkdf.Expand(sha256.New, prk, "veil1 s2c"+string(tr), 32)
	return
}

// ---------------------------------------------------------------- client

type ClientHello struct {
	Bytes []byte // write this first on the request body

	eph       *ecdh.PrivateKey
	ephPub    []byte
	serverPub *ecdh.PublicKey
	dh1       []byte
	psk       []byte

	// Limits is set by Finish on a successful handshake if the server reported account limits
	// alongside its StatusOK (expiry, data cap/usage, rate limit). nil if the server didn't send
	// any (older server build) or the handshake failed.
	Limits *AccountLimits
}

// AccountLimits is what the server reports about the connecting key right after a successful
// handshake. The GUI shows these as time/data/speed limits; a nil or zero field means "no limit
// set" for that dimension. It is refreshed for free on every carrier rotation, since each
// rotation redoes the handshake.
type AccountLimits struct {
	ExpiresAt int64   `json:"expires_at,omitempty"` // unix seconds, 0 = never expires
	DataCap   int64   `json:"data_cap,omitempty"`   // total up+down bytes allowed, 0 = unlimited
	UsedUp    int64   `json:"used_up"`
	UsedDown  int64   `json:"used_down"`
	RateMbps  float64 `json:"rate_mbps,omitempty"` // per direction, 0 = unlimited
}

func NewClientHello(serverPub, psk []byte, t Target) (*ClientHello, error) {
	sp, err := x25519().NewPublicKey(serverPub)
	if err != nil {
		return nil, fmt.Errorf("bad server key: %w", err)
	}
	eph, err := x25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	dh1, err := eph.ECDH(sp)
	if err != nil {
		return nil, err
	}
	// early plaintext
	var p bytes.Buffer
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(time.Now().Unix()))
	p.Write(ts[:])
	p.WriteByte(t.Net)
	if ip := net.ParseIP(t.Host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			p.WriteByte(1)
			p.Write(v4)
		} else {
			p.WriteByte(4)
			p.Write(ip.To16())
		}
	} else {
		if len(t.Host) == 0 || len(t.Host) > 255 {
			return nil, errors.New("bad host")
		}
		p.WriteByte(3)
		p.WriteByte(byte(len(t.Host)))
		p.WriteString(t.Host)
	}
	var pt [2]byte
	binary.BigEndian.PutUint16(pt[:], t.Port)
	p.Write(pt[:])
	padn := int(randU32() % 256)
	var pl [2]byte
	binary.BigEndian.PutUint16(pl[:], uint16(padn))
	p.Write(pl[:])
	p.Write(make([]byte, padn))

	k1 := newAEAD(earlyKey(dh1, psk))
	var lb [2]byte
	binary.BigEndian.PutUint16(lb[:], uint16(p.Len()))
	out := append([]byte{}, eph.PublicKey().Bytes()...)
	out = k1.Seal(out, nonce(0), lb[:], nil)
	out = k1.Seal(out, nonce(1), p.Bytes(), nil)
	return &ClientHello{Bytes: out, eph: eph, ephPub: eph.PublicKey().Bytes(), serverPub: sp, dh1: dh1, psk: psk}, nil
}

// Finish reads the server hello from r and returns the secure connection.
func (h *ClientHello) Finish(r io.Reader, w io.Writer, closer func() error) (*Conn, error) {
	es := make([]byte, 32)
	if _, err := io.ReadFull(r, es); err != nil {
		return nil, err
	}
	esPub, err := x25519().NewPublicKey(es)
	if err != nil {
		return nil, ErrAuth
	}
	dh2, err := h.eph.ECDH(esPub)
	if err != nil {
		return nil, ErrAuth
	}
	c2s, s2c := sessionKeys(h.dh1, dh2, h.psk, h.ephPub, es)
	c := newConn(r, w, newAEAD(c2s), newAEAD(s2c), closer)
	typ, data, err := c.readRecord()
	if err != nil {
		return nil, err
	}
	if typ != recStatus || len(data) < 1 {
		return nil, ErrAuth
	}
	switch data[0] {
	case StatusOK:
		if len(data) > 1 {
			var lim AccountLimits
			if json.Unmarshal(data[1:], &lim) == nil {
				h.Limits = &lim
			}
		}
		return c, nil
	case StatusNotAllowed:
		return nil, errors.New("destination not allowed by server")
	case StatusTimeout:
		return nil, errors.New("destination timed out")
	case StatusDisabled:
		return nil, ErrDisabled
	case StatusExpired:
		return nil, ErrExpired
	case StatusQuota:
		return nil, ErrQuota
	case StatusBusy:
		return nil, errors.New("too many devices are using this key")
	default:
		return nil, errors.New("destination connect failed")
	}
}

// IsAccountError reports whether err means the key itself is the problem (disabled, expired, or
// out of data) rather than a reachability or per-destination issue - the caller should stop
// retrying and tell the person, not treat it like a dropped connection.
func IsAccountError(err error) bool {
	return errors.Is(err, ErrDisabled) || errors.Is(err, ErrExpired) || errors.Is(err, ErrQuota)
}

// ---------------------------------------------------------------- server

type ReplayCache struct {
	mu   sync.Mutex
	seen map[[32]byte]time.Time
}

func NewReplayCache() *ReplayCache {
	rc := &ReplayCache{seen: map[[32]byte]time.Time{}}
	go func() {
		for range time.Tick(time.Minute) {
			rc.mu.Lock()
			for k, t := range rc.seen {
				if time.Since(t) > 4*maxSkew {
					delete(rc.seen, k)
				}
			}
			rc.mu.Unlock()
		}
	}()
	return rc
}

func (rc *ReplayCache) check(k [32]byte) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if _, ok := rc.seen[k]; ok {
		return false
	}
	rc.seen[k] = time.Now()
	return true
}

type Accepted struct {
	Target Target
	User   *User

	r    io.Reader
	priv *ecdh.PrivateKey
	ec   []byte
	dh1  []byte
	psk  []byte
}

// ServerAccept reads and authenticates the client hello. On any failure it
// returns an error and the caller must behave exactly like an ordinary website.
func ServerAccept(priv *ecdh.PrivateKey, users []User, r io.Reader, rc *ReplayCache) (*Accepted, error) {
	hdr := make([]byte, 32+2+16)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	ec := hdr[:32]
	ecPub, err := x25519().NewPublicKey(ec)
	if err != nil {
		return nil, ErrAuth
	}
	dh1, err := priv.ECDH(ecPub)
	if err != nil {
		return nil, ErrAuth
	}
	var k1 cipher.AEAD
	var user *User
	var lb []byte
	for i := range users {
		a := newAEAD(earlyKey(dh1, users[i].PSK))
		if l, err := a.Open(nil, nonce(0), hdr[32:], nil); err == nil {
			k1, user, lb = a, &users[i], l
			break
		}
	}
	if k1 == nil {
		return nil, ErrAuth
	}
	n := int(binary.BigEndian.Uint16(lb))
	if n < 14 || n > 4096 {
		return nil, ErrAuth
	}
	body := make([]byte, n+16)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	pt, err := k1.Open(nil, nonce(1), body, nil)
	if err != nil {
		return nil, ErrAuth
	}
	ts := int64(binary.BigEndian.Uint64(pt[:8]))
	if d := time.Since(time.Unix(ts, 0)); d > maxSkew || d < -maxSkew {
		return nil, ErrAuth
	}
	var key [32]byte
	copy(key[:], ec)
	if !rc.check(key) {
		return nil, ErrReplay
	}
	t := Target{Net: pt[8]}
	if t.Net != NetTCP && t.Net != NetUDP && t.Net != NetMux {
		return nil, ErrAuth
	}
	p := pt[9:]
	switch p[0] {
	case 1:
		if len(p) < 1+4+2 {
			return nil, ErrAuth
		}
		t.Host = net.IP(p[1:5]).String()
		p = p[5:]
	case 4:
		if len(p) < 1+16+2 {
			return nil, ErrAuth
		}
		t.Host = net.IP(p[1:17]).String()
		p = p[17:]
	case 3:
		if len(p) < 2 || len(p) < 2+int(p[1])+2 {
			return nil, ErrAuth
		}
		l := int(p[1])
		t.Host = string(p[2 : 2+l])
		p = p[2+l:]
	default:
		return nil, ErrAuth
	}
	t.Port = binary.BigEndian.Uint16(p[:2])
	return &Accepted{Target: t, User: user, r: r, priv: priv, ec: append([]byte{}, ec...), dh1: dh1, psk: user.PSK}, nil
}

// SetReader replaces the reader the session will read client records from
// (split transport: an ordered reassembly of many POST bodies).
func (a *Accepted) SetReader(r io.Reader) { a.r = r }

// Complete sends the server hello + status and returns the secure connection.
func (a *Accepted) Complete(w io.Writer, status byte, closer func() error) (*Conn, error) {
	return a.CompleteWithLimits(w, status, closer, nil)
}

// CompleteWithLimits is Complete, plus an optional JSON-encoded AccountLimits payload tacked on
// after the status byte when status is StatusOK - this is how the client learns its expiry, data
// cap/usage and rate limit without a separate round trip, refreshed for free on every carrier
// rotation since each rotation redoes the handshake.
func (a *Accepted) CompleteWithLimits(w io.Writer, status byte, closer func() error, limits []byte) (*Conn, error) {
	ecPub, _ := x25519().NewPublicKey(a.ec)
	eph, err := x25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	dh2, err := eph.ECDH(ecPub)
	if err != nil {
		return nil, err
	}
	es := eph.PublicKey().Bytes()
	c2s, s2c := sessionKeys(a.dh1, dh2, a.psk, a.ec, es)
	c := newConn(a.r, w, newAEAD(s2c), newAEAD(c2s), closer)
	payload := []byte{status}
	if status == StatusOK && len(limits) > 0 {
		payload = append(payload, limits...)
	}
	rec := c.sealRecord(recStatus, payload)
	if _, err := w.Write(append(append([]byte{}, es...), rec...)); err != nil {
		return nil, err
	}
	flush(w)
	return c, nil
}

func randU32() uint32 {
	var b [4]byte
	rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}
