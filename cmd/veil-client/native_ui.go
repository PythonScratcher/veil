//go:build windows || nativeui

package main

// Native window (Gio) for the Windows app. It drives the same in-process API as the browser UI
// (gui.call), so behaviour and validation are identical; only the presentation is native.

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gioui.org/f32"
	"gioui.org/font"
	"gioui.org/font/gofont"
	"gioui.org/font/opentype"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

type (
	C = layout.Context
	D = layout.Dimensions
)

var (
	cBg     = rgb(0x0f1117)
	cPanel  = rgb(0x171a23)
	cPanel2 = rgb(0x1e2230)
	cLine   = rgb(0x2a2f42)
	cText   = rgb(0xe7e9f1)
	cMute   = rgb(0x8d93ab)
	cAccent = rgb(0xa78bfa)
	cAcc2   = rgb(0x7c5cf0)
	cOK     = rgb(0x4ade80)
	cWarn   = rgb(0xfbbf24)
	cBad    = rgb(0xf87171)
)

func rgb(v uint32) color.NRGBA {
	return color.NRGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 255}
}
func alpha(c color.NRGBA, a uint8) color.NRGBA { c.A = a; return c }

type uiProfile struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Server string `json:"server"`
}

type uiState struct {
	Engine   EngineStatus `json:"engine"`
	Admin    bool         `json:"admin"`
	Windows  bool         `json:"windows"`
	Version  string       `json:"version"`
	Profiles []uiProfile  `json:"profiles"`
	Selected string       `json:"selected"`
	Profile  *struct {
		Name      string `json:"name"`
		Server    string `json:"server"`
		SNI       string `json:"sni"`
		LocalCert bool   `json:"local_cert"`
	} `json:"profile"`
	Settings *settingsView `json:"settings"`
}

type section struct {
	hdr  widget.Clickable
	open bool
}

type nativeUI struct {
	g  *gui
	th *material.Theme
	// invalidate asks for a new frame (set by the window owner)
	invalidate func()

	mu       sync.Mutex
	st       uiState
	loaded   bool
	upRate   float64
	downRate float64
	lastUp   int64
	lastDown int64
	lastT    time.Time
	toast    string
	toastAt  time.Time
	busy     map[string]bool

	list widget.List

	power, addSrv, pickSrv, elevate        widget.Clickable
	refreshLim, repair, quit, webui         widget.Clickable
	saveSet, openAdmin, rename, remove      widget.Clickable
	impAdd, impFile, impCancel             widget.Clickable
	srvRows                                []widget.Clickable
	pickerOpen, importing, removeArm       bool
	secSettings, secManage, secServer      section
	carrier, transport, frag               widget.Enum
	cover, bounce, adminTrust              widget.Bool
	rotate, dns, adminURL, nameEd          widget.Editor
	impText, impName                       widget.Editor
	setDirty, mngDirty                     bool
	filledFor                              string
}

func newNativeUI(g *gui) *nativeUI {
	u := &nativeUI{g: g, busy: map[string]bool{}}
	th := material.NewTheme()
	th.Shaper = text.NewShaper(text.WithCollection(uiFonts()))
	th.Palette = material.Palette{Bg: cBg, Fg: cText, ContrastBg: cAcc2, ContrastFg: rgb(0xffffff)}
	th.TextSize = unit.Sp(14)
	u.th = th
	u.list.Axis = layout.Vertical
	for _, e := range []*widget.Editor{&u.rotate, &u.dns, &u.adminURL, &u.nameEd, &u.impName} {
		e.SingleLine = true
	}
	u.rotate.Filter = "-0123456789"
	u.impText.SingleLine = false
	u.secSettings.open = false
	return u
}

// uiFonts prefers Segoe UI (what the rest of Windows uses) and falls back to the Go fonts.
func uiFonts() []font.FontFace {
	var out []font.FontFace
	dir := filepath.Join(os.Getenv("WINDIR"), "Fonts")
	for _, f := range []string{"segoeui.ttf", "seguisb.ttf", "segoeuib.ttf"} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			continue
		}
		if faces, err := opentype.ParseCollection(b); err == nil {
			out = append(out, faces...)
		}
	}
	return append(out, gofont.Collection()...)
}

// ---------- data

