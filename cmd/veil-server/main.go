// veil-server: Linux server side of the Veil tunnel.
package main

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/net/http2"

	"veil/proto"
)

type UserCfg struct {
	Name string `json:"name"`
	PSK  string `json:"psk"`
}

type Config struct {
	Listen       string     `json:"listen"`
	Domain       string     `json:"domain"`
	SNI          string     `json:"sni,omitempty"`
	ClientCA     string     `json:"client_ca,omitempty"`
	Update       *UpdateCfg `json:"update_site,omitempty"` // branding for the built-in update-server decoy
	CertFile     string     `json:"cert_file,omitempty"`
	KeyFile      string     `json:"key_file,omitempty"`
	AutocertDir  string     `json:"autocert_dir,omitempty"`
	PrivateKey   string     `json:"private_key"`
	Users        []UserCfg  `json:"users"`
	Path         string     `json:"path"`
	DecoyURL     string     `json:"decoy_url,omitempty"` // reverse-proxy a real website
	DecoyDir     string     `json:"decoy_dir,omitempty"` // or serve static files
	AllowPrivate bool       `json:"allow_private,omitempty"`

	// admin web UI
	AdminListen   string `json:"admin_listen,omitempty"`        // e.g. 127.0.0.1:8088 or 0.0.0.0:8088
	AdminPassHash string `json:"admin_password_hash,omitempty"` // bcrypt
	AdminTLS      bool   `json:"admin_tls,omitempty"`
	ListenQUIC    string `json:"listen_quic,omitempty"`    // optional UDP/QUIC listener (e.g. 0.0.0.0:443), same certificate as HTTPS
	ListenHTTP    string `json:"listen_http,omitempty"`    // optional plain-HTTP listener (e.g. 0.0.0.0:80): decoy site + WebSocket carrier
	AdminPrefix   string `json:"admin_prefix,omitempty"`   // secret URL path of the panel; everything else on the admin port is the decoy           // serve admin over HTTPS using the server certificate
	DataDir       string `json:"data_dir,omitempty"`       // where users.json lives (default: next to server.json)
	PublicPort    int    `json:"public_port,omitempty"`    // port clients should use (default: port of "listen")
	RealIPHeader  string `json:"real_ip_header,omitempty"` // e.g. CF-Connecting-IP when behind a CDN
}

