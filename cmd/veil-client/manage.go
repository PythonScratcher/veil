package main

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// "Manage server": the server's own admin web panel, shown through a small local proxy so it works
// with the key's pinned certificate (or a self-signed one) and never needs a certificate warning.
// It is served from a SECOND local port, i.e. a different browser origin than the Veil window, so
// the (remote, user-supplied) panel page can never call the Veil window's own API.

var mng struct {
	sync.Mutex
	port  int
	token string
}

func adminTransport(c *Config) *http.Transport {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.AdminTrustAny {
		tc.InsecureSkipVerify = true
	} else {
		pool, _ := x509.SystemCertPool()
		if pool == nil {
			pool = x509.NewCertPool()
		}
		if c.CAPEM != "" {
			pool.AppendCertsFromPEM([]byte(c.CAPEM))
		}
		tc.RootCAs = pool
	}
	return &http.Transport{TLSClientConfig: tc, ResponseHeaderTimeout: 30 * time.Second, DialContext: (&net.Dialer{Timeout: 15 * time.Second}).DialContext}
}

func parseAdminURL(s string) (*url.URL, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("enter the admin panel address first")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("that is not a valid admin panel address")
	}
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	u.RawQuery, u.Fragment = "", ""
	return u, nil
}

// startManager starts the local listener once and returns its port and access token.
func (g *gui) startManager() (int, string, error) {
	mng.Lock()
	defer mng.Unlock()
	if mng.port != 0 {
		return mng.port, mng.token, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, "", err
	}
	tb := make([]byte, 24)
	rand.Read(tb)
	mng.port, mng.token = ln.Addr().(*net.TCPAddr).Port, hex.EncodeToString(tb)
	port, token := mng.port, mng.token
	srv := &http.Server{Handler: g.manageHandler(port, token), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	return port, token, nil
}

func (g *gui) manageHandler(port int, token string) http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			c, _ := g.loadCfg()
			u, err := parseAdminURL(c.AdminURL)
			if err != nil {
				u = &url.URL{Scheme: "https", Host: "invalid.invalid", Path: "/"}
			}
			out := pr.Out
			out.URL.Scheme, out.URL.Host = u.Scheme, u.Host
			out.URL.Path = u.Path + strings.TrimPrefix(pr.In.URL.Path, "/")
			out.URL.RawPath = ""
			out.Host = u.Host
			out.Header.Del("Origin")
			out.Header.Del("Referer")
			// only the panel's own cookie goes to the panel
			out.Header.Del("Cookie")
			if ck, err := pr.In.Cookie("veil_admin"); err == nil {
				out.AddCookie(ck)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			cks := resp.Cookies()
			if len(cks) > 0 {
				resp.Header.Del("Set-Cookie")
				for _, ck := range cks {
					ck.Path, ck.Secure, ck.SameSite, ck.Domain = "/", false, http.SameSiteLaxMode, ""
					resp.Header.Add("Set-Cookie", ck.String())
				}
			}
			if loc := resp.Header.Get("Location"); loc != "" {
				if c, _ := g.loadCfg(); c != nil {
					if u, err := parseAdminURL(c.AdminURL); err == nil {
						if lu, err := url.Parse(loc); err == nil && (lu.Host == "" || lu.Host == u.Host) && strings.HasPrefix(lu.Path, u.Path) {
							resp.Header.Set("Location", "/"+strings.TrimPrefix(lu.Path, u.Path))
						}
					}
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "Cannot reach the admin panel: "+err.Error()+"\n\nCheck the address in Veil > Manage server, that the server is up, and that port 8088 is reachable from here.", http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != fmt.Sprintf("127.0.0.1:%d", port) && r.Host != fmt.Sprintf("localhost:%d", port) {
			http.Error(w, "forbidden", http.StatusForbidden) // DNS-rebinding guard
			return
		}
		if t := r.URL.Query().Get("t"); t != "" && t == token && r.URL.Path == "/" {
			http.SetCookie(w, &http.Cookie{Name: "veilmng", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		if ck, err := r.Cookie("veilmng"); err != nil || ck.Value != token {
			http.Error(w, "forbidden - open the admin panel from the Veil window", http.StatusForbidden)
			return
		}
		c, err := g.loadCfg()
		if err != nil {
			http.Error(w, "no server selected", http.StatusBadRequest)
			return
		}
		proxy.Transport = adminTransport(c) // current certificate settings
		proxy.ServeHTTP(w, r)
	})
}

// manageOpen returns the local address that shows the admin panel.
func (g *gui) manageOpen(w http.ResponseWriter, r *http.Request) {
	c, err := g.loadCfg()
	if err != nil {
		jsonOut(w, 400, map[string]any{"error": "import a key first"})
		return
	}
	if _, err := parseAdminURL(c.AdminURL); err != nil {
		jsonOut(w, 400, map[string]any{"error": err.Error()})
		return
	}
	port, token, err := g.startManager()
	if err != nil {
		jsonOut(w, 500, map[string]any{"error": err.Error()})
		return
	}
	jsonOut(w, 200, map[string]any{"url": fmt.Sprintf("http://127.0.0.1:%d/?t=%s", port, token)})
}
