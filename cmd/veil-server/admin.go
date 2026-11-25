package main

import (
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"embed"
	"encoding/base64"
	"encoding/json"
	"github.com/skip2/go-qrcode"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

//go:embed web/*
var webFS embed.FS

type session struct {
	csrf string
	exp  time.Time
}

type failRec struct {
	n     int
	until time.Time
}

type admin struct {
	s        *server
	started  time.Time
	mu       sync.Mutex
	sessions map[string]*session
	fails    map[string]*failRec
}

func (s *server) serveAdmin(tlsCfg *tls.Config) {
	a := &admin{s: s, started: time.Now(), sessions: map[string]*session{}, fails: map[string]*failRec{}}
	ln, err := net.Listen("tcp", s.cfg.AdminListen)
	if err != nil {
		log.Printf("admin UI: %v", err)
		return
	}
	srv := &http.Server{Handler: a.hidden(a.routes()), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, ErrorLog: log.New(discard{}, "", 0)}
	scheme := "http"
	if s.cfg.AdminTLS {
		scheme = "https"
		srv.TLSConfig = tlsCfg.Clone()
	} else if host, _, _ := net.SplitHostPort(s.cfg.AdminListen); host != "127.0.0.1" && host != "localhost" && host != "::1" {
		log.Printf("WARNING: admin UI is on %s over plain HTTP; the password crosses the network in clear. Set \"admin_tls\": true.", s.cfg.AdminListen)
	}
	log.Printf("admin web UI on %s://%s%s", scheme, s.cfg.AdminListen, s.adminPrefix())
	if s.cfg.AdminTLS {
		err = srv.ServeTLS(ln, "", "")
	} else {
		err = srv.Serve(ln)
	}
	log.Printf("admin UI stopped: %v", err)
}

// hidden serves the real panel only under the secret prefix. Everything else
// on this port (including "/") is the decoy company website, so a scanner that
// finds :8088 sees an ordinary company site.
func (a *admin) hidden(inner http.Handler) http.Handler {
	pfx := a.s.adminPrefix()
	if pfx == "/" {
		return inner
	}
	panel := http.StripPrefix(strings.TrimSuffix(pfx, "/"), inner)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == strings.TrimSuffix(pfx, "/") {
			http.Redirect(w, r, pfx, http.StatusFound)
			return
		}
		if strings.HasPrefix(r.URL.Path, pfx) {
			panel.ServeHTTP(w, r)
			return
		}
		a.s.decoy.ServeHTTP(w, r)
	})
}

func (a *admin) routes() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServerFS(sub))
	mux.HandleFunc("POST /api/login", a.login)
	mux.HandleFunc("POST /api/logout", a.auth(a.logout))
	mux.HandleFunc("GET /api/session", a.auth(a.sessionInfo))
	mux.HandleFunc("GET /api/server", a.auth(a.serverInfo))
	mux.HandleFunc("POST /api/password", a.auth(a.changePassword))
	mux.HandleFunc("GET /api/history", a.auth(a.history))
	mux.HandleFunc("GET /api/users", a.auth(a.listUsers))
	mux.HandleFunc("POST /api/users", a.auth(a.createUser))
	mux.HandleFunc("PATCH /api/users/{id}", a.auth(a.patchUser))
	mux.HandleFunc("POST /api/users/{id}/update", a.auth(a.patchUser)) // for clients that cannot send PATCH (Android HttpURLConnection)
	mux.HandleFunc("DELETE /api/users/{id}", a.auth(a.deleteUser))
	mux.HandleFunc("POST /api/users/{id}/reset-usage", a.auth(a.resetUsage))
	mux.HandleFunc("POST /api/users/{id}/regen-key", a.auth(a.regenKey))
	mux.HandleFunc("GET /api/users/{id}/config", a.auth(a.userConfig))
	mux.HandleFunc("GET /api/users/{id}/qr.png", a.auth(a.userQR))
	return secure(mux)
}

func secure(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Cache-Control", "no-store")
		hd.Set("X-Robots-Tag", "noindex, nofollow")
		h.ServeHTTP(w, r)
	})
}

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
func jerr(w http.ResponseWriter, code int, msg string) {
	jsonOut(w, code, map[string]string{"error": msg})
}

func (a *admin) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("veil_admin")
		if err != nil {
			jerr(w, 401, "not logged in")
			return
		}
		a.mu.Lock()
		ss := a.sessions[c.Value]
		if ss != nil && time.Now().After(ss.exp) {
			delete(a.sessions, c.Value)
			ss = nil
		}
		a.mu.Unlock()
		if ss == nil {
			jerr(w, 401, "session expired")
			return
		}
		if r.Method != http.MethodGet {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(ss.csrf)) != 1 {
				jerr(w, 403, "bad csrf token")
				return
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		h(w, r)
	}
}