func (u *nativeUI) poll() {
	var st uiState
	if err := u.g.call("GET", "/state", nil, &st); err != nil {
		return
	}
	u.mu.Lock()
	now := time.Now()
	if !u.lastT.IsZero() && st.Engine.State == "connected" {
		dt := now.Sub(u.lastT).Seconds()
		if dt > 0.2 {
			u.upRate = 0.6*u.upRate + 0.4*float64(max64(0, st.Engine.Up-u.lastUp))/dt
			u.downRate = 0.6*u.downRate + 0.4*float64(max64(0, st.Engine.Down-u.lastDown))/dt
		}
	} else if st.Engine.State != "connected" {
		u.upRate, u.downRate = 0, 0
	}
	u.lastUp, u.lastDown, u.lastT = st.Engine.Up, st.Engine.Down, now
	u.st, u.loaded = st, true
	u.mu.Unlock()
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (u *nativeUI) say(msg string) {
	u.mu.Lock()
	u.toast, u.toastAt = msg, time.Now()
	u.mu.Unlock()
	if u.invalidate != nil {
		u.invalidate()
	}
}

// do runs an API call off the UI thread, shows errors as a toast, then refreshes.
func (u *nativeUI) do(key, method, path string, body any, okMsg string, after func()) {
	u.mu.Lock()
	if u.busy[key] {
		u.mu.Unlock()
		return
	}
	u.busy[key] = true
	u.mu.Unlock()
	go func() {
		err := u.g.call(method, path, body, nil)
		u.mu.Lock()
		delete(u.busy, key)
		u.mu.Unlock()
		if err != nil {
			u.say(err.Error())
		} else {
			if okMsg != "" {
				u.say(okMsg)
			}
			if after != nil {
				after()
			}
		}
		u.poll()
		if u.invalidate != nil {
			u.invalidate()
		}
	}()
}

func (u *nativeUI) isBusy(k string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.busy[k]
}

// ---------- events

func (u *nativeUI) update(gtx C, st uiState) {
	eng := st.Engine
	if u.power.Clicked(gtx) {
		switch eng.State {
		case "connected", "connecting":
			u.do("power", "POST", "/disconnect", nil, "", nil)
		case "disconnecting":
		default:
			if st.Profile == nil {
				u.importing = true
			} else {
				u.do("power", "POST", "/connect", nil, "", nil)
			}
		}
	}
	if u.elevate.Clicked(gtx) {
		u.do("elevate", "POST", "/elevate", nil, "", nil)
	}
	if u.pickSrv.Clicked(gtx) {
		u.pickerOpen = !u.pickerOpen
	}
	for i := range u.srvRows {
		if i < len(st.Profiles) && u.srvRows[i].Clicked(gtx) {
			u.pickerOpen = false
			if st.Profiles[i].ID != st.Selected {
				u.setDirty, u.mngDirty = false, false
				u.do("select", "POST", "/select", map[string]string{"id": st.Profiles[i].ID}, "", nil)
			}
		}
	}
	if u.addSrv.Clicked(gtx) {
		u.pickerOpen, u.importing = false, true
		u.impText.SetText("")
		u.impName.SetText("")
	}
	if u.impCancel.Clicked(gtx) {
		u.importing = false
	}
	if u.impFile.Clicked(gtx) {
		go func() {
			p := pickFile()
			if p == "" {
				return
			}
			b, err := os.ReadFile(p)
			if err != nil || len(b) > 1<<20 {
				u.say("could not read that file")
				return
			}
			u.impText.SetText(string(b))
			if u.invalidate != nil {
				u.invalidate()
			}
		}()
	}
	if u.impAdd.Clicked(gtx) {
		body := map[string]string{"text": u.impText.Text(), "name": strings.TrimSpace(u.impName.Text())}
		u.do("import", "POST", "/import", body, "Server added", func() { u.importing = false })
	}
	if u.refreshLim.Clicked(gtx) {
		u.do("limits", "POST", "/refresh-limits", nil, "Limits updated", nil)
	}
	if u.repair.Clicked(gtx) {
		u.do("repair", "POST", "/repair", nil, "Network settings cleaned up", nil)
	}
	if u.quit.Clicked(gtx) {
		u.do("quit", "POST", "/quit", nil, "", nil)
	}
	if u.webui.Clicked(gtx) {
		openWindow(fmt.Sprintf("http://127.0.0.1:%d/?t=%s", u.g.port, u.g.token), u.g.dir)
	}
	for _, s := range []*section{&u.secSettings, &u.secManage, &u.secServer} {
		if s.hdr.Clicked(gtx) {
			s.open = !s.open
		}
	}
	// settings widgets
	for _, e := range []*widget.Enum{&u.carrier, &u.transport, &u.frag} {
		if e.Update(gtx) {
			u.setDirty = true
		}
	}
	for _, b := range []*widget.Bool{&u.cover, &u.bounce} {
		if b.Update(gtx) {
			u.setDirty = true
		}
	}
	if u.adminTrust.Update(gtx) {
		u.mngDirty = true
	}
	for _, e := range []*widget.Editor{&u.rotate, &u.dns} {
		for {
			ev, ok := e.Update(gtx)
			if !ok {
				break
			}
			if _, ok := ev.(widget.ChangeEvent); ok {
				u.setDirty = true
			}
		}
	}
	for {
		ev, ok := u.adminURL.Update(gtx)
		if !ok {
			break
		}
		if _, ok := ev.(widget.ChangeEvent); ok {
			u.mngDirty = true
		}
	}
	if u.saveSet.Clicked(gtx) && st.Settings != nil {
		u.do("save", "POST", "/settings", u.readSettings(*st.Settings), "Saved - applies the next time you connect", func() { u.setDirty = false })
	}
	if u.openAdmin.Clicked(gtx) && st.Settings != nil {
		body := u.readSettings(*st.Settings)
		go func() {
			if err := u.g.call("POST", "/settings", body, nil); err != nil {
				u.say(err.Error())
				return
			}
			u.mngDirty = false
			var r struct {
				URL string `json:"url"`
			}
			if err := u.g.call("POST", "/manage/open", nil, &r); err != nil {
				u.say(err.Error())
				return
			}
			openBrowser(r.URL)
			u.say("Admin panel opened in your browser")
		}()
	}
	if u.rename.Clicked(gtx) {
		u.do("rename", "POST", "/rename", map[string]string{"id": st.Selected, "name": strings.TrimSpace(u.nameEd.Text())}, "Renamed", nil)
	}
	if u.remove.Clicked(gtx) {
		if !u.removeArm {
			u.removeArm = true
		} else {
			u.removeArm = false
			u.do("remove", "POST", "/remove", map[string]string{"id": st.Selected}, "Server removed", nil)
		}
	}
	// fill the form from the saved settings unless the user is editing it
	if st.Settings != nil {
		key := st.Selected
		s := st.Settings
		if !u.setDirty || u.filledFor != key {
			u.carrier.Value, u.transport.Value, u.frag.Value = s.Carrier, s.Transport, s.Frag
			u.cover.Value, u.bounce.Value = s.Cover, s.NetBounce
			setText(&u.rotate, strconv.Itoa(s.Rotate))
			setText(&u.dns, s.DNS)
			u.setDirty = false
		}
		if !u.mngDirty || u.filledFor != key {
			setText(&u.adminURL, s.AdminURL)
			u.adminTrust.Value = s.AdminTrustAny
			u.mngDirty = false
		}
		if u.filledFor != key && st.Profile != nil {
			setText(&u.nameEd, st.Profile.Name)
			u.removeArm = false
		}
		u.filledFor = key
	}
}

func setText(e *widget.Editor, s string) {
	if e.Text() != s {
		e.SetText(s)
	}
}

func (u *nativeUI) readSettings(base settingsView) settingsView {
	v := base
	v.Carrier, v.Transport, v.Frag = u.carrier.Value, u.transport.Value, u.frag.Value
	v.Rotate, _ = strconv.Atoi(strings.TrimSpace(u.rotate.Text()))
	v.DNS = strings.TrimSpace(u.dns.Text())
	v.Cover, v.NetBounce = u.cover.Value, u.bounce.Value
	v.AdminURL, v.AdminTrustAny = strings.TrimSpace(u.adminURL.Text()), u.adminTrust.Value
	return v
}

// ---------- layout

func (u *nativeUI) Layout(gtx C) D {
	u.mu.Lock()
	st := u.st
	loaded := u.loaded
	toast, toastAt := u.toast, u.toastAt
	up, down := u.upRate, u.downRate
	u.mu.Unlock()
	if len(u.srvRows) < len(st.Profiles) {
		u.srvRows = make([]widget.Clickable, len(st.Profiles)+4)
	}
	u.update(gtx, st)
	if st.Profile == nil && loaded && len(st.Profiles) == 0 {
		u.importing = true
	}

	paint.Fill(gtx.Ops, cBg)
	var rows []layout.Widget
	rows = append(rows, func(gtx C) D { return u.header(gtx, st) })
	if loaded && st.Windows && !st.Admin {
		rows = append(rows, func(gtx C) D { return u.adminBanner(gtx) })
	}
	if u.importing {
		rows = append(rows, func(gtx C) D { return u.importCard(gtx, len(st.Profiles) > 0) })
	} else {
		rows = append(rows,
			func(gtx C) D { return u.serverPicker(gtx, st) },
			func(gtx C) D { return u.hero(gtx, st) },
			func(gtx C) D { return u.stats(gtx, st, up, down) },
		)
		if st.Engine.Limits != nil {
			rows = append(rows, func(gtx C) D { return u.limits(gtx, st.Engine.Limits) })
		}
		if st.Settings != nil {
			rows = append(rows,
				func(gtx C) D { return u.collapsible(gtx, &u.secSettings, "Settings", u.settingsBody) },
				func(gtx C) D { return u.collapsible(gtx, &u.secManage, "Manage server (admin)", u.manageBody) },
				func(gtx C) D {
					return u.collapsible(gtx, &u.secServer, "This server", func(gtx C) D { return u.serverBody(gtx, st) })
				},
			)
		}
	}
	rows = append(rows, func(gtx C) D { return u.footer(gtx, st) })

	d := layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx C) D {
			return material.List(u.th, &u.list).Layout(gtx, len(rows), func(gtx C, i int) D {
				return layout.Inset{Left: unit.Dp(18), Right: unit.Dp(18), Top: unit.Dp(gap(i)), Bottom: unit.Dp(lastPad(i, len(rows)))}.Layout(gtx, rows[i])
			})
		}),
		layout.Stacked(func(gtx C) D { return D{Size: gtx.Constraints.Max} }),
	)
	if toast != "" && time.Since(toastAt) < 4*time.Second {
		u.toastView(gtx, toast)
		gtx.Execute(op.InvalidateCmd{At: toastAt.Add(4 * time.Second)})
	}
	if st.Engine.State == "connecting" || st.Engine.State == "disconnecting" || (st.Engine.State == "connected" && !st.Engine.Carrier) {
		gtx.Execute(op.InvalidateCmd{At: gtx.Now.Add(40 * time.Millisecond)}) // pulse animation
	}
	return d
}

