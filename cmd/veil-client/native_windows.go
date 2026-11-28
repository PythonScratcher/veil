//go:build windows

package main

import (
	"sync"
	"time"
	"unsafe"

	"gioui.org/app"
	"gioui.org/io/system"
	"gioui.org/op"
	"gioui.org/unit"
	"golang.org/x/sys/windows"
)

var (
	natMu  sync.Mutex
	natWin *app.Window
)

func init() { nativeWindow = showNative }

// showNative opens the native window, or brings the open one to the front.
func showNative(g *gui) {
	natMu.Lock()
	if natWin != nil {
		w := natWin
		natMu.Unlock()
		w.Perform(system.ActionRaise)
		return
	}
	w := new(app.Window)
	natWin = w
	natMu.Unlock()
	go runNative(g, w)
}

func runNative(g *gui, w *app.Window) {
	defer func() {
		natMu.Lock()
		natWin = nil
		natMu.Unlock()
	}()
	w.Option(app.Title("Veil"), app.Size(unit.Dp(460), unit.Dp(740)), app.MinSize(unit.Dp(380), unit.Dp(520)))
	u := newNativeUI(g)
	u.invalidate = w.Invalidate
	u.poll()
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(700 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				u.poll()
				w.Invalidate()
			}
		}
	}()
	var ops op.Ops
	for {
		switch e := w.Event().(type) {
		case app.DestroyEvent:
			return // closing the window keeps Veil running in the tray
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			u.Layout(gtx)
			e.Frame(gtx.Ops)
		}
	}
}

func openBrowser(url string) {
	verb, _ := windows.UTF16PtrFromString("open")
	u, _ := windows.UTF16PtrFromString(url)
	windows.ShellExecute(0, verb, u, nil, nil, windows.SW_SHOWNORMAL)
}

var (
	comdlg32         = windows.NewLazySystemDLL("comdlg32.dll")
	procGetOpenFileW = comdlg32.NewProc("GetOpenFileNameW")
)

type openFileName struct {
	lStructSize       uint32
	hwndOwner         uintptr
	hInstance         uintptr
	lpstrFilter       *uint16
	lpstrCustomFilter *uint16
	nMaxCustFilter    uint32
	nFilterIndex      uint32
	lpstrFile         *uint16
	nMaxFile          uint32
	lpstrFileTitle    *uint16
	nMaxFileTitle     uint32
	lpstrInitialDir   *uint16
	lpstrTitle        *uint16
	flags             uint32
	nFileOffset       uint16
	nFileExtension    uint16
	lpstrDefExt       *uint16
	lCustData         uintptr
	lpfnHook          uintptr
	lpTemplateName    *uint16
	pvReserved        uintptr
	dwReserved        uint32
	flagsEx           uint32
}

// pickFile shows the standard Windows "Open" dialog for a key file.
func pickFile() string {
	buf := make([]uint16, 1024)
	filter := windows.StringToUTF16("Veil key (*.json)\x00*.json;*.txt\x00All files\x00*.*\x00\x00")
	title, _ := windows.UTF16PtrFromString("Load a Veil key")
	ofn := openFileName{
		lpstrFilter: &filter[0],
		lpstrFile:   &buf[0],
		nMaxFile:    uint32(len(buf)),
		lpstrTitle:  title,
		flags:       0x00001000 | 0x00000800 | 0x00080000, // FILEMUSTEXIST | PATHMUSTEXIST | EXPLORER
	}
	ofn.lStructSize = uint32(unsafe.Sizeof(ofn))
	r, _, _ := procGetOpenFileW.Call(uintptr(unsafe.Pointer(&ofn)))
	if r == 0 {
		return ""
	}
	return windows.UTF16ToString(buf)
}
