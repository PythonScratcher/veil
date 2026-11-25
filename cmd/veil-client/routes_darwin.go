//go:build darwin

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
)

const tunName = "utun" // the kernel picks the number (utunN)

const dnsStateFile = "/var/db/veil-dns.json"

func run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// defaultGateway returns the gateway and interface of the current IPv4 default route.
func defaultGateway() (gw, iface string, err error) {
	out, err := run("/sbin/route", "-n", "get", "-inet", "default")
	if err != nil {
		return "", "", fmt.Errorf("%v: %s", err, out)
	}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) == 2 && f[0] == "gateway:" {
			gw = f[1]
		}
		if len(f) == 2 && f[0] == "interface:" {
			iface = f[1]
		}
	}
	if gw == "" && iface == "" {
		return "", "", fmt.Errorf("no default route found")
	}
	return gw, iface, nil
}

func networkServices() []string {
	out, err := run("/usr/sbin/networksetup", "-listallnetworkservices")
	if err != nil {
		return nil
	}
	var s []string
	for i, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if i == 0 || l == "" || strings.HasPrefix(l, "*") { // header line / disabled services
			continue
		}
		s = append(s, l)
	}
	return s
}

func dnsServers(cfg *Config) []string {
	var out []string
	for _, d := range strings.Split(cfg.DNS, ",") {
		if d = strings.TrimSpace(d); net.ParseIP(d) != nil {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		out = []string{"1.1.1.1", "2606:4700:4700::1111"}
	}
	return out
}

// setDNS points every network service at the tunnel's resolvers, remembering the old values on
// disk so a crash can still be undone.
func setDNS(cfg *Config) {
	saved := map[string][]string{}
	if b, err := os.ReadFile(dnsStateFile); err == nil {
		json.Unmarshal(b, &saved) // keep the ORIGINAL values if a stale file exists
	}
	servers := dnsServers(cfg)
	for _, svc := range networkServices() {
		if _, ok := saved[svc]; !ok {
			out, _ := run("/usr/sbin/networksetup", "-getdnsservers", svc)
			var cur []string
			if !strings.Contains(out, "any DNS Servers") {
				cur = strings.Fields(out)
			}
			saved[svc] = cur
		}
		if o, err := run("/usr/sbin/networksetup", append([]string{"-setdnsservers", svc}, servers...)...); err != nil {
			log.Printf("dns %q: %v: %s", svc, err, o)
		}
	}
	b, _ := json.Marshal(saved)
	os.WriteFile(dnsStateFile, b, 0600)
	run("/usr/bin/dscacheutil", "-flushcache")
	run("/usr/bin/killall", "-HUP", "mDNSResponder")
}

func restoreDNS() {
	b, err := os.ReadFile(dnsStateFile)
	if err != nil {
		return
	}
	var saved map[string][]string
	if json.Unmarshal(b, &saved) != nil {
		return
	}
	for svc, list := range saved {
		args := []string{"-setdnsservers", svc}
		if len(list) == 0 {
			args = append(args, "Empty")
		} else {
			args = append(args, list...)
		}
		run("/usr/sbin/networksetup", args...)
	}
	os.Remove(dnsStateFile)
	run("/usr/bin/dscacheutil", "-flushcache")
	run("/usr/bin/killall", "-HUP", "mDNSResponder")
}

func serverIPv4(cfg *Config) string {
	host, _, _ := net.SplitHostPort(cfg.Server)
	if cfg.ConnectIP != "" {
		host = cfg.ConnectIP
	}
	ips, _ := net.LookupIP(host)
	for _, ip := range ips {
		if ip.To4() != nil {
			return ip.String()
		}
	}
	return ""
}

func removeOverrideRoutes(ifname string) {
	for _, p := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
		run("/sbin/route", "-n", "delete", "-net", p)
	}
	for _, p := range []string{"::/1", "8000::/1"} {
		run("/sbin/route", "-n", "delete", "-inet6", "-net", p)
	}
}

// configureRoutes gives the tunnel interface an address, pins the Veil server to the real gateway,
// and sends everything else (IPv4 and IPv6) into the tunnel with the same four "/1" routes the
// Windows client uses, so the normal default route is never touched.
func configureRoutes(ifname string, cfg *Config) (func(), error) {
	srv := serverIPv4(cfg)
	if srv == "" {
		return nil, fmt.Errorf("cannot resolve the server's IPv4 address")
	}
	gw, giface, err := defaultGateway()
	if err != nil {
		return nil, fmt.Errorf("find default gateway: %v", err)
	}
	removeOverrideRoutes(ifname) // leftovers from a crashed run
	restoreDNS()

	cleanup := func() {
		removeOverrideRoutes(ifname)
		run("/sbin/route", "-n", "delete", "-host", srv)
		restoreDNS()
	}
	pin := []string{"-n", "add", "-host", srv}
	if gw != "" && !strings.HasPrefix(gw, "link#") {
		pin = append(pin, gw)
	} else {
		pin = append(pin, "-interface", giface)
	}
	run("/sbin/route", "-n", "delete", "-host", srv)
	steps := [][]string{
		append([]string{"/sbin/route"}, pin...),
		{"/sbin/ifconfig", ifname, "inet", "10.77.0.2", "10.77.0.1", "netmask", "255.255.255.0", "up"},
		{"/sbin/ifconfig", ifname, "inet6", "fd77::2", "prefixlen", "64", "alias"},
		{"/sbin/route", "-n", "add", "-net", "0.0.0.0/1", "-interface", ifname},
		{"/sbin/route", "-n", "add", "-net", "128.0.0.0/1", "-interface", ifname},
		{"/sbin/route", "-n", "add", "-inet6", "-net", "::/1", "-interface", ifname},
		{"/sbin/route", "-n", "add", "-inet6", "-net", "8000::/1", "-interface", ifname},
	}
	for _, s := range steps {
		if out, err := run(s[0], s[1:]...); err != nil {
			// the IPv6 steps may fail on a Mac with IPv6 switched off; only IPv4 is essential
			if strings.Contains(strings.Join(s, " "), "inet6") || strings.Contains(strings.Join(s, " "), "::") {
				log.Printf("ipv6 step skipped: %s: %s", strings.Join(s, " "), out)
				continue
			}
			log.Printf("route step failed: %s: %v: %s", strings.Join(s, " "), err, out)
			cleanup()
			return nil, fmt.Errorf("%s: %v: %s", strings.Join(s, " "), err, out)
		}
	}
	setDNS(cfg)
	return cleanup, nil
}

// cleanupStale undoes whatever a crashed or killed run left behind.
func cleanupStale(cfg *Config) {
	removeOverrideRoutes("")
	if srv := serverIPv4(cfg); srv != "" {
		run("/sbin/route", "-n", "delete", "-host", srv)
	}
	restoreDNS()
}
