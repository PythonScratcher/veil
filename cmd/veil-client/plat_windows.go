package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	isWindows       = true
	isMac           = false
	elevateAtLaunch = true
	guiByDefault    = true
)

// hide stops a console window flashing up for every PowerShell call (the GUI exe has no console).
func hide(c *exec.Cmd) *exec.Cmd {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	return c
}

func isAdmin() bool { return windows.GetCurrentProcessToken().IsElevated() }

// relaunchAsAdmin starts this exe again with the UAC prompt.
func relaunchAsAdmin() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(exe)
	args, _ := windows.UTF16PtrFromString("-elevated")
	dir, _ := windows.UTF16PtrFromString(filepath.Dir(exe))
	return windows.ShellExecute(0, verb, file, args, dir, windows.SW_NORMAL)
}

// openWindow shows the UI as a chromeless app window using Edge/Chrome (always present on Windows 10/11).
// It returns the process so the caller can notice when the window is closed.
func openWindow(url, dataDir string) *exec.Cmd {
	cands := []string{}
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LocalAppData"} {
		b := os.Getenv(env)
		if b == "" {
			continue
		}
		cands = append(cands, filepath.Join(b, `Microsoft\Edge\Application\msedge.exe`), filepath.Join(b, `Google\Chrome\Application\chrome.exe`))
	}
	for _, c := range cands {
		if _, err := os.Stat(c); err == nil {
			cmd := exec.Command(c, "--app="+url, "--window-size=500,780", "--user-data-dir="+filepath.Join(dataDir, "window"), "--no-first-run", "--no-default-browser-check", "--disable-extensions")
			if cmd.Start() == nil {
				return cmd
			}
		}
	}
	exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	return nil
}

// ---- Windows system proxy (so browsers/apps follow the proxy without any setup)

const inetKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

type savedProxy struct {
	Enable   uint32 `json:"enable"`
	Server   string `json:"server"`
	Override string `json:"override"`
	Set      bool   `json:"set"`
}

func refreshInet() {
	wi := windows.NewLazySystemDLL("wininet.dll").NewProc("InternetSetOptionW")
	wi.Call(0, 39, 0, 0) // INTERNET_OPTION_SETTINGS_CHANGED
	wi.Call(0, 37, 0, 0) // INTERNET_OPTION_REFRESH
}

func setSystemProxy(addr, stateFile string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, inetKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	var sp savedProxy
	if b, err := os.ReadFile(stateFile); err == nil && json.Unmarshal(b, &sp) == nil && sp.Set {
		// a previous run never restored: keep the ORIGINAL values, don't save our own
	} else {
		sp.Enable64(k)
		sp.Set = true
		b, _ := json.Marshal(sp)
		os.WriteFile(stateFile, b, 0600)
	}
	k.SetDWordValue("ProxyEnable", 1)
	k.SetStringValue("ProxyServer", addr)
	k.SetStringValue("ProxyOverride", "<local>")
	refreshInet()
	return nil
}

func (sp *savedProxy) Enable64(k registry.Key) {
	if v, _, err := k.GetIntegerValue("ProxyEnable"); err == nil {
		sp.Enable = uint32(v)
	}
	sp.Server, _, _ = k.GetStringValue("ProxyServer")
	sp.Override, _, _ = k.GetStringValue("ProxyOverride")
}

func restoreSystemProxy(stateFile string) {
	b, err := os.ReadFile(stateFile)
	if err != nil {
		return
	}
	var sp savedProxy
	if json.Unmarshal(b, &sp) != nil || !sp.Set {
		return
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, inetKey, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	k.SetDWordValue("ProxyEnable", sp.Enable)
	if sp.Server != "" {
		k.SetStringValue("ProxyServer", sp.Server)
	} else {
		k.DeleteValue("ProxyServer")
	}
	if sp.Override != "" {
		k.SetStringValue("ProxyOverride", sp.Override)
	} else {
		k.DeleteValue("ProxyOverride")
	}
	refreshInet()
	os.Remove(stateFile)
}