func gap(i int) float32 {
	if i == 0 {
		return 14
	}
	return 12
}
func lastPad(i, n int) float32 {
	if i == n-1 {
		return 18
	}
	return 0
}

// card draws a rounded panel behind w.
func card(gtx C, bg, border color.NRGBA, w layout.Widget) D {
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx C) D {
			sz := gtx.Constraints.Min
			r := gtx.Dp(12)
			paint.FillShape(gtx.Ops, border, clip.UniformRRect(image.Rectangle{Max: sz}, r).Op(gtx.Ops))
			b := gtx.Dp(1)
			paint.FillShape(gtx.Ops, bg, clip.UniformRRect(image.Rect(b, b, sz.X-b, sz.Y-b), r-b).Op(gtx.Ops))
			return D{Size: sz}
		}),
		layout.Stacked(func(gtx C) D {
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			return layout.UniformInset(unit.Dp(14)).Layout(gtx, w)
		}),
	)
}

func (u *nativeUI) label(size float32, txt string, c color.NRGBA, w font.Weight) material.LabelStyle {
	l := material.Label(u.th, unit.Sp(size), txt)
	l.Color, l.Font.Weight = c, w
	return l
}

func stateInfo(e EngineStatus) (label string, col color.NRGBA) {
	switch e.State {
	case "connected":
		if !e.Carrier {
			return "Reconnecting", cWarn
		}
		return "Connected", cOK
	case "connecting":
		return "Connecting", cWarn
	case "disconnecting":
		return "Disconnecting", cWarn
	case "error":
		return "Error", cBad
	}
	return "Disconnected", cMute
}

