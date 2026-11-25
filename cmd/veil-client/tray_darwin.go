//go:build darwin

package main

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"math"
	"runtime"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

// Menu-bar item written against the Objective-C runtime through purego (no cgo, so the whole
// client still cross-compiles from Linux). Everything AppKit-related runs on the main thread.
func init() { runtime.LockOSThread() }


func nsString(s string) objc.ID {
	b := append([]byte(s), 0)
	r := objc.ID(objc.GetClass("NSString")).Send(objc.RegisterName("stringWithUTF8String:"), &b[0])
	runtime.KeepAlive(b)
	return r
}

// trayPNG draws an 18pt template icon (36px, 144 dpi): a "V" ring outline when off, a filled disc
// with the V cut out when on. Template images are tinted by macOS for light/dark menu bars.
func trayPNG(on bool) []byte {
	const n, ss = 36, 4
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	seg := func(x, y, x1, y1, x2, y2 float64) float64 {
		dx, dy := x2-x1, y2-y1
		t := ((x-x1)*dx + (y-y1)*dy) / (dx*dx + dy*dy)
		t = math.Max(0, math.Min(1, t))
		px, py := x1+t*dx-x, y1+t*dy-y
		return math.Hypot(px, py)
	}
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			cov := 0.0
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					fx := float64(x) + (float64(sx)+0.5)/ss
					fy := float64(y) + (float64(sy)+0.5)/ss
					d := math.Hypot(fx-18, fy-18)
					v := math.Min(seg(fx, fy, 10.5, 11.5, 18, 25), seg(fx, fy, 25.5, 11.5, 18, 25)) <= 2.2
					disc := d <= 16
					ring := d <= 16 && d >= 13.6
					var hit bool
					if on {
						hit = disc && !v
					} else {
						hit = ring || v
					}
					if hit {
						cov++
					}
				}
			}
			img.SetNRGBA(x, y, color.NRGBA{0, 0, 0, uint8(255 * cov / (ss * ss))})
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	b := buf.Bytes()
	// insert a pHYs chunk (144 dpi) right after IHDR so AppKit sizes it as 18x18 points
	phys := make([]byte, 9)
	binary.BigEndian.PutUint32(phys[0:], 5669)
	binary.BigEndian.PutUint32(phys[4:], 5669)
	phys[8] = 1
	chunk := make([]byte, 0, 21)
	chunk = binary.BigEndian.AppendUint32(chunk, 9)
	body := append([]byte("pHYs"), phys...)
	chunk = append(chunk, body...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(body))
	const ihdrEnd = 8 + 25
	out := append([]byte{}, b[:ihdrEnd]...)
	out = append(out, chunk...)
	return append(out, b[ihdrEnd:]...)
}

func nsImage(data []byte) objc.ID {
	nsdata := objc.ID(objc.GetClass("NSData")).Send(objc.RegisterName("dataWithBytes:length:"), unsafe.Pointer(&data[0]), uint64(len(data)))
	img := objc.ID(objc.GetClass("NSImage")).Send(objc.RegisterName("alloc"))
	img = img.Send(objc.RegisterName("initWithData:"), nsdata)
	img.Send(objc.RegisterName("setTemplate:"), true)
	runtime.KeepAlive(data)
	return img
}

func runTray(g *gui) {
	if _, err := purego.Dlopen("/System/Library/Frameworks/AppKit.framework/AppKit", purego.RTLD_NOW|purego.RTLD_GLOBAL); err != nil {
		<-g.quit // no AppKit: run without a menu-bar item
		return
	}
	sel := objc.RegisterName
	pool := func() objc.ID {
		return objc.ID(objc.GetClass("NSAutoreleasePool")).Send(sel("alloc")).Send(sel("init"))
	}
	p0 := pool()
	app := objc.ID(objc.GetClass("NSApplication")).Send(sel("sharedApplication"))
	app.Send(sel("setActivationPolicy:"), 1) // accessory: no Dock icon
	app.Send(sel("finishLaunching"))

	cls, err := objc.RegisterClass("VeilTrayTarget", objc.GetClass("NSObject"), nil, nil, []objc.MethodDef{
		{Cmd: sel("openAction:"), Fn: func(self objc.ID, _ objc.SEL, sender objc.ID) { g.showWindow() }},
		{Cmd: sel("toggleAction:"), Fn: func(self objc.ID, _ objc.SEL, sender objc.ID) { g.toggleConnect() }},
		{Cmd: sel("quitAction:"), Fn: func(self objc.ID, _ objc.SEL, sender objc.ID) { g.doQuit() }},
	})
	if err != nil {
		<-g.quit
		return
	}
	target := objc.ID(cls).Send(sel("alloc")).Send(sel("init"))

	bar := objc.ID(objc.GetClass("NSStatusBar")).Send(sel("systemStatusBar"))
	item := bar.Send(sel("statusItemWithLength:"), float64(-1)) // variable length
	item.Send(sel("retain"))
	button := item.Send(sel("button"))
	imgOff, imgOn := nsImage(trayPNG(false)), nsImage(trayPNG(true))
	button.Send(sel("setImage:"), imgOff)

	menu := objc.ID(objc.GetClass("NSMenu")).Send(sel("alloc")).Send(sel("init"))
	add := func(title string, action string) objc.ID {
		var a objc.SEL
		if action != "" {
			a = sel(action)
		}
		mi := menu.Send(sel("addItemWithTitle:action:keyEquivalent:"), nsString(title), a, nsString(""))
		if action != "" {
			mi.Send(sel("setTarget:"), target)
		}
		return mi
	}
	status := add("Veil - disconnected", "")
	toggle := add("Connect", "toggleAction:")
	add("Open Veil", "openAction:")
	menu.Send(sel("addItem:"), objc.ID(objc.GetClass("NSMenuItem")).Send(sel("separatorItem")))
	add("Quit Veil", "quitAction:")
	item.Send(sel("setMenu:"), menu)

	last := ""
	update := func() {
		s := g.eng.Status()
		label, tgl, on := "disconnected", "Connect", false
		switch s.State {
		case "connecting":
			label, tgl = "connecting…", "Cancel"
		case "connected":
			label, tgl, on = "connected", "Disconnect", true
			if !s.Carrier {
				label, on = "reconnecting…", false
			}
		case "disconnecting":
			label, tgl = "disconnecting…", "Disconnecting…"
		case "error":
			label = "error: " + s.Error
		}
		if label+tgl == last {
			return
		}
		last = label + tgl
		status.Send(sel("setTitle:"), nsString("Veil - "+label))
		toggle.Send(sel("setTitle:"), nsString(tgl))
		if on {
			button.Send(sel("setImage:"), imgOn)
		} else {
			button.Send(sel("setImage:"), imgOff)
		}
	}
	p0.Send(sel("drain"))

	mode := nsString("kCFRunLoopDefaultMode")
	nextEvent := sel("nextEventMatchingMask:untilDate:inMode:dequeue:")
	dateCls := objc.ID(objc.GetClass("NSDate"))
	lastUpd := time.Time{}
	for {
		select {
		case <-g.quit:
			bar.Send(sel("removeStatusItem:"), item)
			return
		default:
		}
		p := pool()
		if time.Since(lastUpd) > 600*time.Millisecond {
			update()
			lastUpd = time.Now()
		}
		until := dateCls.Send(sel("dateWithTimeIntervalSinceNow:"), float64(0.3))
		if ev := app.Send(nextEvent, ^uint64(0), until, mode, true); ev != 0 {
			app.Send(sel("sendEvent:"), ev)
		}
		p.Send(sel("drain"))
	}
}