type ClientConfig struct {
	Server    string `json:"server"`
	SNI       string `json:"sni"`
	Path      string `json:"path"`
	ServerPub string `json:"server_pub"`
	PSK       string `json:"psk"`
	ConnectIP string `json:"connect_ip,omitempty"`
	CAPEM     string `json:"ca_pem,omitempty"`
	CAFile    string `json:"ca_file,omitempty"`
	WSAddr    string `json:"ws_addr,omitempty"`   // plain-HTTP WebSocket carrier (host:port), used when HTTPS is blocked
	Transport string `json:"transport,omitempty"` // "stream" (default) or "split"
	Frag      string `json:"frag,omitempty"`      // ClientHello fragmentation: none|tcp|record|both (default both)
	Rotate    int    `json:"rotate_minutes,omitempty"`
	Cover     *bool  `json:"cover,omitempty"`
	Socks     string `json:"listen_socks"`
	HTTP      string `json:"listen_http"`
	TUN       bool   `json:"tun"`
	DNS       string `json:"tun_dns,omitempty"`
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "init":
		cmdInit(os.Args[2:])
	case "run":
		cmdRun(os.Args[2:])
	case "passwd":
		cmdPasswd(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:\n  veil-server passwd -c server.json                        (reset the admin web UI password)\n  veil-server init --domain example.com [--listen :443]   (writes server.json + client.json)\n  veil-server run  -c server.json")
	os.Exit(2)
}

func cmdInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	domain := fs.String("domain", "", "public domain name pointing at this server")
	listen := fs.String("listen", ":443", "listen address")
	user := fs.String("user", "alice", "first user name")
	local := fs.Bool("local", false, "no domain/Let's Encrypt: generate a local self-signed certificate (pinned by the client)")
	sniFlag := fs.String("sni", "", "with --local: hostname clients present in TLS (default www.corvane.com)")
	decoy := fs.String("decoy-url", "", "optional real site to reverse-proxy for non-Veil visitors")
	fs.Parse(args)
	if *domain == "" {
		log.Fatal("--domain required (with --local: the server's public IP or hostname)")
	}
	if *local && *listen == ":443" {
		*listen = ":8443"
	}
	priv, _ := ecdh.X25519().GenerateKey(rand.Reader)
	psk := make([]byte, 32)
	rand.Read(psk)
	pathb := make([]byte, 12)
	rand.Read(pathb)
	path := "/api/v3/sync/" + hex.EncodeToString(pathb)
	adminPass := randPass(18)
	pfxb := make([]byte, 10)
	rand.Read(pfxb)
	prefix := "/console-" + hex.EncodeToString(pfxb) + "/"
	hash, _ := bcrypt.GenerateFromPassword([]byte(adminPass), 11)
	sc := Config{AdminListen: "0.0.0.0:8088", AdminTLS: true, AdminPrefix: prefix, AdminPassHash: string(hash), Listen: *listen, Domain: *domain, AutocertDir: "/var/lib/veil/certs",
		PrivateKey: b64(priv.Bytes()), Users: []UserCfg{{*user, b64(psk)}}, Path: path, DecoyURL: *decoy}
	_, lport, _ := net.SplitHostPort(*listen)
	if lport == "" {
		lport = "443"
	}
	if *local {
		sni := *sniFlag
		if sni == "" {
			sni = "www.corvane.com"
		}
		cp, kp, cpem := makeLocalCert(".", sni, *domain)
		abs := func(p string) string { a, _ := filepath.Abs(p); return a }
		sc.CertFile, sc.KeyFile, sc.AutocertDir, sc.SNI, sc.ClientCA = abs(cp), abs(kp), "", sni, cpem
	}
	cc := ClientConfig{Server: *domain + ":" + lport, SNI: *domain, Path: path, ServerPub: b64(priv.PublicKey().Bytes()),
		PSK: b64(psk), Socks: "127.0.0.1:1080", HTTP: "127.0.0.1:8080", TUN: false, DNS: "1.1.1.1"}
	write := func(name string, v any) {
		j, _ := json.MarshalIndent(v, "", "  ")
		if err := os.WriteFile(name, j, 0600); err != nil {
			log.Fatal(err)
		}
		fmt.Println("wrote", name)
	}
	if *local {
		cc.SNI, cc.CAPEM = sc.SNI, sc.ClientCA
	}
	write("server.json", sc)
	write("client.json", cc)
	fmt.Println("copy client.json to the Windows machine. Keep server.json private.")
	fmt.Printf("\nAdmin web UI: https://%s:8088%s   (open TCP 8088; the root of :8088 is a decoy company website, only this secret path is the panel)\nAdmin password (shown once): %s\n", *domain, prefix, adminPass)
	fmt.Println("Prefer it private? set \"admin_listen\": \"127.0.0.1:8088\" in server.json and use: ssh -L 8088:127.0.0.1:8088 you@server")
}

