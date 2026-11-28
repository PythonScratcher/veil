//go:build windows || nativeui

package main

import (
	"fmt"
	"image"
	"time"

	"gioui.org/font"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"veil/proto"
)

func (u *nativeUI) limits(gtx C, l *proto.AccountLimits) D {
	timeLeft := "No expiry"
	tc := cText
	if l.ExpiresAt > 0 {
		left := time.Until(time.Unix(l.ExpiresAt, 0))
		if left <= 0 {
			timeLeft, tc = "Expired", cBad
		} else {
			timeLeft = fmtDur(left) + " left"
			if left < 72*time.Hour {
				tc = cWarn
			}
		}
	}
	used := l.UsedUp + l.UsedDown
	data := fmtBytes(used) + " used"
	frac := -1.0
	if l.DataCap > 0 {
		data = fmtBytes(used) + " of " + fmtBytes(l.DataCap)
		frac = float64(used) / float64(l.DataCap)
	}
	speed := "Unlimited"
	if l.RateMbps > 0 {
		speed = fmt.Sprintf("%g Mbps", l.RateMbps)
	}
	busy := u.isBusy("limits")
	return card(gtx, cPanel, cLine, func(gtx C) D {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx C) D {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, u.label(14, "Your plan", cText, font.SemiBold).Layout),
					layout.Rigid(func(gtx C) D {
						t := "Refresh limits"
						if busy {
							t = "Refreshing…"
						}
						return u.link(gtx, &u.refreshLim, t, cAccent)
					}),
				)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
			layout.Rigid(func(gtx C) D { return u.kv(gtx, "Time", timeLeft, tc) }),
			layout.Rigid(func(gtx C) D { return u.kv(gtx, "Data", data, cText) }),
			layout.Rigid(func(gtx C) D {
				if frac < 0 {
					return D{}
				}
				return layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(4)}.Layout(gtx, func(gtx C) D { return bar(gtx, frac) })
			}),
			layout.Rigid(func(gtx C) D { return u.kv(gtx, "Speed", speed, cText) }),
		)
	})
}

func bar(gtx C, frac float64) D {
	if frac > 1 {
		frac = 1
	}
	w, h := gtx.Constraints.Max.X, gtx.Dp(6)
	paint.FillShape(gtx.Ops, cPanel2, clip.UniformRRect(image.Rect(0, 0, w, h), h/2).Op(gtx.Ops))
	col := cAcc2
	if frac > 0.9 {
		col = cBad
	} else if frac > 0.75 {
		col = cWarn
	}
	if fw := int(float64(w) * frac); fw > 0 {
		paint.FillShape(gtx.Ops, col, clip.UniformRRect(image.Rect(0, 0, max(fw, h), h), h/2).Op(gtx.Ops))
	}
	return D{Size: image.Pt(w, h)}
}

func (u *nativeUI) collapsible(gtx C, s *section, title string, body layout.Widget) D {
	return card(gtx, cPanel, cLine, func(gtx C) D {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx C) D {
				return s.hdr.Layout(gtx, func(gtx C) D {
					pointer.CursorPointer.Add(gtx.Ops)
					gtx.Constraints.Min.X = gtx.Constraints.Max.X
					c := cText
					if s.hdr.Hovered() {
						c = cAccent
					}
					return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
						layout.Flexed(1, u.label(14, title, c, font.SemiBold).Layout),
						layout.Rigid(func(gtx C) D { return chevron(gtx, s.open, cMute) }),
					)
				})
			}),
			layout.Rigid(func(gtx C) D {
				if !s.open {
					return D{}
				}
				return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, body)
			}),
		)
	})
}

func (u *nativeUI) field(title string, w layout.Widget) layout.FlexChild {
	return layout.Rigid(func(gtx C) D {
		return layout.Inset{Bottom: unit.Dp(12)}.Layout(gtx, func(gtx C) D {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(u.label(12.5, title, cMute, font.Medium).Layout),
				layout.Rigid(layout.Spacer{Height: unit.Dp(5)}.Layout),
				layout.Rigid(w),
			)
		})
	})
}

