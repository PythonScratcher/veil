package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// API keys for the REST API (/api/v1). They are created and revoked in the admin panel only, so an
// API key can never mint more keys. The secret is shown once; only its SHA-256 is stored.

type APIKey struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Scope    string `json:"scope"` // "read" (GET only) or "write" (everything in /api/v1)
	Hash     string `json:"hash"`
	Hint     string `json:"hint"` // last 4 characters, to recognise a key
	Created  int64  `json:"created"`
	LastUsed int64  `json:"last_used,omitempty"`
	LastIP   string `json:"last_ip,omitempty"`
	Expires  int64  `json:"expires,omitempty"` // 0 = never
}

type apiKeyStore struct {
	mu    sync.Mutex
	path  string
	list  []APIKey
	dirty bool
}

func openAPIKeys(path string) *apiKeyStore {
	k := &apiKeyStore{path: path}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &k.list)
	}
	return k
}

func (k *apiKeyStore) saveLocked() error {
	b, _ := json.MarshalIndent(k.list, "", "  ")
	tmp := k.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	k.dirty = false
	return os.Rename(tmp, k.path)
}

func hashSecret(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (k *apiKeyStore) create(name, scope string, days int) (APIKey, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return APIKey{}, "", errors.New("give the key a name (up to 64 characters)")
	}
	if scope != "read" && scope != "write" {
		return APIKey{}, "", errors.New(`scope must be "read" or "write"`)
	}
	id := strings.ToLower(randTok(6))
	id = strings.NewReplacer("-", "a", "_", "b").Replace(id)
	secret := randTok(30)
	token := "veil_" + id + "_" + secret
	ak := APIKey{ID: id, Name: name, Scope: scope, Hash: hashSecret(secret), Hint: secret[len(secret)-4:], Created: time.Now().Unix()}
	if days > 0 {
		ak.Expires = time.Now().Add(time.Duration(days) * 24 * time.Hour).Unix()
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.list) >= 200 {
		return APIKey{}, "", errors.New("too many API keys")
	}
	k.list = append(k.list, ak)
	return ak, token, k.saveLocked()
}

func (k *apiKeyStore) all() []APIKey {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := append([]APIKey(nil), k.list...)
	sort.Slice(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	return out
}

func (k *apiKeyStore) revoke(id string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	for i := range k.list {
		if k.list[i].ID == id {
			k.list = append(k.list[:i], k.list[i+1:]...)
			return k.saveLocked()
		}
	}
	return errors.New("no such API key")
}

// check validates "veil_<id>_<secret>" and records its use.
func (k *apiKeyStore) check(token, ip string) (APIKey, bool) {
	rest, ok := strings.CutPrefix(token, "veil_")
	if !ok {
		return APIKey{}, false
	}
	id, secret, ok := strings.Cut(rest, "_")
	if !ok {
		return APIKey{}, false
	}
	h := hashSecret(secret)
	k.mu.Lock()
	defer k.mu.Unlock()
	for i := range k.list {
		ak := &k.list[i]
		if ak.ID != id {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(ak.Hash), []byte(h)) != 1 {
			return APIKey{}, false
		}
		if ak.Expires > 0 && time.Now().Unix() > ak.Expires {
			return APIKey{}, false
		}
		now := time.Now().Unix()
		if now-ak.LastUsed > 60 || ak.LastIP != ip {
			ak.LastUsed, ak.LastIP = now, ip
			k.saveLocked()
		}
		return *ak, true
	}
	return APIKey{}, false
}

// ---- REST API

type ctxKey struct{}

// apiAuth: Authorization: Bearer veil_... (or X-API-Key). Read keys may only GET.
func (a *admin) apiAuth(write bool, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		a.mu.Lock()
		f := a.fails[ip]
		blocked := f != nil && time.Now().Before(f.until)
		a.mu.Unlock()
		if blocked {
			jerr(w, 429, "too many failed attempts, try again later")
			return
		}
		tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if tok == "" {
			tok = strings.TrimSpace(r.Header.Get("X-API-Key"))
		}
		ak, ok := a.s.apikeys.check(tok, ip)
		if !ok {
			a.mu.Lock()
			if a.fails[ip] == nil {
				a.fails[ip] = &failRec{}
			}
			a.fails[ip].n++
			if a.fails[ip].n >= 10 {
				a.fails[ip].until, a.fails[ip].n = time.Now().Add(10*time.Minute), 0
			}
			a.mu.Unlock()
			time.Sleep(300 * time.Millisecond)
			jerr(w, 401, "missing or invalid API key")
			return
		}
		if write && ak.Scope != "write" {
			jerr(w, 403, "this API key is read-only")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, ak)))
	}
}