func randTok(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (a *admin) login(w http.ResponseWriter, r *http.Request) {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	a.mu.Lock()
	f := a.fails[ip]
	if f != nil && time.Now().Before(f.until) {
		a.mu.Unlock()
		jerr(w, 429, "too many attempts, try again in a few minutes")
		return
	}
	a.mu.Unlock()
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var in struct{ Password string }
	json.NewDecoder(r.Body).Decode(&in)
	if bcrypt.CompareHashAndPassword([]byte(a.passHash()), []byte(in.Password)) != nil {
		time.Sleep(500 * time.Millisecond)
		a.mu.Lock()
		if a.fails[ip] == nil {
			a.fails[ip] = &failRec{}
		}
		a.fails[ip].n++
		if a.fails[ip].n >= 5 {
			a.fails[ip].until, a.fails[ip].n = time.Now().Add(10*time.Minute), 0
		}
		a.mu.Unlock()
		jerr(w, 401, "wrong password")
		return
	}
	a.mu.Lock()
	delete(a.fails, ip)
	tok := randTok(32)
	ss := &session{csrf: randTok(24), exp: time.Now().Add(12 * time.Hour)}
	a.sessions[tok] = ss
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "veil_admin", Value: tok, Path: a.s.adminPrefix(), HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: 12 * 3600})
	jsonOut(w, 200, map[string]string{"csrf": ss.csrf})
}

func (a *admin) cur(r *http.Request) *session {
	c, _ := r.Cookie("veil_admin")
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[c.Value]
}

func (a *admin) logout(w http.ResponseWriter, r *http.Request) {
	c, _ := r.Cookie("veil_admin")
	a.mu.Lock()
	delete(a.sessions, c.Value)
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "veil_admin", Path: a.s.adminPrefix(), MaxAge: -1})
	jsonOut(w, 200, map[string]bool{"ok": true})
}

func (a *admin) sessionInfo(w http.ResponseWriter, r *http.Request) {
	jsonOut(w, 200, map[string]string{"csrf": a.cur(r).csrf})
}

// ---------------------------------------------------------------- views

type userView struct {
	UserRec
	PSK       string   `json:"-"`
	Status    string   `json:"status"`
	Conns     int      `json:"conns"`
	IPs       []string `json:"ips"`
	SpeedUp   int64    `json:"speed_up"`
	SpeedDown int64    `json:"speed_down"`
}

func (a *admin) view(ac *Account) userView {
	r := ac.snapshot()
	v := userView{UserRec: r, SpeedUp: ac.speedUp.Load(), SpeedDown: ac.speedDown.Load()}
	v.Conns, v.IPs = ac.liveInfo()
	switch ac.status(time.Now().Unix()) {
	case 0:
		v.Status = "active"
	case 4:
		v.Status = "disabled"
	case 5:
		v.Status = "expired"
	case 6:
		v.Status = "capped"
	}
	return v
}

func (a *admin) serverInfo(w http.ResponseWriter, r *http.Request) {
	var conns int
	var up, down int64
	var su, sd int64
	for _, ac := range a.s.store.list() {
		n, _ := ac.liveInfo()
		conns += n
		up += ac.up.Load()
		down += ac.down.Load()
		su += ac.speedUp.Load()
		sd += ac.speedDown.Load()
	}
	jsonOut(w, 200, map[string]any{"domain": a.s.cfg.Domain, "port": a.s.publicPort(), "uptime": int64(time.Since(a.started).Seconds()),
		"users": len(a.s.store.list()), "conns": conns, "total_up": up, "total_down": down, "speed_up": su, "speed_down": sd, "now": time.Now().Unix()})
}

func (a *admin) listUsers(w http.ResponseWriter, r *http.Request) {
	out := []userView{}
	for _, ac := range a.s.store.list() {
		out = append(out, a.view(ac))
	}
	jsonOut(w, 200, out)
}

// userIn: every field optional so the same body serves create and partial edit.
type userIn struct {
	Name         *string  `json:"name"`
	Note         *string  `json:"note"`
	Enabled      *bool    `json:"enabled"`
	ExpiresAt    *int64   `json:"expires_at"`     // unix, 0 = never
	AddDays      *float64 `json:"add_days"`       // extend (from now if already expired)
	DataCapBytes *int64   `json:"data_cap_bytes"` // 0 = unlimited
	AddDataBytes *int64   `json:"add_data_bytes"` // top up; if unlimited, becomes used+add
	RateMbps     *float64 `json:"rate_mbps"`
	MaxDevices   *int     `json:"max_devices"`
	ResetDays    *int     `json:"reset_days"`
}