func (u *nativeUI) header(gtx C, st uiState) D {
	lbl, col := stateInfo(st.Engine)
	return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx C) D {
			sz := gtx.Dp(26)
			drawLogo(gtx, sz)
			return D{Size: image.Pt(sz, sz)}
		}),
		layout.Rigid(layout.Spacer{Width: unit.Dp(9)}.Layout),
		layout.Flexed(1, u.label(20, "Veil", cText, font.Bold).Layout),
		layout.Rigid(func(gtx C) D { return u.pill(gtx, lbl, col) }),
	)
}

// drawLogo: the app icon (purple rounded square with a white V), drawn as vectors.
func drawLogo(gtx C, sz int) {
	r := sz * 26 / 100
	paint.FillShape(gtx.Ops, rgb(0x7656ee), clip.UniformRRect(image.Rectangle{Max: image.Pt(sz, sz)}, r).Op(gtx.Ops))
	f := float32(sz)
	var p clip.Path
	p.Begin(gtx.Ops)
	p.MoveTo(pt(f*0.29, f*0.31))
	p.LineTo(pt(f*0.5, f*0.72))
	p.LineTo(pt(f*0.71, f*0.31))
	paint.FillShape(gtx.Ops, rgb(0xffffff), clip.Stroke{Path: p.End(), Width: f * 0.125}.Op())
}