type server struct {
	priv    *ecdh.PrivateKey
	store   *Store
	replay  *proto.ReplayCache
	cfg     Config
	cfgPath string
	decoy   http.Handler
	dialer  *net.Dialer
	splits  splitReg
}

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("c", "server.json", "config file")
	fs.Parse(args)
	raw, err := os.ReadFile(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		log.Fatal(err)
	}
	pk, err := base64.StdEncoding.DecodeString(cfg.PrivateKey)
	if err != nil {
		log.Fatal("private_key: ", err)
	}
	priv, err := ecdh.X25519().NewPrivateKey(pk)
	if err != nil {
		log.Fatal("private_key: ", err)
	}
	ensureAdminPrefix(&cfg, *cfgPath)
	s := &server{priv: priv, replay: proto.NewReplayCache(), cfg: cfg, cfgPath: *cfgPath}
	dataDir := cfg.DataDir
	if dataDir == "" {
		dataDir = filepath.Dir(*cfgPath)
	}
	s.store, err = openStore(filepath.Join(dataDir, "users.json"), cfg.Users)
	if err != nil {
		log.Fatal("users.json: ", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	storeDone := make(chan struct{})
	go func() { s.store.run(ctx); close(storeDone) }()
	s.decoy = buildDecoy(cfg)
	if s.decoy == nil {
		s.decoy = s.updateSite()
	}
	s.dialer = &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if !cfg.AllowPrivate {
		s.dialer.Control = denyPrivate
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == cfg.Path {
			s.tunnel(w, r)
			return
		}
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, cfg.Path+"/") {
			s.splitUpload(w, r)
			return
		}
		s.decoy.ServeHTTP(w, r)
	})

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			log.Fatal(err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
		tlsCfg.NextProtos = []string{"h2", "http/1.1"}
	} else {
		m := &autocert.Manager{Prompt: autocert.AcceptTOS, HostPolicy: autocert.HostWhitelist(cfg.Domain), Cache: autocert.DirCache(cfg.AutocertDir)}
		tlsCfg = m.TLSConfig()
		tlsCfg.MinVersion = tls.VersionTLS12
	}
	srv := &http.Server{Handler: mux, TLSConfig: tlsCfg, ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 5 * time.Minute, ErrorLog: log.New(discard{}, "", 0)}
	http2.ConfigureServer(srv, &http2.Server{
		MaxUploadBufferPerStream:     1 << 20,
		MaxUploadBufferPerConnection: 16 << 20,
		MaxConcurrentStreams:         250,
		IdleTimeout:                  5 * time.Minute,
	})
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Fatal(err)
	}
	if cfg.ListenQUIC != "" {
		qh2 := &http2.Server{MaxUploadBufferPerStream: 1 << 20, MaxUploadBufferPerConnection: 16 << 20, MaxConcurrentStreams: 250, IdleTimeout: 5 * time.Minute}
		qopts := &http2.ServeConnOpts{Handler: srv.Handler, BaseConfig: srv}
		go func() {
			err := proto.ServeQUIC(ctx, cfg.ListenQUIC, tlsCfg, func(c net.Conn) { qh2.ServeConn(c, qopts); c.Close() })
			if err != nil && ctx.Err() == nil {
				log.Printf("QUIC listener stopped: %v", err)
			}
		}()
		log.Printf("QUIC carrier listening on udp %s", cfg.ListenQUIC)
	}
	var hs *http.Server
	if cfg.ListenHTTP != "" {
		hln, err := net.Listen("tcp", cfg.ListenHTTP)
		if err != nil {
			log.Fatal("listen_http: ", err)
		}
		h2s := &http2.Server{MaxUploadBufferPerStream: 1 << 20, MaxUploadBufferPerConnection: 16 << 20, MaxConcurrentStreams: 250, IdleTimeout: 5 * time.Minute}
		hs = &http.Server{Handler: s.plainHandler(h2s, srv), ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 5 * time.Minute, ErrorLog: log.New(discard{}, "", 0)}
		go hs.Serve(hln)
		log.Printf("plain-HTTP carrier + decoy listening on %s", cfg.ListenHTTP)
	}
	log.Printf("veil-server listening on %s for %s (%d key(s))", cfg.Listen, cfg.Domain, len(*s.store.auth.Load()))
	if cfg.AdminListen != "" && cfg.AdminPassHash != "" {
		go s.serveAdmin(tlsCfg)
	} else {
		log.Printf("admin web UI disabled (no admin_listen / admin_password_hash; run `veil-server passwd`)")
	}
	go func() {
		if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	log.Printf("shutting down, saving usage...")
	srv.Close()
	if hs != nil {
		hs.Close()
	}
	<-storeDone
}

func randPass(n int) string {
	const al = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = al[int(b[i])%len(al)]
	}
	return string(b)
}

func cmdPasswd(args []string) {
	fs := flag.NewFlagSet("passwd", flag.ExitOnError)
	cfgPath := fs.String("c", "server.json", "config file")
	fs.Parse(args)
	raw, err := os.ReadFile(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		log.Fatal(err)
	}
	pw := randPass(18)
	h, _ := bcrypt.GenerateFromPassword([]byte(pw), 11)
	m["admin_password_hash"] = string(h)
	if _, ok := m["admin_listen"]; !ok {
		m["admin_listen"], m["admin_tls"] = "0.0.0.0:8088", true
	}
	j, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(*cfgPath, j, 0600); err != nil {
		log.Fatal(err)
	}
	fmt.Println("new admin password:", pw, "\n(restart veil-server to apply)")
}

