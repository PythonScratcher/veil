package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"
)

// call runs one request against the app's own API in-process, so a native window shares every
// check and side effect with the browser UI instead of duplicating the logic.
func (g *gui) call(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, "/api"+path, rd)
	r.Host = fmt.Sprintf("127.0.0.1:%d", g.port)
	r.RemoteAddr = "127.0.0.1:1"
	r.AddCookie(&http.Cookie{Name: "veilgui", Value: g.token})
	r.Header.Set("X-Veil", "1")
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	g.h.ServeHTTP(rec, r)
	if rec.Code >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(rec.Body.Bytes(), &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return errors.New(strings.TrimSpace(rec.Body.String()))
	}
	if out != nil {
		return json.Unmarshal(rec.Body.Bytes(), out)
	}
	return nil
}

// showRemote asks an already-running copy of Veil to show its window.
func showRemote(gi guiInfo) bool {
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/api/show", gi.Port), nil)
	req.Header.Set("X-Veil", "1")
	req.AddCookie(&http.Cookie{Name: "veilgui", Value: gi.Token})
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// nativeWindow, when set (Windows), replaces the browser window with a native one.
var nativeWindow func(g *gui)