func (u *nativeUI) pill(gtx C, txt string, col color.NRGBA) D {
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx C) D {
			sz := gtx.Constraints.Min
			paint.FillShape(gtx.Ops, alpha(col, 60), clip.UniformRRect(image.Rectangle{Max: sz}, sz.Y/2).Op(gtx.Ops))
			paint.FillShape(gtx.Ops, cPanel, clip.UniformRRect(image.Rect(1, 1, sz.X-1, sz.Y-1), sz.Y/2-1).Op(gtx.Ops))
			return D{Size: sz}
		}),
		layout.Stacked(func(gtx C) D {
			return layout.Inset{Left: unit.Dp(10), Right: unit.Dp(11), Top: unit.Dp(4), Bottom: unit.Dp(4)}.Layout(gtx, func(gtx C) D {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx C) D {
						d := gtx.Dp(8)
						paint.FillShape(gtx.Ops, col, clip.Ellipse{Max: image.Pt(d, d)}.Op(gtx.Ops))
						return D{Size: image.Pt(d, d)}
					}),
					layout.Rigid(layout.Spacer{Width: unit.Dp(7)}.Layout),
					layout.Rigid(u.label(12, txt, col, font.Medium).Layout),
				)
			})
		}),
	)
}

func (u *nativeUI) adminBanner(gtx C) D {
	return card(gtx, rgb(0x2a2414), rgb(0x6b5a1f), func(gtx C) D {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(u.label(13, "Veil needs Administrator rights to create the VPN adapter.", cWarn, font.Normal).Layout),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
			layout.Rigid(func(gtx C) D { return u.btn(gtx, &u.elevate, "Restart as Administrator", true) }),
		)
	})
}

func (u *nativeUI) btn(gtx C, c *widget.Clickable, txt string, primary bool) D {
	bg, fg := cPanel2, cText
	if primary {
		bg, fg = cAcc2, rgb(0xffffff)
	}
	return u.btnColors(gtx, c, txt, bg, fg)
}

func (u *nativeUI) btnColors(gtx C, c *widget.Clickable, txt string, bg, fg color.NRGBA) D {
	return c.Layout(gtx, func(gtx C) D {
		pointer.CursorPointer.Add(gtx.Ops)
		b := bg
		if c.Hovered() {
			b = mix(bg, rgb(0xffffff), 0.12)
		}
		if !gtx.Enabled() {
			b, fg = cPanel2, cMute
		}
		return layout.Stack{Alignment: layout.Center}.Layout(gtx,
			layout.Expanded(func(gtx C) D {
				paint.FillShape(gtx.Ops, b, clip.UniformRRect(image.Rectangle{Max: gtx.Constraints.Min}, gtx.Dp(9)).Op(gtx.Ops))
				return D{Size: gtx.Constraints.Min}
			}),
			layout.Stacked(func(gtx C) D {
				return layout.Inset{Top: unit.Dp(9), Bottom: unit.Dp(9), Left: unit.Dp(15), Right: unit.Dp(15)}.Layout(gtx, func(gtx C) D {
					return layout.Center.Layout(gtx, u.label(13.5, txt, fg, font.SemiBold).Layout)
				})
			}),
		)
	})
}

