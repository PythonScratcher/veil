//go:build nativeui

package main

import (
	"image"
	"image/png"
	"os"
	"testing"
	"time"

	"gioui.org/gpu/headless"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"

	"veil/proto"
)

func render(t *testing.T, u *nativeUI, name string, h int) {
	const scale = 2
	w := 460 * scale
	win, err := headless.NewWindow(w, h*scale)
	if err != nil {
		t.Fatal(err)
	}
	var ops op.Ops
	for i := 0; i < 2; i++ { // second frame: widgets filled from state
		ops.Reset()
		gtx := layout.Context{Ops: &ops, Metric: unit.Metric{PxPerDp: scale, PxPerSp: scale}, Constraints: layout.Exact(image.Pt(w, h*scale)), Now: time.Now()}
		u.Layout(gtx)
		win.Frame(&ops)
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h*scale))
	win.Screenshot(img)
	f, _ := os.Create("/tmp/prev/" + name + ".png")
	png.Encode(f, img)
	f.Close()
}

func TestPreview(t *testing.T) {
	os.MkdirAll("/tmp/prev", 0755)
	g := &gui{dir: t.TempDir(), logs: &logRing{}, eng: NewEngine("")}
	g.h = g.routes()
	u := newNativeUI(g)
	sv := settingsView{Carrier: "tls", Transport: "stream", Frag: "both", Rotate: 20, Cover: true, DNS: "1.1.1.1", AdminURL: "https://144.21.58.137:8088/console-d07218617b/"}
	base := uiState{Admin: true, Windows: true, Version: "1.7.0", Selected: "a",
		Profiles: []uiProfile{{"a", "Corvane VPS", "corvane.com.sh:443"}, {"b", "Backup", "144.21.58.137:443"}}, Settings: &sv}
	base.Profile = &struct {
		Name      string `json:"name"`
		Server    string `json:"server"`
		SNI       string `json:"sni"`
		LocalCert bool   `json:"local_cert"`
	}{"Corvane VPS", "corvane.com.sh:443", "www.corvane.com", true}
	st := base
	st.Engine = EngineStatus{State: "connected", Carrier: true, Since: time.Now().Add(-83 * time.Minute).Unix(), Up: 48 << 20, Down: 1300 << 20,
		Limits: &proto.AccountLimits{ExpiresAt: time.Now().Add(12*24*time.Hour + 5*time.Hour).Unix(), DataCap: 50 << 30, UsedDown: 31 << 30, UsedUp: 2 << 30, RateMbps: 50}}
	u.st, u.loaded, u.downRate, u.upRate = st, true, 2.6e6, 180e3
	render(t, u, "connected", 800)
	st2 := base
	st2.Engine = EngineStatus{State: "disconnected"}
	u.st, u.downRate, u.upRate = st2, 0, 0
	u.secSettings.open, u.secManage.open = true, true
	render(t, u, "settings", 1500)
	u.secSettings.open, u.secManage.open = false, false
	u.pickerOpen = true
	st3 := base
	st3.Engine = EngineStatus{State: "error", Error: "cannot reach server: dial tcp 1.2.3.4:443: i/o timeout"}
	u.st = st3
	render(t, u, "picker", 820)
	u.pickerOpen = false
	u.importing = true
	render(t, u, "import", 620)
}
