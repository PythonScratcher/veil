package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"time"

	"fyne.io/systray"
)

// ico wraps a PNG into a single-image .ico (supported by Windows Vista+).
func ico(c color.RGBA, ring color.RGBA) []byte {
	const n = 32
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			dx, dy := float64(x)-15.5, float64(y)-15.5
			d := dx*dx + dy*dy
			switch {
			case d <= 12*12:
				img.Set(x, y, c)
			case d <= 15*15:
				img.Set(x, y, ring)
			}
		}
	}
	// a white "V": two thick strokes meeting at the bottom
	w := color.RGBA{255, 255, 255, 255}
	seg := func(x, y, x1, y1, x2, y2 float64) float64 {
		dx, dy := x2-x1, y2-y1
		t := ((x-x1)*dx + (y-y1)*dy) / (dx*dx + dy*dy)
		if t < 0 {
			t = 0
		} else if t > 1 {
			t = 1
		}
		px, py := x1+t*dx-x, y1+t*dy-y
		return px*px + py*py
	}
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			if seg(fx, fy, 8, 9, 16, 23) <= 2.6*2.6 || seg(fx, fy, 24, 9, 16, 23) <= 2.6*2.6 {
				img.Set(x, y, w)
			}
		}
	}
	var pb bytes.Buffer
	png.Encode(&pb, img)
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, [3]uint16{0, 1, 1})
	b.Write([]byte{n, n, 0, 0})
	binary.Write(&b, binary.LittleEndian, [2]uint16{1, 32})
	binary.Write(&b, binary.LittleEndian, uint32(pb.Len()))
	binary.Write(&b, binary.LittleEndian, uint32(22))
	b.Write(pb.Bytes())
	return b.Bytes()
}

// runTray puts Veil in the notification area and blocks until the app quits.
func runTray(g *gui) {
	icons := map[string][]byte{
		"off":  ico(color.RGBA{0x4b, 0x50, 0x6b, 255}, color.RGBA{0x2a, 0x2f, 0x42, 255}),
		"busy": ico(color.RGBA{0xd9, 0x9e, 0x1c, 255}, color.RGBA{0x6b, 0x5a, 0x1f, 255}),
		"on":   ico(color.RGBA{0x7c, 0x5c, 0xf0, 255}, color.RGBA{0x4a, 0xde, 0x80, 255}),
		"err":  ico(color.RGBA{0xd9, 0x4a, 0x4a, 255}, color.RGBA{0x6b, 0x2a, 0x2a, 255}),
	}
	systray.Run(func() {
		systray.SetIcon(icons["off"])
		systray.SetTitle("Veil")
		systray.SetTooltip("Veil - disconnected")
		status := systray.AddMenuItem("Veil - disconnected", "")
		status.Disable()
		toggle := systray.AddMenuItem("Connect", "")
		open := systray.AddMenuItem("Open Veil", "")
		systray.AddSeparator()
		quit := systray.AddMenuItem("Quit", "Disconnect and exit")
		systray.SetOnTapped(g.showWindow)
		go func() {
			last := ""
			for {
				select {
				case <-g.quit:
					systray.Quit()
					return
				case <-time.After(700 * time.Millisecond):
				}
				s := g.eng.Status()
				label, key, tgl := "disconnected", "off", "Connect"
				switch s.State {
				case "connecting":
					label, key, tgl = "connecting…", "busy", "Cancel"
				case "connected":
					label, key, tgl = "connected", "on", "Disconnect"
					if !s.Carrier {
						label, key = "reconnecting…", "busy"
					}
				case "disconnecting":
					label, key, tgl = "disconnecting…", "busy", "Disconnecting…"
				case "error":
					label, key = "error: "+s.Error, "err"
				}
				if label+tgl != last {
					last = label + tgl
					status.SetTitle("Veil - " + label)
					toggle.SetTitle(tgl)
					systray.SetIcon(icons[key])
					tip := "Veil - " + label
					if len(tip) > 120 {
						tip = tip[:120]
					}
					systray.SetTooltip(tip)
				}
			}
		}()
		go func() {
			for {
				select {
				case <-toggle.ClickedCh:
					g.toggleConnect()
				case <-open.ClickedCh:
					g.showWindow()
				case <-quit.ClickedCh:
					select {
					case <-g.quit:
					default:
						close(g.quit)
					}
					return
				}
			}
		}()
	}, func() {})
}