func mix(a, b color.NRGBA, t float32) color.NRGBA {
	f := func(x, y uint8) uint8 { return uint8(float32(x)*(1-t) + float32(y)*t) }
	return color.NRGBA{f(a.R, b.R), f(a.G, b.G), f(a.B, b.B), 255}
}

func (u *nativeUI) link(gtx C, c *widget.Clickable, txt string, col color.NRGBA) D {
	return c.Layout(gtx, func(gtx C) D {
		pointer.CursorPointer.Add(gtx.Ops)
		cc := col
		if c.Hovered() {
			cc = cText
		}
		return layout.UniformInset(unit.Dp(4)).Layout(gtx, u.label(13, txt, cc, font.Normal).Layout)
	})
}

func (u *nativeUI) serverPicker(gtx C, st uiState) D {
	name, addr := "No server", "Add a server to start"
	if st.Profile != nil {
		name, addr = st.Profile.Name, st.Profile.Server
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx C) D {
			return u.pickSrv.Layout(gtx, func(gtx C) D {
				pointer.CursorPointer.Add(gtx.Ops)
				border := cLine
				if u.pickSrv.Hovered() || u.pickerOpen {
					border = cAcc2
				}
				return card(gtx, cPanel, border, func(gtx C) D {
					return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
						layout.Rigid(func(gtx C) D { return globe(gtx, gtx.Dp(30)) }),
						layout.Rigid(layout.Spacer{Width: unit.Dp(12)}.Layout),
						layout.Flexed(1, func(gtx C) D {
							return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
								layout.Rigid(u.label(15, name, cText, font.SemiBold).Layout),
								layout.Rigid(u.label(12.5, addr, cMute, font.Normal).Layout),
							)
						}),
						layout.Rigid(func(gtx C) D { return chevron(gtx, u.pickerOpen, cMute) }),
					)
				})
			})
		}),
		layout.Rigid(func(gtx C) D {
			if !u.pickerOpen {
				return D{}
			}
			return layout.Inset{Top: unit.Dp(6)}.Layout(gtx, func(gtx C) D {
				return card(gtx, cPanel, cLine, func(gtx C) D {
					var kids []layout.FlexChild
					for i, p := range st.Profiles {
						i, p := i, p
						kids = append(kids, layout.Rigid(func(gtx C) D {
							return u.srvRows[i].Layout(gtx, func(gtx C) D {
								pointer.CursorPointer.Add(gtx.Ops)
								sel := p.ID == st.Selected
								return layout.Stack{}.Layout(gtx,
									layout.Expanded(func(gtx C) D {
										if sel || u.srvRows[i].Hovered() {
											c := cPanel2
											if sel {
												c = rgb(0x221e3d)
											}
											paint.FillShape(gtx.Ops, c, clip.UniformRRect(image.Rectangle{Max: gtx.Constraints.Min}, gtx.Dp(8)).Op(gtx.Ops))
										}
										return D{Size: gtx.Constraints.Min}
									}),
									layout.Stacked(func(gtx C) D {
										gtx.Constraints.Min.X = gtx.Constraints.Max.X
										return layout.UniformInset(unit.Dp(9)).Layout(gtx, func(gtx C) D {
											return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
												layout.Flexed(1, func(gtx C) D {
													return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
														layout.Rigid(u.label(14, p.Name, cText, font.Medium).Layout),
														layout.Rigid(u.label(12, p.Server, cMute, font.Normal).Layout),
													)
												}),
												layout.Rigid(func(gtx C) D {
													if !sel {
														return D{}
													}
													return u.label(12, "Selected", cAccent, font.Medium).Layout(gtx)
												}),
											)
										})
									}),
								)
							})
						}))
					}
					kids = append(kids, layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
						layout.Rigid(func(gtx C) D { return u.btn(gtx, &u.addSrv, "+  Add server", false) }))
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
				})
			})
		}),
	)
}