func (a *admin) apiRoutes(mux *http.ServeMux) {
	R := func(p string, h http.HandlerFunc) { mux.HandleFunc("GET /api/v1"+p, a.apiAuth(false, h)) }
	W := func(m, p string, h http.HandlerFunc) { mux.HandleFunc(m+" /api/v1"+p, a.apiAuth(true, h)) }

	R("/me", func(w http.ResponseWriter, r *http.Request) {
		ak := r.Context().Value(ctxKey{}).(APIKey)
		jsonOut(w, 200, map[string]any{"id": ak.ID, "name": ak.Name, "scope": ak.Scope, "expires": ak.Expires})
	})
	R("/server", a.serverInfo)
	R("/stats", a.serverInfo)
	R("/history", a.history)
	R("/users", a.apiListUsers)
	W("POST", "/users", a.createUser)
	W("POST", "/users/bulk", a.bulkCreate)
	R("/users/{id}", a.getUser)
	W("PATCH", "/users/{id}", a.patchUser)
	W("POST", "/users/{id}/update", a.patchUser)
	W("DELETE", "/users/{id}", a.deleteUser)
	W("POST", "/users/{id}/enable", a.setEnabled(true))
	W("POST", "/users/{id}/disable", a.setEnabled(false))
	W("POST", "/users/{id}/reset-usage", a.resetUsage)
	W("POST", "/users/{id}/regen-key", a.regenKey)
	W("POST", "/users/{id}/kick", a.kickUser)
	R("/users/{id}/config", a.userConfig)
	R("/users/{id}/qr.png", a.userQR)
	R("/users/{id}/history", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		q.Set("id", r.PathValue("id"))
		r.URL.RawQuery = q.Encode()
		a.history(w, r)
	})
	R("/domains", a.listDomains)
	W("POST", "/domains", a.addDomain)
	R("/domains/{name}", a.getDomain)
	W("PATCH", "/domains/{name}", a.patchDomain)
	W("POST", "/domains/{name}/primary", a.primaryDomain)
	W("DELETE", "/domains/{name}", a.deleteDomain)
	for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
		mux.HandleFunc(m+" /api/v1/", func(w http.ResponseWriter, r *http.Request) { jerr(w, 404, "no such endpoint, see the API docs in the admin panel") })
	}
}

// panel-only (session) routes for API keys and domains
func (a *admin) panelExtraRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/apikeys", a.auth(func(w http.ResponseWriter, r *http.Request) {
		out := []map[string]any{}
		for _, k := range a.s.apikeys.all() {
			out = append(out, map[string]any{"id": k.ID, "name": k.Name, "scope": k.Scope, "hint": k.Hint, "created": k.Created, "last_used": k.LastUsed, "last_ip": k.LastIP, "expires": k.Expires})
		}
		jsonOut(w, 200, out)
	}))
	mux.HandleFunc("POST /api/apikeys", a.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name  string `json:"name"`
			Scope string `json:"scope"`
			Days  int    `json:"days"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			jerr(w, 400, "bad request")
			return
		}
		ak, tok, err := a.s.apikeys.create(in.Name, in.Scope, in.Days)
		if err != nil {
			jerr(w, 400, err.Error())
			return
		}
		jsonOut(w, 200, map[string]any{"id": ak.ID, "name": ak.Name, "scope": ak.Scope, "token": tok})
	}))
	mux.HandleFunc("DELETE /api/apikeys/{id}", a.auth(func(w http.ResponseWriter, r *http.Request) {
		if err := a.s.apikeys.revoke(r.PathValue("id")); err != nil {
			jerr(w, 404, err.Error())
			return
		}
		jsonOut(w, 200, map[string]any{"ok": true})
	}))
	mux.HandleFunc("GET /api/domains", a.auth(a.listDomains))
	mux.HandleFunc("POST /api/domains", a.auth(a.addDomain))
	mux.HandleFunc("GET /api/domains/{name}", a.auth(a.getDomain))
	mux.HandleFunc("PATCH /api/domains/{name}", a.auth(a.patchDomain))
	mux.HandleFunc("POST /api/domains/{name}/primary", a.auth(a.primaryDomain))
	mux.HandleFunc("DELETE /api/domains/{name}", a.auth(a.deleteDomain))
}

// ---- users (extra endpoints)

func (a *admin) apiListUsers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status, search := q.Get("status"), strings.ToLower(q.Get("q"))
	out := []userView{}
	for _, ac := range a.s.store.list() {
		v := a.view(ac)
		if status != "" && v.Status != status {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(v.Name+" "+v.Note), search) {
			continue
		}
		out = append(out, v)
	}
	jsonOut(w, 200, out)
}

func (a *admin) getUser(w http.ResponseWriter, r *http.Request) {
	if ac := a.acct(w, r); ac != nil {
		jsonOut(w, 200, a.view(ac))
	}
}

func (a *admin) setEnabled(on bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ac := a.acct(w, r)
		if ac == nil {
			return
		}
		applyIn(ac, userIn{Enabled: &on})
		if !on {
			ac.killAll()
		}
		a.s.store.save()
		jsonOut(w, 200, a.view(ac))
	}
}

func (a *admin) kickUser(w http.ResponseWriter, r *http.Request) {
	ac := a.acct(w, r)
	if ac == nil {
		return
	}
	ac.killAll()
	jsonOut(w, 200, a.view(ac))
}

// bulkCreate makes N keys with the same limits: {"count":10,"name_prefix":"class-","user":{...userIn}}
func (a *admin) bulkCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Count  int             `json:"count"`
		Prefix string          `json:"name_prefix"`
		User   json.RawMessage `json:"user"`
		Domain string          `json:"domain"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || in.Count < 1 || in.Count > 500 {
		jerr(w, 400, "count must be 1-500")
		return
	}
	if strings.TrimSpace(in.Prefix) == "" {
		in.Prefix = "key-"
	}
	var out []map[string]any
	for i := 1; i <= in.Count; i++ {
		var ui userIn
		if len(in.User) > 0 {
			if err := json.Unmarshal(in.User, &ui); err != nil {
				jerr(w, 400, "bad user object")
				return
			}
		}
		name := in.Prefix + itoa(i)
		ui.Name = &name
		ac, err := a.s.store.create(UserRec{Name: "x", Enabled: true})
		if err != nil {
			jerr(w, 500, err.Error())
			return
		}
		if e := applyIn(ac, ui); e != "" {
			a.s.store.remove(ac.rec.ID)
			a.s.store.save()
			jerr(w, 400, e)
			return
		}
		cc, _ := a.s.clientConfig(ac.snapshot().PSK, in.Domain)
		out = append(out, map[string]any{"user": a.view(ac), "config": cc})
	}
	a.s.store.save()
	jsonOut(w, 200, out)
}

