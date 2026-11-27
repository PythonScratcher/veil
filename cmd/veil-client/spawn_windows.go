//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// detach makes the child independent of this process and invisible.
func detach(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x00000008 | 0x00000200 | 0x08000000} // DETACHED_PROCESS|NEW_PROCESS_GROUP|NO_WINDOW
}