type f32Point = f32.Point

func pt(x, y float32) f32Point { return f32Point{X: x, Y: y} }

// globe: small round server badge.
func globe(gtx C, sz int) D {
	paint.FillShape(gtx.Ops, rgb(0x221e3d), clip.Ellipse{Max: image.Pt(sz, sz)}.Op(gtx.Ops))
	f := float32(sz)
	c := f / 2
	stroke := func(build func(p *clip.Path)) {
		var p clip.Path
		p.Begin(gtx.Ops)
		build(&p)
		paint.FillShape(gtx.Ops, cAccent, clip.Stroke{Path: p.End(), Width: f * 0.06}.Op())
	}
	rr := f * 0.3
	stroke(func(p *clip.Path) { arcPath(p, c, c, rr, 0, 2*math.Pi) })
	stroke(func(p *clip.Path) { p.MoveTo(pt(c-rr, c)); p.LineTo(pt(c+rr, c)) })
	stroke(func(p *clip.Path) { ellipsePath(p, c, c, rr*0.45, rr) })
	return D{Size: image.Pt(sz, sz)}
}

func arcPath(p *clip.Path, cx, cy, r float32, a0, a1 float64) {
	const n = 48
	for i := 0; i <= n; i++ {
		a := a0 + (a1-a0)*float64(i)/n
		q := pt(cx+r*float32(math.Sin(a)), cy-r*float32(math.Cos(a)))
		if i == 0 {
			p.MoveTo(q)
		} else {
			p.LineTo(q)
		}
	}
}

func ellipsePath(p *clip.Path, cx, cy, rx, ry float32) {
	const n = 48
	for i := 0; i <= n; i++ {
		a := 2 * math.Pi * float64(i) / n
		q := pt(cx+rx*float32(math.Sin(a)), cy-ry*float32(math.Cos(a)))
		if i == 0 {
			p.MoveTo(q)
		} else {
			p.LineTo(q)
		}
	}
}

func chevron(gtx C, open bool, col color.NRGBA) D {
	sz := gtx.Dp(14)
	f := float32(sz)
	var p clip.Path
	p.Begin(gtx.Ops)
	if open {
		p.MoveTo(pt(f*0.2, f*0.65))
		p.LineTo(pt(f*0.5, f*0.35))
		p.LineTo(pt(f*0.8, f*0.65))
	} else {
		p.MoveTo(pt(f*0.2, f*0.35))
		p.LineTo(pt(f*0.5, f*0.65))
		p.LineTo(pt(f*0.8, f*0.35))
	}
	paint.FillShape(gtx.Ops, col, clip.Stroke{Path: p.End(), Width: f * 0.13}.Op())
	return D{Size: image.Pt(sz, sz)}
}

func (u *nativeUI) hero(gtx C, st uiState) D {
	e := st.Engine
	lbl, col := stateInfo(e)
	sub := ""
	switch e.State {
	case "connected":
		if e.Since > 0 {
			sub = "Connected for " + fmtDur(time.Since(time.Unix(e.Since, 0)))
		}
		if !e.Carrier {
			sub = "Lost the server, reconnecting automatically…"
		}
	case "connecting":
		sub = "Setting up the tunnel…"
	case "disconnected":
		sub = "Tap to connect"
		if st.Profile == nil {
			sub = "Add a server first"
		}
	}
	return layout.Flex{Axis: layout.Vertical, Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx C) D {
			return layout.Center.Layout(gtx, func(gtx C) D { return u.powerButton(gtx, e, col) })
		}),
		layout.Rigid(func(gtx C) D {
			l := u.label(22, lbl, cText, font.SemiBold)
			if e.State == "connected" && e.Carrier {
				l.Color = cOK
			}
			l.Alignment = text.Middle
			return layout.Center.Layout(gtx, l.Layout)
		}),
		layout.Rigid(func(gtx C) D {
			l := u.label(13, sub, cMute, font.Normal)
			l.Alignment = text.Middle
			return layout.Center.Layout(gtx, l.Layout)
		}),
		layout.Rigid(func(gtx C) D {
			if e.State != "error" || e.Error == "" {
				return D{}
			}
			return layout.Inset{Top: unit.Dp(10)}.Layout(gtx, func(gtx C) D {
				return card(gtx, rgb(0x2a1618), rgb(0x6b2a2a), u.label(13, e.Error, cBad, font.Normal).Layout)
			})
		}),
	)
}