func (s *server) sniName() string {
	if s.cfg.SNI != "" {
		return s.cfg.SNI
	}
	return s.cfg.Domain
}

// makeLocalCert writes a self-signed certificate valid for the SNI name and the
// server address, so no domain name or Let's Encrypt is needed.
func makeLocalCert(dir, sni, host string) (certPath, keyPath, certPEM string) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	sn, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tpl := &x509.Certificate{SerialNumber: sn, Subject: pkix.Name{CommonName: sni},
		NotBefore: time.Now().Add(-24 * time.Hour), NotAfter: time.Now().AddDate(10, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true,
		DNSNames: []string{sni}}
	if ip := net.ParseIP(host); ip != nil {
		tpl.IPAddresses = []net.IP{ip}
	} else if host != sni {
		tpl.DNSNames = append(tpl.DNSNames, host)
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		log.Fatal(err)
	}
	kd, _ := x509.MarshalPKCS8PrivateKey(key)
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	certPath, keyPath = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, []byte(certPEM), 0644); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}), 0600); err != nil {
		log.Fatal(err)
	}
	return
}

func (s *server) adminPrefix() string {
	p := s.cfg.AdminPrefix
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// ensureAdminPrefix gives older configs a secret panel path on first start.
func ensureAdminPrefix(cfg *Config, path string) {
	if cfg.AdminListen == "" || cfg.AdminPrefix != "" {
		return
	}
	b := make([]byte, 10)
	rand.Read(b)
	cfg.AdminPrefix = "/console-" + hex.EncodeToString(b) + "/"
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var mm map[string]any
	if json.Unmarshal(raw, &mm) != nil {
		return
	}
	mm["admin_prefix"] = cfg.AdminPrefix
	j, _ := json.MarshalIndent(mm, "", "  ")
	os.WriteFile(path, j, 0600)
}

func (s *server) clientIP(r *http.Request) string {
	if h := s.cfg.RealIPHeader; h != "" {
		if v := r.Header.Get(h); v != "" {
			return v
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *server) publicPort() int {
	if s.cfg.PublicPort != 0 {
		return s.cfg.PublicPort
	}
	if _, p, err := net.SplitHostPort(s.cfg.Listen); err == nil {
		if n, _ := strconv.Atoi(p); n != 0 {
			return n
		}
	}
	return 443
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

type flushWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (fw flushWriter) Write(p []byte) (int, error) { return fw.w.Write(p) }
func (fw flushWriter) Flush()                      { fw.f.Flush() }

func (s *server) tunnel(w http.ResponseWriter, r *http.Request) {
	type result struct {
		acc *proto.Accepted
		err error
	}
	ch := make(chan result, 1)
	go func() {
		acc, err := proto.ServerAccept(s.priv, *s.store.auth.Load(), r.Body, s.replay)
		ch <- result{acc, err}
	}()
	var res result
	select {
	case res = <-ch:
	case <-time.After(8 * time.Second):
		s.decoy.ServeHTTP(w, r)
		return
	}
	if res.err != nil {
		// Unauthenticated: indistinguishable from a visit to a missing page.
		s.decoy.ServeHTTP(w, r)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		s.decoy.ServeHTTP(w, r)
		return
	}
	acc := res.acc
	a := s.store.get(acc.User.Name) // proto.User.Name carries the key id
	if a == nil {
		s.decoy.ServeHTTP(w, r)
		return
	}
	a.lastSeen.Store(time.Now().Unix())
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	fw := flushWriter{w, fl}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// Authenticated but not allowed: tell the client why, inside the encrypted channel.
	if st := a.status(time.Now().Unix()); st != proto.StatusOK {
		acc.Complete(fw, st, nil)
		return
	}
	unreg, ok := a.register(s.clientIP(r), func() { cancel(); r.Body.Close() })
	if !ok {
		acc.Complete(fw, proto.StatusBusy, nil)
		return
	}
	defer unreg()

	if acc.Target.Net == proto.NetMux {
		s.serveCarrier(a, acc, fw, r, ctx, cancel)
		return
	}
	dst := acc.Target.Addr()
	if dialHook != nil {
		dst = dialHook(dst)
	}
	tc, err := s.dialer.DialContext(ctx, acc.Target.NetName(), dst)
	status := proto.StatusOK
	if err != nil {
		status = proto.StatusFailed
		var ne net.Error
		switch {
		case errors.Is(err, errDenied):
			status = proto.StatusNotAllowed
		case errors.As(err, &ne) && ne.Timeout():
			status = proto.StatusTimeout
		}
		log.Printf("[%s] %s %s: %v", a.rec.Name, acc.Target.NetName(), acc.Target.Addr(), err)
	}
	conn, cerr := acc.Complete(fw, status, r.Body.Close)
	if cerr != nil || status != proto.StatusOK {
		return
	}
	go func() { <-ctx.Done(); tc.Close() }() // kill switch: admin edits / expiry close the target too
	m := &metered{s: conn, a: a}
	if acc.Target.Net == proto.NetUDP {
		relayUDP(m, tc)
	} else {
		proto.Pipe(tc, m)
	}
}

func relayUDP(c proto.Stream, u net.Conn) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 65535)
		for {
			u.SetReadDeadline(time.Now().Add(90 * time.Second))
			n, err := u.Read(buf)
			if err != nil {
				return
			}
			if _, err := c.Write(buf[:n]); err != nil {
				return
			}
		}
	}()
	buf := make([]byte, proto.MaxData)
	for {
		n, err := c.Read(buf)
		if err != nil {
			break
		}
		u.Write(buf[:n])
	}
	u.Close()
	<-done
}

// dialHook is nil in release builds; only the veiltest build tag sets it.
var dialHook func(string) string

var errDenied = errors.New("destination is a private/loopback address")

// denyPrivate stops authenticated users from reaching the server's own
// loopback/LAN/cloud-metadata addresses (SSRF), checked on the resolved IP.
func denyPrivate(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return errDenied
	}
	return nil
}