// choice: a vertical list of radio options.
func (u *nativeUI) choice(e *widget.Enum, opts [][2]string) layout.Widget {
	return func(gtx C) D {
		var kids []layout.FlexChild
		for _, o := range opts {
			o := o
			kids = append(kids, layout.Rigid(func(gtx C) D {
				return e.Layout(gtx, o[0], func(gtx C) D {
					pointer.CursorPointer.Add(gtx.Ops)
					sel := e.Value == o[0]
					hk, hh := e.Hovered()
					hov := hh && hk == o[0]
					return layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(4)}.Layout(gtx, func(gtx C) D {
						return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
							layout.Rigid(func(gtx C) D {
								d := gtx.Dp(18)
								ring := cLine
								if sel {
									ring = cAcc2
								} else if hov {
									ring = cMute
								}
								paint.FillShape(gtx.Ops, ring, clip.Ellipse{Max: image.Pt(d, d)}.Op(gtx.Ops))
								b := gtx.Dp(2)
								paint.FillShape(gtx.Ops, cPanel2, clip.Ellipse{Min: image.Pt(b, b), Max: image.Pt(d-b, d-b)}.Op(gtx.Ops))
								if sel {
									i := gtx.Dp(5)
									paint.FillShape(gtx.Ops, cAccent, clip.Ellipse{Min: image.Pt(i, i), Max: image.Pt(d-i, d-i)}.Op(gtx.Ops))
								}
								return D{Size: image.Pt(d, d)}
							}),
							layout.Rigid(layout.Spacer{Width: unit.Dp(10)}.Layout),
							layout.Flexed(1, u.label(13.5, o[1], cText, font.Normal).Layout),
						)
					})
				})
			}))
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
	}
}

func (u *nativeUI) input(gtx C, e *widget.Editor, hint string) D { return u.inputH(gtx, e, hint, 0) }

func (u *nativeUI) inputH(gtx C, e *widget.Editor, hint string, minH unit.Dp) D {
	return card(gtx, cPanel2, cLine, func(gtx C) D {
		gtx.Constraints.Min.Y = gtx.Dp(minH)
		ed := material.Editor(u.th, e, hint)
		ed.Color, ed.HintColor, ed.TextSize, ed.SelectionColor = cText, cMute, unit.Sp(13.5), alpha(cAcc2, 120)
		return ed.Layout(gtx)
	})
}

func (u *nativeUI) toggle(b *widget.Bool, title, desc string) layout.FlexChild {
	return layout.Rigid(func(gtx C) D {
		return layout.Inset{Bottom: unit.Dp(10)}.Layout(gtx, func(gtx C) D {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, func(gtx C) D {
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
						layout.Rigid(u.label(13.5, title, cText, font.Medium).Layout),
						layout.Rigid(func(gtx C) D {
							if desc == "" {
								return D{}
							}
							return u.label(12, desc, cMute, font.Normal).Layout(gtx)
						}),
					)
				}),
				layout.Rigid(layout.Spacer{Width: unit.Dp(10)}.Layout),
				layout.Rigid(func(gtx C) D {
					sw := material.Switch(u.th, b, title)
					sw.Color.Enabled, sw.Color.Disabled, sw.Color.Track = cAcc2, cMute, cLine
					return sw.Layout(gtx)
				}),
			)
		})
	})
}

