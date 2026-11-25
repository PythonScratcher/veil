package main

import (
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var webFS embed.FS

// appVersion is stamped from the VERSION file at build time (-ldflags -X main.appVersion=...)
var appVersion = "dev"

type guiInfo struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
}

type gui struct {
	dir      string
	profile  string
	profiles string
	activeID string
	eng      *Engine
	logs     *logRing
	token    string
	port     int
	quit     chan struct{}
	qonce    sync.Once
	winMu    sync.Mutex
	winOpen  bool
}

func (g *gui) doQuit() { g.qonce.Do(func() { close(g.quit) }) }

// showWindow opens the UI unless one is already open.
func (g *gui) showWindow() {
	g.winMu.Lock()
	if g.winOpen {
		g.winMu.Unlock()
		return
	}
	g.winOpen = true
	g.winMu.Unlock()
	cmd := openWindow(fmt.Sprintf("http://127.0.0.1:%d/?t=%s", g.port, g.token), g.dir)
	go func() {
		if cmd != nil {
			cmd.Wait()
			time.Sleep(1500 * time.Millisecond)
		} else {
			time.Sleep(8 * time.Second) // plain browser tab: cannot tell when it closes
		}
		g.winMu.Lock()
		g.winOpen = false
		g.winMu.Unlock()
	}()
}

// toggleConnect is the tray's Connect/Disconnect.
func (g *gui) toggleConnect() {
	if g.eng.Running() {
		go g.eng.Stop()
		return
	}
	if g.eng.Busy() {
		return
	}
	c, err := g.loadCfg()
	if err != nil {
		g.showWindow() // no key yet
		return
	}
	if !isAdmin() {
		log.Printf("not running as Administrator: cannot create the VPN adapter")
		return
	}
	g.activeID = g.loadStore().Selected
	g.eng.Start(*c, "tun")
}

func configDir() string {
	d, err := os.UserConfigDir()
	if err != nil {
		d = "."
	}
	old := filepath.Join(d, "VeilCat") // legacy name; migrate once so saved servers survive
	d = filepath.Join(d, "Veil")
	if _, err := os.Stat(d); err != nil {
		if _, err2 := os.Stat(old); err2 == nil {
			os.Rename(old, d)
		}
	}
	os.MkdirAll(d, 0700)
	return d
}

type profileEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Cfg  Config `json:"cfg"`
}

type profileStore struct {
	Selected string         `json:"selected"`
	Profiles []profileEntry `json:"profiles"`
}

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// loadStore reads profiles.json (migrating the single-key profile.json of older versions).
func (g *gui) loadStore() *profileStore {
	var s profileStore
	if b, err := os.ReadFile(g.profiles); err == nil {
		json.Unmarshal(b, &s)
	} else if b, err := os.ReadFile(g.profile); err == nil {
		var c Config
		if json.Unmarshal(b, &c) == nil && c.Server != "" {
			host, _, _ := net.SplitHostPort(c.Server)
			id := newID()
			s = profileStore{Selected: id, Profiles: []profileEntry{{id, host, c}}}
			g.saveStore(&s)
			os.Remove(g.profile)
		}
	}
	ok := false
	for _, p := range s.Profiles {
		if p.ID == s.Selected {
			ok = true
		}
	}
	if !ok && len(s.Profiles) > 0 {
		s.Selected = s.Profiles[0].ID
	}
	return &s
}

func (g *gui) saveStore(s *profileStore) error {
	b, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(g.profiles, b, 0600)
}

func (s *profileStore) find(id string) *profileEntry {
	for i := range s.Profiles {
		if s.Profiles[i].ID == id {
			return &s.Profiles[i]
		}
	}
	return nil
}

func (s *profileStore) uniqueName(n string) string {
	name := n
	for i := 2; ; i++ {
		clash := false
		for _, p := range s.Profiles {
			if strings.EqualFold(p.Name, name) {
				clash = true
			}
		}
		if !clash {
			return name
		}
		name = fmt.Sprintf("%s (%d)", n, i)
	}
}

// loadCfg returns the selected server's config.
func (g *gui) loadCfg() (*Config, error) {
	s := g.loadStore()
	p := s.find(s.Selected)
	if p == nil {
		return nil, errors.New("no server")
	}
	c := p.Cfg
	return &c, nil
}

func (g *gui) saveCfg(c *Config) error {
	s := g.loadStore()
	p := s.find(s.Selected)
	if p == nil {
		return errors.New("no server")
	}
	p.Cfg = *c
	return g.saveStore(s)
}