// ---- domains

func (a *admin) domainView(d Domain) map[string]any {
	m := map[string]any{"name": d.Name, "cert": d.Cert, "primary": d.Primary, "note": d.Note, "added": d.Added, "main": false}
	if e := a.s.domains.lastErr(d.Name); e != "" {
		m["cert_error"] = e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ips, _ := net.DefaultResolver.LookupHost(ctx, d.Name)
	m["resolves_to"] = ips
	return m
}

func (a *admin) listDomains(w http.ResponseWriter, r *http.Request) {
	list := a.s.domains.all()
	primary := false
	for _, d := range list {
		primary = primary || d.Primary
	}
	cert := "server"
	if a.s.certs != nil && a.s.certs.mainLE {
		cert = "letsencrypt"
	}
	main := a.domainView(Domain{Name: a.s.cfg.Domain, Cert: cert, Primary: !primary})
	main["main"] = true
	out := []map[string]any{main}
	for _, d := range list {
		out = append(out, a.domainView(d))
	}
	jsonOut(w, 200, out)
}

func (a *admin) getDomain(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	d, ok := a.s.domains.get(name)
	if !ok && name != a.s.cfg.Domain {
		jerr(w, 404, "no such domain")
		return
	}
	if !ok {
		d = Domain{Name: name}
	}
	m := a.domainView(d)
	if r.URL.Query().Get("check") == "1" && a.s.certs != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		m["certificate"] = a.s.certs.certInfo(ctx, d.Name)
	}
	jsonOut(w, 200, m)
}

func (a *admin) addDomain(w http.ResponseWriter, r *http.Request) {
	var in Domain
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		jerr(w, 400, "bad request")
		return
	}
	if n, err := normDomain(in.Name); err == nil && n == strings.ToLower(a.s.cfg.Domain) {
		jerr(w, 400, "that is already the server's main domain")
		return
	}
	d, err := a.s.domains.add(in)
	if err != nil {
		jerr(w, 400, err.Error())
		return
	}
	if d.Cert == "letsencrypt" && a.s.certs != nil {
		go func() { // request the certificate in the background so it is ready when a client connects
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			a.s.certs.certInfo(ctx, d.Name)
		}()
	}
	jsonOut(w, 200, a.domainView(d))
}

func (a *admin) patchDomain(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Cert *string `json:"cert"`
		Note *string `json:"note"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		jerr(w, 400, "bad request")
		return
	}
	if in.Cert != nil && *in.Cert != "letsencrypt" && *in.Cert != "server" {
		jerr(w, 400, `cert must be "letsencrypt" or "server"`)
		return
	}
	d, err := a.s.domains.update(r.PathValue("name"), func(d *Domain) {
		if in.Cert != nil {
			d.Cert = *in.Cert
		}
		if in.Note != nil {
			d.Note = *in.Note
		}
	})
	if err != nil {
		jerr(w, 404, err.Error())
		return
	}
	jsonOut(w, 200, a.domainView(d))
}

func (a *admin) primaryDomain(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == a.s.cfg.Domain {
		name = "" // back to the main domain
	}
	if err := a.s.domains.setPrimary(name); err != nil {
		jerr(w, 404, err.Error())
		return
	}
	a.listDomains(w, r)
}

func (a *admin) deleteDomain(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("name") == a.s.cfg.Domain {
		jerr(w, 400, "the main domain is set in server.json and cannot be removed here")
		return
	}
	if err := a.s.domains.remove(r.PathValue("name")); err != nil {
		jerr(w, 404, err.Error())
		return
	}
	jsonOut(w, 200, map[string]any{"ok": true})
}
