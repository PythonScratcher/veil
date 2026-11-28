package main

import (
	"fmt"
	"log"
	"net"
	"os/exec"
	"strings"
)

const tunName = "Veil"

func ps(script string) (string, error) {
	out, err := hide(exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// configureRoutes gives the wintun adapter an address, points DNS at it, sends
// all traffic into it with two /1 routes (more specific than the default
// route, so nothing is ever deleted), and pins the Veil server's own IP to the
// real gateway so the tunnel doesn't try to ride itself.
func configureRoutes(ifname string, cfg *Config) (func(), error) {
	host, _, _ := net.SplitHostPort(cfg.Server)
	if cfg.ConnectIP != "" {
		host = cfg.ConnectIP
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("resolve server: %v", err)
	}
	srv := ""
	for _, ip := range ips {
		if ip.To4() != nil {
			srv = ip.String()
			break
		}
	}
	if srv == "" {
		return nil, fmt.Errorf("server has no IPv4 address (IPv6-only servers not supported yet)")
	}
	gw, err := ps(`$r = Get-NetRoute -DestinationPrefix 0.0.0.0/0 | Sort-Object { $_.RouteMetric + (Get-NetIPInterface -InterfaceIndex $_.InterfaceIndex -AddressFamily IPv4).InterfaceMetric } | Select-Object -First 1; "$($r.NextHop) $($r.InterfaceIndex)"`)
	if err != nil {
		return nil, fmt.Errorf("find default gateway: %v: %s", err, gw)
	}
	f := strings.Fields(gw)
	if len(f) != 2 {
		return nil, fmt.Errorf("unexpected gateway output %q", gw)
	}
	dns := dnsList(cfg)
	removeDNSLeakRules() // stale rules from a previous crashed run
	steps := []string{
		fmt.Sprintf(`New-NetRoute -DestinationPrefix %s/32 -InterfaceIndex %s -NextHop %s -RouteMetric 1 -PolicyStore ActiveStore`, srv, f[1], f[0]),
		fmt.Sprintf(`New-NetIPAddress -InterfaceAlias '%s' -IPAddress 10.77.0.2 -PrefixLength 24 -PolicyStore ActiveStore | Out-Null`, ifname),
		fmt.Sprintf(`New-NetIPAddress -InterfaceAlias '%s' -IPAddress fd77::2 -PrefixLength 64 -PolicyStore ActiveStore | Out-Null`, ifname),
		fmt.Sprintf(`Set-NetIPInterface -InterfaceAlias '%s' -InterfaceMetric 1`, ifname),
		// catch-all routes go in right away: the slower steps below must not leave a window
		// where traffic still leaves through the real adapter
		fmt.Sprintf(`New-NetRoute -DestinationPrefix 0.0.0.0/1 -InterfaceAlias '%s' -NextHop 0.0.0.0 -RouteMetric 1 -PolicyStore ActiveStore | Out-Null`, ifname),
		fmt.Sprintf(`New-NetRoute -DestinationPrefix 128.0.0.0/1 -InterfaceAlias '%s' -NextHop 0.0.0.0 -RouteMetric 1 -PolicyStore ActiveStore | Out-Null`, ifname),
		// IPv6 is captured too, so a native v6 path can't leak around the tunnel.
		fmt.Sprintf(`New-NetRoute -DestinationPrefix ::/1 -InterfaceAlias '%s' -NextHop :: -RouteMetric 1 -PolicyStore ActiveStore | Out-Null`, ifname),
		fmt.Sprintf(`New-NetRoute -DestinationPrefix 8000::/1 -InterfaceAlias '%s' -NextHop :: -RouteMetric 1 -PolicyStore ActiveStore | Out-Null`, ifname),
		fmt.Sprintf(`Set-DnsClientServerAddress -InterfaceAlias '%s' -ServerAddresses %s`, ifname, dns),
		// Windows asks the DNS servers of EVERY adapter in parallel ("smart multi-homed name
		// resolution"), and LAN resolvers are on-link so they bypass the tunnel routes. Block
		// port 53 on wired/wireless NICs so DNS can only ride the tunnel adapter.
		`New-NetFirewallRule -DisplayName 'Veil DNS guard (udp)' -Direction Outbound -Action Block -Protocol UDP -RemotePort 53 -InterfaceType Wired,Wireless -Profile Any | Out-Null`,
		`New-NetFirewallRule -DisplayName 'Veil DNS guard (tcp)' -Direction Outbound -Action Block -Protocol TCP -RemotePort 53 -InterfaceType Wired,Wireless -Profile Any | Out-Null`,
		`Clear-DnsClientCache`,
	}
	cleanup := func() {
		ps(fmt.Sprintf(`Remove-NetRoute -DestinationPrefix %s/32 -Confirm:$false -ErrorAction SilentlyContinue`, srv))
		removeOverrideRoutes()
		removeDNSLeakRules()
		ps(`Clear-DnsClientCache`)
	}
	// One PowerShell process for every step: starting PowerShell costs ~0.5-1 s each time, so the
	// old one-process-per-step approach added many seconds before traffic actually moved.
	var sb strings.Builder
	sb.WriteString("$ErrorActionPreference = 'Stop'\n")
	for i, st := range steps {
		fmt.Fprintf(&sb, "try { %s } catch { Write-Output \"STEPFAIL %d: $($_.Exception.Message)\"; exit 1 }\n", st, i)
	}
	// Strong-host receive (the Windows default) drops packets that arrive on the VPN adapter but are
	// addressed to the Wi-Fi/Ethernet IP. Connections that were open before connecting (browser
	// keep-alives, QUIC/HTTP3 in Chrome, games, calls) use that address, so their replies were
	// silently dropped until the app timed out - up to a minute. Weak-host receive lets those replies
	// in: QUIC flows simply continue through the tunnel and stale TCP connections get an immediate
	// reset from the tunnel and reconnect.
	fmt.Fprintf(&sb, "Set-NetIPInterface -InterfaceAlias '%s' -WeakHostReceive Enabled -ErrorAction SilentlyContinue\n", ifname)
	if out, err := ps(sb.String()); err != nil {
		log.Printf("route setup failed: %v: %s", err, out)
		cleanup()
		if i := strings.Index(out, "STEPFAIL"); i >= 0 {
			out = out[i:]
		}
		return nil, fmt.Errorf("%v: %s", err, out)
	}
	return cleanup, nil
}

// dnsList turns cfg.DNS ("1.1.1.1" or "1.1.1.1,2606:4700:4700::1111") into a PowerShell array literal.
func dnsList(cfg *Config) string {
	var out []string
	for _, d := range strings.Split(cfg.DNS, ",") {
		if d = strings.TrimSpace(d); net.ParseIP(d) != nil {
			out = append(out, "'"+d+"'")
		}
	}
	if len(out) == 0 {
		out = []string{"'1.1.1.1'", "'2606:4700:4700::1111'"}
	}
	return strings.Join(out, ",")
}

func removeDNSLeakRules() {
	ps(`Remove-NetFirewallRule -DisplayName 'Veil DNS guard*' -ErrorAction SilentlyContinue`)
	ps(`Remove-NetFirewallRule -DisplayName 'VeilCat DNS guard*' -ErrorAction SilentlyContinue`) // legacy name
}

// removeOverrideRoutes removes the four full-tunnel "catch everything" routes (two /1s per
// address family, together outranking the normal default route) by destination prefix alone,
// regardless of which interface they're still attached to. These are the routes that matter most
// to clean up: if the TUN adapter is torn down without them being removed first (a crash, a kill,
// or anything else that skips the normal disconnect path), they can be left pointing at an
// interface that no longer exists - silently black-holing every bit of traffic in that address
// family, system-wide, until someone finds and removes them by hand. A plain adapter or stack
// reset does not reliably clear these; removing them by prefix does.
func removeOverrideRoutes() {
	for _, prefix := range []string{"0.0.0.0/1", "128.0.0.0/1", "::/1", "8000::/1"} {
		ps(fmt.Sprintf(`Remove-NetRoute -DestinationPrefix %s -Confirm:$false -ErrorAction SilentlyContinue`, prefix))
	}
}

// cleanupStale undoes everything a crashed/killed run may have left behind.
func cleanupStale(cfg *Config) {
	removeDNSLeakRules()
	removeOverrideRoutes()
	host, _, _ := net.SplitHostPort(cfg.Server)
	if host == "" {
		ps(`Clear-DnsClientCache`)
		return
	}
	if cfg.ConnectIP != "" {
		host = cfg.ConnectIP
	}
	if ips, err := net.LookupIP(host); err == nil {
		for _, ip := range ips {
			if ip.To4() != nil {
				ps(fmt.Sprintf(`Remove-NetRoute -DestinationPrefix %s/32 -Confirm:$false -ErrorAction SilentlyContinue`, ip))
			}
		}
	}
	ps(`Clear-DnsClientCache`)
	fmt.Println("cleaned up Veil firewall rules and routes")
}
