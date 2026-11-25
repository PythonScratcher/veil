//go:build darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const (
	isWindows       = false
	isMac           = true
	elevateAtLaunch = true
	guiByDefault    = true
)

func hide(c *exec.Cmd) *exec.Cmd { return c }
func isAdmin() bool              { return os.Geteuid() == 0 }

// asQuote makes s safe inside an AppleScript double-quoted string.
func asQuote(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}

// shQuote single-quotes s for /bin/sh.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// relaunchAsAdmin starts this program again as root through the standard macOS password prompt.
// HOME and the login user's uid are passed along so the root copy uses the same settings folder
// and can still open the UI window in the user's own session.
func relaunchAsAdmin() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	uid := fmt.Sprint(os.Getuid())
	sh := fmt.Sprintf("/usr/bin/env HOME=%s VEIL_UID=%s %s -elevated >/dev/null 2>&1 &", shQuote(home), uid, shQuote(exe))
	script := fmt.Sprintf(`do shell script "%s" with administrator privileges with prompt "Veil needs permission to create the VPN network adapter."`, asQuote(sh))
	out, err := exec.Command("/usr/bin/osascript", "-e", script).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "-128") {
			return errors.New("administrator permission was cancelled")
		}
		return fmt.Errorf("%v: %s", err, msg)
	}
	return nil
}

// openWindow opens the UI in the user's default browser (as the login user even when we are root).
func openWindow(url, dataDir string) *exec.Cmd {
	if os.Geteuid() == 0 {
		if uid := os.Getenv("VEIL_UID"); uid != "" && uid != "0" {
			if exec.Command("/bin/launchctl", "asuser", uid, "/usr/bin/open", url).Run() == nil {
				return nil
			}
		}
	}
	exec.Command("/usr/bin/open", url).Run()
	return nil // a normal browser tab: we cannot tell when it closes
}

func setSystemProxy(addr, stateFile string) error { return errors.New("system proxy is Windows-only") }
func restoreSystemProxy(stateFile string)         {}
