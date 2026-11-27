package main

import (
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// A full-tunnel session changes system-wide state (routes, firewall rules, DNS). If the app is
// killed, crashes, freezes or is replaced by an update while connected, that state would otherwise
// stay behind and break all traffic - even after restarting Veil. So every TUN session:
//   - writes session.json (who owns the changes) and touches session.beat every few seconds, and
//   - starts a tiny detached copy of this program ("guardian") that watches the owner and, the
//     moment it dies or stops beating, undoes the changes itself and (if frozen) kills it.
// A normal disconnect removes session.json first, so the guardian just exits.

type session struct {
	PID       int    `json:"pid"`
	Server    string `json:"server"`
	ConnectIP string `json:"connect_ip,omitempty"`
	DNS       string `json:"dns,omitempty"`
}

func sessionPath() string { return filepath.Join(configDir(), "session.json") }
func beatPath() string    { return filepath.Join(configDir(), "session.beat") }

func readSession() (session, bool) {
	var s session
	b, err := os.ReadFile(sessionPath())
	if err != nil || json.Unmarshal(b, &s) != nil || s.PID == 0 {
		return s, false
	}
	return s, true
}

func (s session) cfg() *Config { return &Config{Server: s.Server, ConnectIP: s.ConnectIP, DNS: s.DNS} }

// beginSession records ownership, starts the heartbeat and the guardian. The returned func ends it.
func beginSession(cfg *Config) func() {
	os.MkdirAll(configDir(), 0700)
	s := session{PID: os.Getpid(), Server: cfg.Server, ConnectIP: cfg.ConnectIP, DNS: cfg.DNS}
	b, _ := json.Marshal(s)
	os.WriteFile(sessionPath(), b, 0600)
	touch := func() {
		now := time.Now()
		if os.Chtimes(beatPath(), now, now) != nil {
			os.WriteFile(beatPath(), []byte("1"), 0600)
		}
	}
	touch()
	if exe, err := os.Executable(); err == nil {
		c := exec.Command(exe, "-guardian", strconv.Itoa(s.PID))
		detach(c)
		if err := c.Start(); err != nil {
			log.Printf("guardian not started: %v", err)
		} else {
			go c.Wait()
		}
	}
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				touch()
			}
		}
	}()
	return func() {
		close(stop)
		os.Remove(sessionPath())
		os.Remove(beatPath())
	}
}

// runGuardian watches process pid; see the comment at the top of this file.
func runGuardian(pid int) {
	last := time.Now()
	for {
		time.Sleep(2 * time.Second)
		slept := time.Since(last) > 10*time.Second // the computer was asleep: do not judge the heartbeat
		last = time.Now()
		s, ok := readSession()
		if !ok || s.PID != pid {
			return // clean disconnect, or a newer session took over
		}
		alive := processAlive(pid)
		stale := false
		if fi, err := os.Stat(beatPath()); err == nil && !slept && time.Since(fi.ModTime()) > 45*time.Second {
			stale = true
		}
		if alive && !stale {
			continue
		}
		if alive {
			log.Printf("guardian: Veil (pid %d) stopped responding - killing it and restoring the network", pid)
			if p, err := os.FindProcess(pid); err == nil {
				p.Kill()
			}
			time.Sleep(time.Second)
		} else {
			log.Printf("guardian: Veil (pid %d) is gone - restoring the network", pid)
		}
		if s2, ok := readSession(); !ok || s2.PID != pid {
			return
		}
		cleanupStale(s.cfg())
		os.Remove(sessionPath())
		os.Remove(beatPath())
		return
	}
}

// repairNetwork removes anything a dead session left behind. Safe to call when not connected.
func repairNetwork() {
	if s, ok := readSession(); ok {
		cleanupStale(s.cfg())
	} else {
		cleanupStale(&Config{})
	}
	os.Remove(sessionPath())
	os.Remove(beatPath())
}