func applyIn(ac *Account, in userIn) string {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	r := &ac.rec
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if n == "" || len(n) > 64 {
			return "name must be 1-64 characters"
		}
		r.Name = n
	}
	if in.Note != nil {
		if len(*in.Note) > 300 {
			return "note too long"
		}
		r.Note = *in.Note
	}
	if in.Enabled != nil {
		r.Enabled = *in.Enabled
	}
	if in.ExpiresAt != nil {
		if *in.ExpiresAt < 0 {
			return "bad expiry"
		}
		r.ExpiresAt = *in.ExpiresAt
	}
	if in.AddDays != nil {
		base := time.Now().Unix()
		if r.ExpiresAt > base {
			base = r.ExpiresAt
		}
		r.ExpiresAt = base + int64(*in.AddDays*86400)
	}
	if in.DataCapBytes != nil {
		if *in.DataCapBytes < 0 {
			return "bad data cap"
		}
		r.DataCap = *in.DataCapBytes
	}
	if in.AddDataBytes != nil {
		if r.DataCap == 0 {
			r.DataCap = ac.up.Load() + ac.down.Load()
		}
		r.DataCap += *in.AddDataBytes
		if r.DataCap < 0 {
			r.DataCap = 0
		}
	}
	if in.RateMbps != nil {
		if *in.RateMbps < 0 || *in.RateMbps > 100000 {
			return "bad bandwidth"
		}
		r.RateMbps = *in.RateMbps
	}
	if in.MaxDevices != nil {
		if *in.MaxDevices < 0 || *in.MaxDevices > 10000 {
			return "bad device limit"
		}
		r.MaxDevices = *in.MaxDevices
	}
	if in.ResetDays != nil {
		if *in.ResetDays < 0 || *in.ResetDays > 3650 {
			return "bad reset period"
		}
		if *in.ResetDays > 0 && r.ResetDays == 0 {
			r.LastReset = time.Now().Unix()
		}
		r.ResetDays = *in.ResetDays
	}
	ac.apply()
	return ""
}

func (a *admin) createUser(w http.ResponseWriter, r *http.Request) {
	var in userIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == nil {
		jerr(w, 400, "name required")
		return
	}
	ac, err := a.s.store.create(UserRec{Name: "x", Enabled: true})
	if err != nil {
		jerr(w, 500, err.Error())
		return
	}
	if e := applyIn(ac, in); e != "" {
		a.s.store.remove(ac.rec.ID)
		jerr(w, 400, e)
		return
	}
	a.s.store.save()
	jsonOut(w, 201, a.view(ac))
}

func (a *admin) acct(w http.ResponseWriter, r *http.Request) *Account {
	ac := a.s.store.get(r.PathValue("id"))
	if ac == nil {
		jerr(w, 404, "no such key")
	}
	return ac
}

func (a *admin) patchUser(w http.ResponseWriter, r *http.Request) {
	ac := a.acct(w, r)
	if ac == nil {
		return
	}
	var in userIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jerr(w, 400, "bad json")
		return
	}
	if e := applyIn(ac, in); e != "" {
		jerr(w, 400, e)
		return
	}
	if ac.status(time.Now().Unix()) != 0 {
		ac.killAll() // lowered/disabled: cut live tunnels now, don't wait for the tick
	}
	a.s.store.save()
	jsonOut(w, 200, a.view(ac))
}

func (a *admin) deleteUser(w http.ResponseWriter, r *http.Request) {
	if !a.s.store.remove(r.PathValue("id")) {
		jerr(w, 404, "no such key")
		return
	}
	a.s.store.save()
	jsonOut(w, 200, map[string]bool{"ok": true})
}

func (a *admin) resetUsage(w http.ResponseWriter, r *http.Request) {
	ac := a.acct(w, r)
	if ac == nil {
		return
	}
	ac.up.Store(0)
	ac.down.Store(0)
	ac.mu.Lock()
	ac.prevUp, ac.prevDown = 0, 0
	ac.rec.LastReset = time.Now().Unix()
	ac.mu.Unlock()
	a.s.store.save()
	jsonOut(w, 200, a.view(ac))
}

func (a *admin) regenKey(w http.ResponseWriter, r *http.Request) {
	ac := a.acct(w, r)
	if ac == nil {
		return
	}
	ac.regenKey()
	a.s.store.rebuildAuth()
	ac.killAll() // old key holders are cut off immediately
	a.s.store.save()
	jsonOut(w, 200, a.view(ac))
}