// ---------------------------------------------------------------- decoy

func buildDecoy(cfg Config) http.Handler {
	switch {
	case cfg.DecoyURL != "":
		u, err := url.Parse(cfg.DecoyURL)
		if err != nil {
			log.Fatal("decoy_url: ", err)
		}
		rp := httputil.NewSingleHostReverseProxy(u)
		d := rp.Director
		rp.Director = func(r *http.Request) { d(r); r.Host = u.Host }
		rp.ErrorLog = log.New(discard{}, "", 0)
		return rp
	case cfg.DecoyDir != "":
		return nginxish(http.FileServer(http.Dir(cfg.DecoyDir)))
	}
	return nil // built later: needs the server (update site)
}

func nginxish(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "nginx")
		h.ServeHTTP(w, r)
	})
}

// plainHandler serves the plain-HTTP port: the decoy website for everyone, except a WebSocket
// upgrade on the secret path, which becomes a sealed carrier running the normal HTTP/2 session.
func (s *server) plainHandler(h2s *http2.Server, srv *http.Server) http.Handler {
	opts := &http2.ServeConnOpts{Handler: srv.Handler, BaseConfig: srv}
	polls := proto.NewPollServer(s.priv, func(c net.Conn) { h2s.ServeConn(c, opts); c.Close() })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == s.cfg.Path+"/ws" {
			c, err := proto.WSServerUpgrade(w, r, s.priv)
			if err == nil {
				h2s.ServeConn(c, opts)
				c.Close()
				return
			}
			if err != proto.ErrNotWebSocket {
				return // connection already taken over
			}
		} else if suf, ok := strings.CutPrefix(r.URL.Path, s.cfg.Path+"/p"); ok && (suf == "/n" || strings.HasPrefix(suf, "/")) {
			if polls.Handle(w, r, suf) {
				return
			}
		}
		s.decoy.ServeHTTP(w, r)
	})
}
