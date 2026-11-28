package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// Extra domain names that point at this server. Each can carry its own certificate: a real one from
// Let's Encrypt, or the server's own certificate (self-signed/pinned or the configured cert file).
// Client keys can be generated for any of them, so if one name gets blocked, hand out another.

type Domain struct {
	Name    string `json:"name"`
	Cert    string `json:"cert"`              // "letsencrypt" or "server"
	Primary bool   `json:"primary,omitempty"` // used for new keys / configs by default
	Note    string `json:"note,omitempty"`
	Added   int64  `json:"added"`
}

type domainStore struct {
	mu   sync.Mutex
	path string
	list []Domain
	errs map[string]string // last certificate error per domain
}

func openDomains(path string) *domainStore {
	d := &domainStore{path: path, errs: map[string]string{}}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &d.list)
	}
	return d
}

func (d *domainStore) saveLocked() error {
	b, _ := json.MarshalIndent(d.list, "", "  ")
	tmp := d.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, d.path)
}

func (d *domainStore) all() []Domain {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Domain(nil), d.list...)
}

func (d *domainStore) get(name string) (Domain, bool) {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, x := range d.list {
		if x.Name == name {
			return x, true
		}
	}
	return Domain{}, false
}

var domainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,62}$`)

func normDomain(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	s = strings.TrimSuffix(strings.SplitN(s, "/", 2)[0], ".")
	if h, _, err := net.SplitHostPort(s); err == nil {
		s = h
	}
	if len(s) > 253 || !domainRe.MatchString(s) {
		return "", errors.New("that is not a valid domain name (example: vpn.example.com)")
	}
	return s, nil
}

func (d *domainStore) add(x Domain) (Domain, error) {
	n, err := normDomain(x.Name)
	if err != nil {
		return x, err
	}
	x.Name = n
	if x.Cert == "" {
		x.Cert = "letsencrypt"
	}
	if x.Cert != "letsencrypt" && x.Cert != "server" {
		return x, errors.New(`cert must be "letsencrypt" or "server"`)
	}
	if len(x.Note) > 200 {
		x.Note = x.Note[:200]
	}
	x.Added = time.Now().Unix()
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, y := range d.list {
		if y.Name == x.Name {
			return x, errors.New("that domain is already added")
		}
	}
	if len(d.list) >= 100 {
		return x, errors.New("too many domains")
	}
	if len(d.list) == 0 {
		x.Primary = false
	}
	if x.Primary {
		for i := range d.list {
			d.list[i].Primary = false
		}
	}
	d.list = append(d.list, x)
	sort.Slice(d.list, func(i, j int) bool { return d.list[i].Name < d.list[j].Name })
	return x, d.saveLocked()
}

func (d *domainStore) update(name string, f func(*Domain)) (Domain, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range d.list {
		if d.list[i].Name == name {
			f(&d.list[i])
			return d.list[i], d.saveLocked()
		}
	}
	return Domain{}, errors.New("no such domain")
}

func (d *domainStore) setPrimary(name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	found := name == ""
	for i := range d.list {
		d.list[i].Primary = d.list[i].Name == name
		found = found || d.list[i].Primary
	}
	if !found {
		return errors.New("no such domain")
	}
	return d.saveLocked()
}

func (d *domainStore) remove(name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range d.list {
		if d.list[i].Name == name {
			d.list = append(d.list[:i], d.list[i+1:]...)
			delete(d.errs, name)
			return d.saveLocked()
		}
	}
	return errors.New("no such domain")
}

func (d *domainStore) setErr(name, e string) {
	d.mu.Lock()
	if e == "" {
		delete(d.errs, name)
	} else {
		d.errs[name] = e
	}
	d.mu.Unlock()
}

func (d *domainStore) lastErr(name string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.errs[name]
}

// ---- TLS

type certSource struct {
	s      *server
	base   *tls.Certificate // configured or self-signed certificate (nil when the main domain uses Let's Encrypt)
	mainLE bool             // the main domain (cfg.Domain) uses Let's Encrypt
	acme   *autocert.Manager
}

func (s *server) buildTLS(cfg Config, dataDir string) *tls.Config {
	cs := &certSource{s: s}
	if cfg.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			log.Fatal(err)
		}
		cs.base = &cert
	} else {
		cs.mainLE = true
	}
	dir := cfg.AutocertDir
	if dir == "" {
		dir = filepath.Join(dataDir, "autocert")
	}
	cs.acme = &autocert.Manager{
		Prompt: autocert.AcceptTOS,
		Cache:  autocert.DirCache(dir),
		HostPolicy: func(_ context.Context, host string) error {
			if cs.mainLE && host == cfg.Domain {
				return nil
			}
			if d, ok := s.domains.get(host); ok && d.Cert == "letsencrypt" {
				return nil
			}
			return errors.New("veil: host not allowed")
		},
	}
	s.certs = cs
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		NextProtos:     []string{"h2", "http/1.1", acme.ALPNProto},
		GetCertificate: cs.get,
	}
}

func (cs *certSource) leFor(name string) bool {
	if cs.mainLE && name == cs.s.cfg.Domain {
		return true
	}
	d, ok := cs.s.domains.get(name)
	return ok && d.Cert == "letsencrypt"
}

func (cs *certSource) get(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	name := strings.ToLower(strings.TrimSuffix(hello.ServerName, "."))
	if cs.leFor(name) {
		c, err := cs.acme.GetCertificate(hello)
		if err == nil {
			cs.s.domains.setErr(name, "")
			return c, nil
		}
		cs.s.domains.setErr(name, err.Error())
		if cs.base != nil {
			return cs.base, nil
		}
		return nil, err
	}
	if cs.base != nil {
		return cs.base, nil
	}
	// main domain is Let's Encrypt and this name is unknown (e.g. the decoy SNI): serve the main cert
	h := *hello
	h.ServerName = cs.s.cfg.Domain
	return cs.acme.GetCertificate(&h)
}

// httpChallenge wraps a plain-HTTP handler so Let's Encrypt can also validate over port 80.
func (cs *certSource) httpChallenge(h http.Handler) http.Handler {
	if cs == nil || cs.acme == nil {
		return h
	}
	return cs.acme.HTTPHandler(h)
}

// certInfo reports the certificate a domain currently serves (issuing one now if needed).
func (cs *certSource) certInfo(ctx context.Context, name string) map[string]any {
	out := map[string]any{}
	var c *tls.Certificate
	var err error
	check := cs.s.sniName() // a server-certificate domain is reached with the decoy name in TLS
	if cs.leFor(name) {
		check = name
		done := make(chan struct{})
		go func() {
			c, err = cs.acme.GetCertificate(&tls.ClientHelloInfo{ServerName: name, CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256}, SupportedProtos: []string{"h2"}})
			close(done)
		}()
		select {
		case <-done:
		case <-ctx.Done():
			out["status"], out["error"] = "pending", "still requesting the certificate, check again in a minute"
			return out
		}
		if err != nil {
			cs.s.domains.setErr(name, err.Error())
		} else {
			cs.s.domains.setErr(name, "")
		}
	} else {
		c = cs.base
	}
	if err != nil {
		out["status"], out["error"] = "error", err.Error()
		return out
	}
	if c == nil {
		out["status"] = "unknown"
		return out
	}
	leaf := c.Leaf
	if leaf == nil && len(c.Certificate) > 0 {
		leaf, _ = x509.ParseCertificate(c.Certificate[0])
	}
	if leaf != nil {
		out["issuer"], out["expires"] = leaf.Issuer.CommonName, leaf.NotAfter.Unix()
		out["covers"] = leaf.VerifyHostname(check) == nil
	}
	out["status"] = "ok"
	return out
}

// ---- helpers used by key/config generation

// pickDomain returns the domain to put in a client config: the requested one, the primary extra
// domain, or the main domain from server.json.
func (s *server) pickDomain(req string) (name string, le bool, err error) {
	if req != "" {
		n, err := normDomain(req)
		if err != nil {
			if req == s.cfg.Domain {
				return s.cfg.Domain, s.certs != nil && s.certs.mainLE, nil
			}
			return "", false, err
		}
		if n == strings.ToLower(s.cfg.Domain) {
			return s.cfg.Domain, s.certs != nil && s.certs.mainLE, nil
		}
		d, ok := s.domains.get(n)
		if !ok {
			return "", false, errors.New("unknown domain " + n)
		}
		return d.Name, d.Cert == "letsencrypt", nil
	}
	for _, d := range s.domains.all() {
		if d.Primary {
			return d.Name, d.Cert == "letsencrypt", nil
		}
	}
	return s.cfg.Domain, s.certs != nil && s.certs.mainLE, nil
}

// clientConfig builds the key for one user and one domain.
func (s *server) clientConfig(psk, domain string) (ClientConfig, error) {
	name, le, err := s.pickDomain(domain)
	if err != nil {
		return ClientConfig{}, err
	}
	cc := ClientConfig{
		Server: net.JoinHostPort(name, itoa(s.publicPort())), SNI: s.sniName(), CAPEM: s.cfg.ClientCA, Path: s.cfg.Path,
		ServerPub: b64(s.priv.PublicKey().Bytes()), PSK: psk, Socks: "127.0.0.1:1080", HTTP: "127.0.0.1:8080", DNS: "1.1.1.1",
	}
	if le {
		// a real certificate: the name in the TLS handshake must be the domain itself, and the
		// system's trusted roots verify it, so no pinned CA is needed
		cc.SNI, cc.CAPEM = name, ""
	}
	if lh := s.cfg.ListenHTTP; lh != "" {
		_, port, _ := net.SplitHostPort(lh)
		cc.WSAddr = net.JoinHostPort(name, port)
	}
	return cc, nil
}
