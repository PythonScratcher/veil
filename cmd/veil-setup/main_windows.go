package main

import (
	"embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

//go:embed payload
var payload embed.FS

const (
	appName   = "Veil"
	uninstKey = `Software\Microsoft\Windows\CurrentVersion\Uninstall\Veil`
	taskName  = "Veil (start at sign-in)"
)

// appVer is stamped from the VERSION file at build time (-ldflags -X main.appVer=...)
var appVer = "dev"

var user32 = windows.NewLazySystemDLL("user32.dll")

func msgBox(title, text string, flags uint32) int {
	t, _ := windows.UTF16PtrFromString(text)
	c, _ := windows.UTF16PtrFromString(title)
	r, _, _ := user32.NewProc("MessageBoxW").Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), uintptr(flags|0x10000)) // MB_SETFOREGROUND
	return int(r)
}

const (
	mbYesNo = 4
	mbQ     = 0x20
	mbInfo  = 0x40
	mbErr   = 0x10
	idYes   = 6
)

func quiet(name string, args ...string) ([]byte, error) {
	c := exec.Command(name, args...)
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	return c.CombinedOutput()
}

func ps(script string) error {
	out, err := quiet("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func isAdmin() bool { return windows.GetCurrentProcessToken().IsElevated() }

func relaunchElevated(args []string) error {
	exe, _ := os.Executable()
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(exe)
	a, _ := windows.UTF16PtrFromString(strings.Join(args, " "))
	return windows.ShellExecute(0, verb, file, a, nil, windows.SW_NORMAL)
}

func installDir() string {
	pf := os.Getenv("ProgramFiles")
	if pf == "" {
		pf = `C:\Program Files`
	}
	return filepath.Join(pf, appName)
}

func startMenuDir() string {
	pd := os.Getenv("ProgramData")
	if pd == "" {
		pd = `C:\ProgramData`
	}
	return filepath.Join(pd, `Microsoft\Windows\Start Menu\Programs`, appName)
}

func publicDesktop() string {
	p := os.Getenv("PUBLIC")
	if p == "" {
		p = `C:\Users\Public`
	}
	return filepath.Join(p, "Desktop")
}

func main() {
	args := os.Args[1:]
	has := func(f string) bool {
		for _, a := range args {
			if strings.EqualFold(a, f) {
				return true
			}
		}
		return false
	}
	silent := has("/S") || has("-silent")
	uninstall := has("-uninstall") || strings.HasPrefix(strings.ToLower(filepath.Base(os.Args[0])), "uninstall")
	if !isAdmin() {
		if err := relaunchElevated(args); err != nil {
			msgBox(appName, "Veil needs Administrator rights to install (it creates a VPN adapter).", mbErr)
		}
		return
	}
	var err error
	if uninstall {
		err = doUninstall(silent)
	} else {
		err = doInstall(silent)
	}
	if err != nil {
		if !silent {
			msgBox(appName+" setup", "Something went wrong:\n\n"+err.Error(), mbErr)
		}
		os.Exit(1)
	}
}

const bootTask = "Veil network cleanup"

// cleanNetwork removes the firewall rules and catch-all routes a Veil session that died while
// connected (crash, kill, update) can leave behind. They would otherwise block or black-hole traffic.
func cleanNetwork() {
	ps(`Remove-NetFirewallRule -DisplayName 'Veil DNS guard*' -ErrorAction SilentlyContinue`)
	ps(`Remove-NetFirewallRule -DisplayName 'VeilCat DNS guard*' -ErrorAction SilentlyContinue`)
	for _, p := range []string{"0.0.0.0/1", "128.0.0.0/1", "::/1", "8000::/1"} {
		ps(`Remove-NetRoute -DestinationPrefix ` + p + ` -Confirm:$false -ErrorAction SilentlyContinue`)
	}
}

func doInstall(silent bool) error {
	dir := installDir()
	autostart := false
	if !silent {
		if msgBox(appName+" setup", fmt.Sprintf("Install Veil %s?\n\nIt will be installed to:\n%s\n\nYou will find Veil in the Start menu and on the desktop.", appVer, dir), mbYesNo|mbQ) != idYes {
			return nil
		}
		autostart = msgBox(appName+" setup", "Start Veil automatically when you sign in to Windows?\n\n(It starts quietly in the system tray - it does not connect by itself.)", mbYesNo|mbQ) == idYes
	}
	// stop a running copy so files can be replaced
	quiet("taskkill", "/F", "/IM", "veil.exe")
	time.Sleep(700 * time.Millisecond)
	cleanNetwork() // an update must never leave the previous session's routes/firewall rules behind

	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	for _, f := range []string{"veil.exe", "wintun.dll"} {
		b, err := payload.ReadFile("payload/" + f)
		if err != nil {
			return errors.New("this installer was built without " + f)
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0755); err != nil {
			return fmt.Errorf("writing %s: %w", f, err)
		}
	}
	self, _ := os.Executable()
	if err := copyFile(self, filepath.Join(dir, "uninstall.exe")); err != nil {
		return err
	}
	exe := filepath.Join(dir, "veil.exe")
	sm := startMenuDir()
	os.MkdirAll(sm, 0755)
	for _, lnk := range []string{filepath.Join(sm, "Veil.lnk"), filepath.Join(publicDesktop(), "Veil.lnk")} {
		if err := shortcut(lnk, exe, dir, ""); err != nil {
			return fmt.Errorf("shortcut: %w", err)
		}
	}
	if err := shortcut(filepath.Join(sm, "Uninstall Veil.lnk"), filepath.Join(dir, "uninstall.exe"), dir, "-uninstall"); err != nil {
		return err
	}
	// Add/Remove Programs entry
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, uninstKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	k.SetStringValue("DisplayName", appName)
	k.SetStringValue("DisplayVersion", appVer)
	k.SetStringValue("Publisher", "Veil")
	k.SetStringValue("InstallLocation", dir)
	k.SetStringValue("DisplayIcon", exe)
	k.SetStringValue("UninstallString", `"`+filepath.Join(dir, "uninstall.exe")+`" -uninstall`)
	k.SetDWordValue("NoModify", 1)
	k.SetDWordValue("NoRepair", 1)
	k.SetDWordValue("EstimatedSize", 25000)
	k.Close()
	// at every boot, before anyone signs in: remove leftovers of a session that died while connected
	quiet("schtasks", "/Delete", "/TN", bootTask, "/F")
	quiet("schtasks", "/Create", "/TN", bootTask, "/TR", `"`+exe+`" -cleanup`, "/SC", "ONSTART", "/RU", "SYSTEM", "/RL", "HIGHEST", "/F")
	// start at sign-in: a scheduled task with highest privileges avoids a UAC prompt on every logon
	quiet("schtasks", "/Delete", "/TN", taskName, "/F")
	if autostart {
		if out, err := quiet("schtasks", "/Create", "/TN", taskName, "/TR", `"`+exe+`" -gui -no-open`, "/SC", "ONLOGON", "/RL", "HIGHEST", "/F"); err != nil {
			return fmt.Errorf("could not set up start at sign-in: %s", strings.TrimSpace(string(out)))
		}
	}
	if !silent {
		if msgBox(appName+" setup", "Veil is installed.\n\nStart Veil now?", mbYesNo|mbInfo) == idYes {
			exec.Command(exe).Start()
		}
	}
	return nil
}

func doUninstall(silent bool) error {
	if !silent && msgBox(appName+" setup", "Remove Veil from this computer?", mbYesNo|mbQ) != idYes {
		return nil
	}
	dropData := !silent && msgBox(appName+" setup", "Also delete your saved servers and keys?\n\n(Choose No to keep them in case you reinstall.)", mbYesNo|mbQ) == idYes
	quiet("taskkill", "/F", "/IM", "veil.exe")
	quiet("schtasks", "/Delete", "/TN", taskName, "/F")
	quiet("schtasks", "/Delete", "/TN", bootTask, "/F")
	cleanNetwork() // leftovers of a crashed VPN session
	time.Sleep(500 * time.Millisecond)
	os.RemoveAll(startMenuDir())
	os.Remove(filepath.Join(publicDesktop(), "Veil.lnk"))
	registry.DeleteKey(registry.LOCAL_MACHINE, uninstKey)
	if dropData {
		if d, err := os.UserConfigDir(); err == nil { // elevated as the same user: their own profile
			os.RemoveAll(filepath.Join(d, "Veil"))
			os.RemoveAll(filepath.Join(d, "VeilCat"))
		}
	}
	dir := installDir()
	os.Remove(filepath.Join(dir, "veil.exe"))
	os.Remove(filepath.Join(dir, "wintun.dll"))
	// uninstall.exe is this process: delete the folder once we have exited
	c := exec.Command("cmd", "/C", "ping -n 3 127.0.0.1 >nul & rmdir /s /q \""+dir+"\"")
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	c.Start()
	if !silent {
		msgBox(appName+" setup", "Veil has been removed.", mbInfo)
	}
	return nil
}

func shortcut(lnk, target, dir, args string) error {
	q := func(s string) string { return strings.ReplaceAll(s, "'", "''") }
	return ps(fmt.Sprintf(`$s=(New-Object -ComObject WScript.Shell).CreateShortcut('%s'); $s.TargetPath='%s'; $s.Arguments='%s'; $s.WorkingDirectory='%s'; $s.IconLocation='%s,0'; $s.Description='Veil'; $s.Save()`,
		q(lnk), q(target), q(args), q(dir), q(filepath.Join(dir, "veil.exe"))))
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if filepath.Clean(src) == filepath.Clean(dst) {
		return nil
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