func validateCfg(c *Config) error {
	if c.Server == "" || c.Path == "" {
		return errors.New("this does not look like a Veil client.json (missing server/path)")
	}
	if b, err := base64.StdEncoding.DecodeString(c.ServerPub); err != nil || len(b) != 32 {
		return errors.New("bad server_pub in this key")
	}
	if b, err := base64.StdEncoding.DecodeString(c.PSK); err != nil || len(b) < 16 {
		return errors.New("bad psk in this key")
	}
	if _, _, err := net.SplitHostPort(c.Server); err != nil {
		return errors.New("server must look like host:port")
	}
	return nil
}

func ping(port int) bool {
	c := http.Client{Timeout: 800 * time.Millisecond}
	r, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/ping", port))
	if err != nil {
		return false
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(r.Body, 32))
	return strings.TrimSpace(string(b)) == "veil-gui"
}

func runGUI(noOpen, elevated bool) {
	g := &gui{dir: configDir(), logs: &logRing{}, quit: make(chan struct{})}
	g.profile = filepath.Join(g.dir, "profile.json")
	g.profiles = filepath.Join(g.dir, "profiles.json")
	log.SetOutput(io.MultiWriter(os.Stderr, g.logs))
	infoPath := filepath.Join(g.dir, "gui.json")
	proxyState := filepath.Join(g.dir, "sysproxy.json")

	// single instance: if another copy is running, just show its window
	deadline := time.Now().Add(6 * time.Second)
	for {
		var gi guiInfo
		b, err := os.ReadFile(infoPath)
		if err == nil && json.Unmarshal(b, &gi) == nil && gi.Port != 0 && ping(gi.Port) {
			if elevated && time.Now().Before(deadline) { // the non-admin copy is still shutting down
				time.Sleep(300 * time.Millisecond)
				continue
			}
			openWindow(fmt.Sprintf("http://127.0.0.1:%d/?t=%s", gi.Port, gi.Token), g.dir)
			return
		}
		break
	}
	if elevateAtLaunch && !isAdmin() { // a VPN adapter needs Administrator: ask once, at launch
		if err := relaunchAsAdmin(); err != nil {
			log.Printf("Administrator rights are needed for the VPN: %v", err)
		}
		return
	}
	restoreSystemProxy(proxyState) // leftovers from older versions
	g.eng = NewEngine(proxyState)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	g.port = ln.Addr().(*net.TCPAddr).Port
	tb := make([]byte, 24)
	rand.Read(tb)
	g.token = hex.EncodeToString(tb)
	ib, _ := json.Marshal(guiInfo{g.port, g.token})
	os.WriteFile(infoPath, ib, 0600)
	defer os.Remove(infoPath)

	srv := &http.Server{Handler: g.routes(), ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	go srv.Serve(ln)
	url := fmt.Sprintf("http://127.0.0.1:%d/?t=%s", g.port, g.token)
	log.Printf("Veil %s ready", appVersion)
	if noOpen {
		fmt.Println("UI:", url)
	} else {
		g.showWindow()
	}
	runTray(g) // blocks until Quit
	g.eng.Stop()
	restoreSystemProxy(proxyState)
	srv.Close()
}

func (g *gui) authed(r *http.Request) bool {
	host := r.Host
	if host != fmt.Sprintf("127.0.0.1:%d", g.port) && host != fmt.Sprintf("localhost:%d", g.port) {
		return false // DNS-rebinding guard
	}
	c, err := r.Cookie("veilgui")
	return err == nil && c.Value == g.token
}

func (g *gui) routes() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(webFS, "web")
	static := http.FileServerFS(sub)
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("veil-gui")) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if t := r.URL.Query().Get("t"); t != "" && t == g.token && r.URL.Path == "/" {
			http.SetCookie(w, &http.Cookie{Name: "veilgui", Value: g.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		if !g.authed(r) {
			http.Error(w, "Open Veil from its window.", http.StatusForbidden)
			return
		}
		static.ServeHTTP(w, r)
	})
	api := func(f func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !g.authed(r) || (r.Method == http.MethodPost && r.Header.Get("X-Veil") != "1") {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			f(w, r)
		}
	}
	mux.HandleFunc("GET /api/state", api(g.state))
	mux.HandleFunc("GET /api/logs", api(func(w http.ResponseWriter, r *http.Request) { jsonOut(w, 200, g.logs.Snapshot()) }))
	mux.HandleFunc("POST /api/import", api(g.importKey))
	mux.HandleFunc("POST /api/settings", api(g.settings))
	mux.HandleFunc("POST /api/connect", api(g.connect))
	mux.HandleFunc("POST /api/disconnect", api(func(w http.ResponseWriter, r *http.Request) {
		go g.eng.Stop() // returns at once; the UI shows "Disconnecting…" until teardown finishes
		time.Sleep(50 * time.Millisecond)
		jsonOut(w, 200, map[string]any{"ok": true})
	}))
	mux.HandleFunc("POST /api/select", api(g.selectProfile))
	mux.HandleFunc("POST /api/rename", api(g.renameProfile))
	mux.HandleFunc("POST /api/remove", api(g.removeProfile))
	mux.HandleFunc("POST /api/elevate", api(func(w http.ResponseWriter, r *http.Request) {
		if err := relaunchAsAdmin(); err != nil {
			jsonOut(w, 400, map[string]any{"error": err.Error()})
			return
		}
		jsonOut(w, 200, map[string]any{"ok": true})
		go func() { time.Sleep(400 * time.Millisecond); g.doQuit() }()
	}))
	mux.HandleFunc("POST /api/quit", api(func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, 200, map[string]any{"ok": true})
		go func() { time.Sleep(200 * time.Millisecond); g.doQuit() }()
	}))
	return secureHeaders(mux)
}

func secureHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		h.ServeHTTP(w, r)
	})
}

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

type settingsView struct {
	Transport string `json:"transport"`
	Frag      string `json:"frag"`
	Carrier   string `json:"carrier"`
	Rotate    int    `json:"rotate_minutes"`
	Cover     bool   `json:"cover"`
	Mode      string `json:"mode"`
	SysProxy  bool   `json:"sys_proxy"`
	Socks     string `json:"listen_socks"`
	HTTP      string `json:"listen_http"`
	DNS       string `json:"tun_dns"`
	NetBounce bool   `json:"net_bounce"`
}

func viewOf(c *Config) settingsView {
	v := settingsView{Transport: c.Transport, Frag: c.Frag, Carrier: c.Carrier, Rotate: c.Rotate, Cover: c.Cover == nil || *c.Cover,
		Mode: c.Mode, SysProxy: c.SysProxy, Socks: c.Socks, HTTP: c.HTTP, DNS: c.DNS, NetBounce: c.NetBounce != nil && *c.NetBounce}
	if v.Transport == "" {
		v.Transport = "stream"
	}
	if v.Frag == "" {
		v.Frag = "both"
	}
	if v.Carrier != "ws" && v.Carrier != "poll" && v.Carrier != "quic" {
		v.Carrier = "tls"
	}
	if v.Rotate == 0 {
		v.Rotate = 20
	}
	if v.Mode == "" {
		v.Mode = "proxy"
	}
	if v.Socks == "" {
		v.Socks = "127.0.0.1:1080"
	}
	if v.HTTP == "" {
		v.HTTP = "127.0.0.1:8080"
	}
	if v.DNS == "" {
		v.DNS = "1.1.1.1"
	}
	return v
}

func (g *gui) state(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"engine": g.eng.Status(), "windows": isWindows, "mac": isMac, "admin": isAdmin(), "version": appVersion, "os": runtime.GOOS, "active": g.activeID}
	s := g.loadStore()
	list := []map[string]any{}
	for _, p := range s.Profiles {
		list = append(list, map[string]any{"id": p.ID, "name": p.Name, "server": p.Cfg.Server})
	}
	out["profiles"], out["selected"] = list, s.Selected
	if p := s.find(s.Selected); p != nil {
		c := &p.Cfg
		host, port, _ := net.SplitHostPort(c.Server)
		out["profile"] = map[string]any{"name": p.Name, "server": c.Server, "host": host, "port": port, "sni": c.SNI, "local_cert": c.CAPEM != "" || c.CAFile != ""}
		out["settings"] = viewOf(c)
	}
	jsonOut(w, 200, out)
}