func (a *admin) userConfig(w http.ResponseWriter, r *http.Request) {
	ac := a.acct(w, r)
	if ac == nil {
		return
	}
	rec := ac.snapshot()
	cc := ClientConfig{
		Server: net.JoinHostPort(a.s.cfg.Domain, itoa(a.s.publicPort())), SNI: a.s.sniName(), CAPEM: a.s.cfg.ClientCA, Path: a.s.cfg.Path,
		ServerPub: b64(a.s.priv.PublicKey().Bytes()), PSK: rec.PSK, Socks: "127.0.0.1:1080", HTTP: "127.0.0.1:8080", DNS: "1.1.1.1",
	}
	if lh := a.s.cfg.ListenHTTP; lh != "" {
		_, port, _ := net.SplitHostPort(lh)
		cc.WSAddr = net.JoinHostPort(a.s.cfg.Domain, port)
	}
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", `attachment; filename="client.json"`)
	}
	w.Header().Set("Content-Type", "application/json")
	j, _ := json.MarshalIndent(cc, "", "  ")
	w.Write(j)
}

func itoa(n int) string { return strconv.Itoa(n) }

func (a *admin) history(w http.ResponseWriter, r *http.Request) {
	id, rng := r.URL.Query().Get("id"), r.URL.Query().Get("range")
	if id == "" {
		id = "total"
	}
	if rng == "" {
		rng = "24h"
	}
	names := map[string]string{}
	for _, ac := range a.s.store.list() {
		names[ac.rec.ID] = ac.snapshot().Name
	}
	if _, ok := names[id]; !ok && id != "total" {
		jerr(w, 404, "no such key")
		return
	}
	pts, step, shares, ok := a.s.store.hist.query(id, rng, time.Now().Unix(), names)
	if !ok {
		jerr(w, 400, "bad range (1h, 6h, 24h, 7d, 30d)")
		return
	}
	jsonOut(w, 200, map[string]any{"step": step, "points": pts, "per_key": shares})
}

func (a *admin) passHash() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.s.cfg.AdminPassHash
}

// changePassword verifies the current password, stores a new bcrypt hash in the config file and
// signs out every other session.
func (a *admin) changePassword(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		jerr(w, 400, "bad request")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(a.passHash()), []byte(in.Current)) != nil {
		time.Sleep(500 * time.Millisecond)
		jerr(w, 403, "current password is wrong")
		return
	}
	if len(in.New) < 10 {
		jerr(w, 400, "new password must be at least 10 characters")
		return
	}
	if len(in.New) > 72 {
		jerr(w, 400, "new password must be at most 72 characters")
		return
	}
	h, err := bcrypt.GenerateFromPassword([]byte(in.New), 11)
	if err != nil {
		jerr(w, 500, "could not hash password")
		return
	}
	if a.s.cfgPath == "" {
		jerr(w, 500, "config path unknown; use: veil-server passwd -c server.json")
		return
	}
	raw, err := os.ReadFile(a.s.cfgPath)
	var m map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &m)
	}
	if err != nil {
		jerr(w, 500, "could not read config: "+err.Error())
		return
	}
	m["admin_password_hash"] = string(h)
	j, _ := json.MarshalIndent(m, "", "  ")
	tmp := a.s.cfgPath + ".tmp"
	if err := os.WriteFile(tmp, j, 0600); err == nil {
		err = os.Rename(tmp, a.s.cfgPath)
		if err != nil {
			os.Remove(tmp)
		}
	}
	if err != nil {
		jerr(w, 500, "could not save config (is it writable by the service?): "+err.Error())
		return
	}
	keep := a.cur(r)
	a.mu.Lock()
	a.s.cfg.AdminPassHash = string(h)
	for tok, ss := range a.sessions {
		if ss != keep {
			delete(a.sessions, tok)
		}
	}
	a.mu.Unlock()
	jsonOut(w, 200, map[string]bool{"ok": true})
}

// userQR renders the key as a QR code the Android app can scan to import it (compact JSON with
// only the fields a client needs).
func (a *admin) userQR(w http.ResponseWriter, r *http.Request) {
	ac := a.acct(w, r)
	if ac == nil {
		return
	}
	rec := ac.snapshot()
	m := map[string]string{
		"server": net.JoinHostPort(a.s.cfg.Domain, itoa(a.s.publicPort())), "sni": a.s.sniName(), "path": a.s.cfg.Path,
		"server_pub": b64(a.s.priv.PublicKey().Bytes()), "psk": rec.PSK,
	}
	if a.s.cfg.ClientCA != "" {
		m["ca_pem"] = a.s.cfg.ClientCA
	}
	if lh := a.s.cfg.ListenHTTP; lh != "" {
		_, port, _ := net.SplitHostPort(lh)
		m["ws_addr"] = net.JoinHostPort(a.s.cfg.Domain, port)
	}
	j, _ := json.Marshal(m)
	png, err := qrcode.Encode(string(j), qrcode.Low, 640)
	if err != nil {
		jerr(w, 500, "key too large for a QR code")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(png)
}