func (u *nativeUI) settingsBody(gtx C) D {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		u.field("Connection method", u.choice(&u.carrier, [][2]string{
			{"tls", "HTTPS (default)"},
			{"ws", "Plain HTTP / WebSocket (port 80)"},
			{"poll", "Plain HTTP, no upgrade (port 80, works through proxies)"},
			{"quic", "QUIC (UDP 443)"},
		})),
		u.field("Transport", u.choice(&u.transport, [][2]string{{"stream", "Stream (fastest)"}, {"split", "Split (CDN-friendly)"}})),
		u.field("ClientHello fragmentation", u.choice(&u.frag, [][2]string{{"both", "Both (default)"}, {"record", "TLS records"}, {"tcp", "TCP segments"}, {"none", "Off"}})),
		layout.Rigid(func(gtx C) D {
			return layout.Flex{}.Layout(gtx,
				layout.Flexed(1, func(gtx C) D {
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx, u.field("Rotate connection (min)", func(gtx C) D { return u.input(gtx, &u.rotate, "20") }))
				}),
				layout.Rigid(layout.Spacer{Width: unit.Dp(10)}.Layout),
				layout.Flexed(1, func(gtx C) D {
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx, u.field("DNS inside the tunnel", func(gtx C) D { return u.input(gtx, &u.dns, "1.1.1.1") }))
				}),
			)
		}),
		u.toggle(&u.cover, "Cover traffic", "Looks like an update client"),
		u.toggle(&u.bounce, "Instant app switchover", "Experimental: flaps network adapters on connect"),
		layout.Rigid(func(gtx C) D {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx C) D { return u.btn(gtx, &u.saveSet, "Save settings", true) }),
				layout.Rigid(layout.Spacer{Width: unit.Dp(10)}.Layout),
				layout.Flexed(1, u.label(12, "Applies the next time you connect", cMute, font.Normal).Layout),
			)
		}),
	)
}

func (u *nativeUI) manageBody(gtx C) D {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(u.label(12.5, "Opens your server's admin panel (keys, time, data caps, speed limits, QR codes, API keys, domains). You log in with the admin password in the panel; Veil never stores it.", cMute, font.Normal).Layout),
		layout.Rigid(layout.Spacer{Height: unit.Dp(10)}.Layout),
		u.field("Admin panel address", func(gtx C) D { return u.input(gtx, &u.adminURL, "https://your-server:8088/secret-path/") }),
		u.toggle(&u.adminTrust, "Don't check the panel's certificate", "Only for a self-signed panel not covered by the key"),
		layout.Rigid(func(gtx C) D { return u.btn(gtx, &u.openAdmin, "Save & open admin panel", true) }),
	)
}

func (u *nativeUI) serverBody(gtx C, st uiState) D {
	if st.Profile == nil {
		return D{}
	}
	cert := "Public (system trusted)"
	if st.Profile.LocalCert {
		cert = "Pinned by your key"
	}
	rm := "Remove this server"
	if u.removeArm {
		rm = "Click again to remove"
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx C) D { return u.kv(gtx, "Address", st.Profile.Server, cText) }),
		layout.Rigid(func(gtx C) D { return u.kv(gtx, "Disguised as", nonEmpty(st.Profile.SNI, "-"), cText) }),
		layout.Rigid(func(gtx C) D { return u.kv(gtx, "Certificate", cert, cText) }),
		layout.Rigid(layout.Spacer{Height: unit.Dp(10)}.Layout),
		layout.Rigid(func(gtx C) D {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, func(gtx C) D { return u.input(gtx, &u.nameEd, "Name") }),
				layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
				layout.Rigid(func(gtx C) D { return u.btn(gtx, &u.rename, "Rename", false) }),
			)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(10)}.Layout),
		layout.Rigid(func(gtx C) D {
			return u.btnColors(gtx, &u.remove, rm, rgb(0x2a1618), cBad)
		}),
	)
}