func (u *nativeUI) powerButton(gtx C, e EngineStatus, col color.NRGBA) D {
	sz := gtx.Dp(136)
	m := gtx.Dp(26)
	return u.power.Layout(gtx, func(gtx C) D {
		pointer.CursorPointer.Add(gtx.Ops)
		defer op.Offset(image.Pt(m, m)).Push(gtx.Ops).Pop()
		f := float32(sz)
		on := e.State == "connected" && e.Carrier
		pulse := e.State == "connecting" || e.State == "disconnecting" || (e.State == "connected" && !e.Carrier)
		ring := cLine
		fill := cPanel
		icon := cMute
		if u.power.Hovered() {
			ring, icon = cAccent, cText
		}
		switch {
		case on:
			ring, fill, icon = cOK, rgb(0x163626), rgb(0xffffff)
			// glow
			for i := 16; i >= 1; i-- {
				g := gtx.Dp(unit.Dp(float32(i) * 1.5))
				paint.FillShape(gtx.Ops, alpha(cOK, uint8(5)), clip.Ellipse{Min: image.Pt(-g, -g), Max: image.Pt(sz+g, sz+g)}.Op(gtx.Ops))
			}
		case pulse:
			a := 0.55 + 0.45*math.Sin(float64(gtx.Now.UnixMilli())/180)
			ring, icon = alpha(cWarn, uint8(120+135*a)), cWarn
		case e.State == "error":
			ring, icon = cBad, cBad
		}
		bw := gtx.Dp(2)
		paint.FillShape(gtx.Ops, ring, clip.Ellipse{Max: image.Pt(sz, sz)}.Op(gtx.Ops))
		paint.FillShape(gtx.Ops, fill, clip.Ellipse{Min: image.Pt(bw, bw), Max: image.Pt(sz-bw, sz-bw)}.Op(gtx.Ops))
		// power glyph
		c, r := f/2, f*0.2
		var p clip.Path
		p.Begin(gtx.Ops)
		arcPath(&p, c, c+f*0.02, r, 0.75, 2*math.Pi-0.75)
		paint.FillShape(gtx.Ops, icon, clip.Stroke{Path: p.End(), Width: f * 0.045}.Op())
		var l clip.Path
		l.Begin(gtx.Ops)
		l.MoveTo(pt(c, c-r*1.15))
		l.LineTo(pt(c, c-r*0.15))
		paint.FillShape(gtx.Ops, icon, clip.Stroke{Path: l.End(), Width: f * 0.045}.Op())
		return D{Size: image.Pt(sz+2*m, sz+2*m)}
	})
}

func (u *nativeUI) stats(gtx C, st uiState, up, down float64) D {
	box := func(title string, total int64, rate float64, col color.NRGBA) layout.Widget {
		return func(gtx C) D {
			return card(gtx, cPanel, cLine, func(gtx C) D {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(u.label(12, title, cMute, font.Normal).Layout),
					layout.Rigid(u.label(17, fmtRate(rate), col, font.SemiBold).Layout),
					layout.Rigid(u.label(12, fmtBytes(total)+" total", cMute, font.Normal).Layout),
				)
			})
		}
	}
	return layout.Flex{}.Layout(gtx,
		layout.Flexed(1, box("Download", st.Engine.Down, down, cText)),
		layout.Rigid(layout.Spacer{Width: unit.Dp(10)}.Layout),
		layout.Flexed(1, box("Upload", st.Engine.Up, up, cText)),
	)
}

func (u *nativeUI) kv(gtx C, k, v string, vc color.NRGBA) D {
	return layout.Inset{Top: unit.Dp(3), Bottom: unit.Dp(3)}.Layout(gtx, func(gtx C) D {
		return layout.Flex{}.Layout(gtx,
			layout.Flexed(1, u.label(13, k, cMute, font.Normal).Layout),
			layout.Rigid(u.label(13, v, vc, font.Medium).Layout),
		)
	})
}