func (g *gui) importKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
		Name string `json:"name"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in) != nil {
		jsonOut(w, 400, map[string]any{"error": "bad request"})
		return
	}
	txt := strings.TrimSpace(in.Text)
	if !strings.HasPrefix(txt, "{") { // also accept a base64-wrapped key
		if b, err := base64.StdEncoding.DecodeString(txt); err == nil {
			txt = string(b)
		}
	}
	var c Config
	if err := json.Unmarshal([]byte(txt), &c); err != nil {
		jsonOut(w, 400, map[string]any{"error": "that is not valid JSON - paste the whole client.json"})
		return
	}
	if err := validateCfg(&c); err != nil {
		jsonOut(w, 400, map[string]any{"error": err.Error()})
		return
	}
	c.TUN, c.CAFile, c.Socks, c.HTTP, c.Mode, c.SysProxy = true, "", "", "", "tun", false // a pasted key never pulls in local file paths
	s := g.loadStore()
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name, _, _ = net.SplitHostPort(c.Server)
	}
	for i := range s.Profiles { // same server + secret already added: update it instead of duplicating
		p := &s.Profiles[i]
		if p.Cfg.Server == c.Server && p.Cfg.PSK == c.PSK {
			if g.eng.Busy() && g.activeID == p.ID {
				jsonOut(w, 400, map[string]any{"error": "disconnect first"})
				return
			}
			c.Carrier = p.Cfg.Carrier
			c.Transport, c.Frag, c.Rotate, c.Cover, c.DNS, c.NetBounce = p.Cfg.Transport, p.Cfg.Frag, p.Cfg.Rotate, p.Cfg.Cover, p.Cfg.DNS, p.Cfg.NetBounce
			p.Cfg = c
			s.Selected = p.ID
			g.saveStore(s)
			jsonOut(w, 200, map[string]any{"ok": true, "updated": true})
			return
		}
	}
	id := newID()
	s.Profiles = append(s.Profiles, profileEntry{id, s.uniqueName(name), c})
	if !g.eng.Busy() {
		s.Selected = id
	}
	if err := g.saveStore(s); err != nil {
		jsonOut(w, 500, map[string]any{"error": err.Error()})
		return
	}
	jsonOut(w, 200, map[string]any{"ok": true})
}

func (g *gui) selectProfile(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in)
	if g.eng.Busy() {
		jsonOut(w, 400, map[string]any{"error": "disconnect before switching server"})
		return
	}
	s := g.loadStore()
	if s.find(in.ID) == nil {
		jsonOut(w, 400, map[string]any{"error": "unknown server"})
		return
	}
	s.Selected = in.ID
	g.saveStore(s)
	jsonOut(w, 200, map[string]any{"ok": true})
}

func (g *gui) renameProfile(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in)
	in.Name = strings.TrimSpace(in.Name)
	s := g.loadStore()
	p := s.find(in.ID)
	if p == nil || in.Name == "" || len(in.Name) > 40 {
		jsonOut(w, 400, map[string]any{"error": "give the server a name (up to 40 characters)"})
		return
	}
	p.Name = ""
	p.Name = s.uniqueName(in.Name)
	g.saveStore(s)
	jsonOut(w, 200, map[string]any{"ok": true})
}

func (g *gui) removeProfile(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in)
	if g.eng.Busy() && g.activeID == in.ID {
		jsonOut(w, 400, map[string]any{"error": "disconnect first"})
		return
	}
	s := g.loadStore()
	out := s.Profiles[:0]
	for _, p := range s.Profiles {
		if p.ID != in.ID {
			out = append(out, p)
		}
	}
	s.Profiles = out
	if s.Selected == in.ID {
		s.Selected = ""
		if len(out) > 0 {
			s.Selected = out[0].ID
		}
	}
	g.saveStore(s)
	jsonOut(w, 200, map[string]any{"ok": true})
}

func hostPortOK(s string) bool {
	_, p, err := net.SplitHostPort(s)
	return err == nil && p != ""
}

func (g *gui) settings(w http.ResponseWriter, r *http.Request) {
	c, err := g.loadCfg()
	if err != nil {
		jsonOut(w, 400, map[string]any{"error": "import a key first"})
		return
	}
	var in settingsView
	if json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in) != nil {
		jsonOut(w, 400, map[string]any{"error": "bad request"})
		return
	}
	switch {
	case in.Transport != "stream" && in.Transport != "split":
		jsonOut(w, 400, map[string]any{"error": "bad transport"})
		return
	case in.Carrier != "tls" && in.Carrier != "ws" && in.Carrier != "poll" && in.Carrier != "quic":
		jsonOut(w, 400, map[string]any{"error": "bad connection method"})
		return
	case in.Frag != "both" && in.Frag != "record" && in.Frag != "tcp" && in.Frag != "none":
		jsonOut(w, 400, map[string]any{"error": "bad fragmentation mode"})
		return
	}
	if in.Rotate == 0 || in.Rotate < -1 || in.Rotate > 1440 {
		in.Rotate = 20
	}
	cv, nb := in.Cover, in.NetBounce
	c.Carrier = in.Carrier
	c.Transport, c.Frag, c.Rotate, c.Cover, c.Mode, c.SysProxy = in.Transport, in.Frag, in.Rotate, &cv, "tun", false
	c.NetBounce = &nb
	c.DNS = strings.TrimSpace(in.DNS)
	if err := g.saveCfg(c); err != nil {
		jsonOut(w, 500, map[string]any{"error": err.Error()})
		return
	}
	jsonOut(w, 200, map[string]any{"ok": true})
}

func (g *gui) connect(w http.ResponseWriter, r *http.Request) {
	c, err := g.loadCfg()
	if err != nil {
		jsonOut(w, 400, map[string]any{"error": "import a key first"})
		return
	}
	if !isAdmin() {
		jsonOut(w, 400, map[string]any{"error": "Veil needs Administrator rights to create the VPN adapter", "need_admin": true})
		return
	}
	g.activeID = g.loadStore().Selected
	if err := g.eng.Start(*c, "tun"); err != nil {
		jsonOut(w, 400, map[string]any{"error": err.Error()})
		return
	}
	jsonOut(w, 200, map[string]any{"ok": true})
}