func nonEmpty(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func (u *nativeUI) importCard(gtx C, canCancel bool) D {
	return card(gtx, cPanel, cLine, func(gtx C) D {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(u.label(17, "Add a server", cText, font.SemiBold).Layout),
			layout.Rigid(u.label(12.5, "Paste the key (client.json) your server admin gave you, or load the file.", cMute, font.Normal).Layout),
			layout.Rigid(layout.Spacer{Height: unit.Dp(12)}.Layout),
			layout.Rigid(func(gtx C) D {
				gtx.Constraints.Max.Y = gtx.Dp(240)
				return u.inputH(gtx, &u.impText, "{ \"server\": \"…\", … }", 140)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(10)}.Layout),
			layout.Rigid(func(gtx C) D { return u.input(gtx, &u.impName, "Name (optional)") }),
			layout.Rigid(layout.Spacer{Height: unit.Dp(12)}.Layout),
			layout.Rigid(func(gtx C) D {
				kids := []layout.FlexChild{
					layout.Rigid(func(gtx C) D { return u.btn(gtx, &u.impAdd, "Add server", true) }),
					layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
					layout.Rigid(func(gtx C) D { return u.btn(gtx, &u.impFile, "Load file…", false) }),
				}
				if canCancel {
					kids = append(kids, layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
						layout.Rigid(func(gtx C) D { return u.btn(gtx, &u.impCancel, "Cancel", false) }))
				}
				return layout.Flex{}.Layout(gtx, kids...)
			}),
		)
	})
}

func (u *nativeUI) footer(gtx C, st uiState) D {
	return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx C) D {
			t := "Fix my internet"
			if u.isBusy("repair") {
				t = "Fixing…"
			}
			return u.link(gtx, &u.repair, t, cMute)
		}),
		layout.Rigid(u.label(13, "  ·  ", cLine, font.Normal).Layout),
		layout.Rigid(func(gtx C) D { return u.link(gtx, &u.webui, "Open in browser", cMute) }),
		layout.Rigid(u.label(13, "  ·  ", cLine, font.Normal).Layout),
		layout.Rigid(func(gtx C) D { return u.link(gtx, &u.quit, "Quit Veil", cMute) }),
		layout.Flexed(1, func(gtx C) D {
			return layout.E.Layout(gtx, u.label(12, "v"+st.Version, cMute, font.Normal).Layout)
		}),
	)
}

func (u *nativeUI) toastView(gtx C, msg string) {
	m := op.Record(gtx.Ops)
	gtx2 := gtx
	gtx2.Constraints.Min = image.Point{}
	gtx2.Constraints.Max.X = gtx.Constraints.Max.X - gtx.Dp(48)
	d := card(gtx2, cPanel2, cAcc2, u.label(13, msg, cText, font.Normal).Layout)
	call := m.Stop()
	x := (gtx.Constraints.Max.X - d.Size.X) / 2
	y := gtx.Constraints.Max.Y - d.Size.Y - gtx.Dp(20)
	defer op.Offset(image.Pt(x, y)).Push(gtx.Ops).Pop()
	call.Add(gtx.Ops)
}

// ---------- formatting

func fmtBytes(n int64) string {
	f := float64(n)
	switch {
	case f >= 1<<40:
		return fmt.Sprintf("%.2f TB", f/(1<<40))
	case f >= 1<<30:
		return fmt.Sprintf("%.2f GB", f/(1<<30))
	case f >= 1<<20:
		return fmt.Sprintf("%.1f MB", f/(1<<20))
	case f >= 1<<10:
		return fmt.Sprintf("%.0f KB", f/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func fmtRate(bps float64) string {
	bits := bps * 8
	switch {
	case bits >= 1e9:
		return fmt.Sprintf("%.2f Gbps", bits/1e9)
	case bits >= 1e6:
		return fmt.Sprintf("%.1f Mbps", bits/1e6)
	case bits >= 1e3:
		return fmt.Sprintf("%.0f kbps", bits/1e3)
	}
	return "0 kbps"
}

func fmtDur(d time.Duration) string {
	s := int64(d.Seconds())
	if s < 0 {
		s = 0
	}
	dd, h, m := s/86400, (s%86400)/3600, (s%3600)/60
	switch {
	case dd > 0:
		return fmt.Sprintf("%dd %dh", dd, h)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, s%60)
	}
	return fmt.Sprintf("%ds", s)
}
