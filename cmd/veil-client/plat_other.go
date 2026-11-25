//go:build !windows && !darwin

package main

import (
	"errors"
	"os/exec"
)

const (
	isWindows       = false
	isMac           = false
	elevateAtLaunch = false
	guiByDefault    = false
)

func hide(c *exec.Cmd) *exec.Cmd { return c }
func isAdmin() bool              { return true }
func relaunchAsAdmin() error     { return errors.New("only needed on Windows") }

func openWindow(url, dataDir string) *exec.Cmd {
	c := exec.Command("xdg-open", url)
	if c.Start() != nil {
		return nil
	}
	return nil // a normal browser tab: we cannot tell when it closes
}

func setSystemProxy(addr, stateFile string) error { return errors.New("system proxy is Windows-only") }
func restoreSystemProxy(stateFile string)         {}
