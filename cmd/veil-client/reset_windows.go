package main

import (
	"fmt"
	"net"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	iphlpapi        = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetTCPTable = iphlpapi.NewProc("GetExtendedTcpTable")
	procSetTCPEntry = iphlpapi.NewProc("SetTcpEntry")
)

type tcpRowPID struct{ state, laddr, lport, raddr, rport, pid uint32 }
type tcpRow struct{ state, laddr, lport, raddr, rport uint32 }

// resetExistingConnections makes every app - including ones that keep long-lived UDP/QUIC
// sessions, like Chrome's HTTP/3 - start using the tunnel without being restarted.
//
// Step 1 is a quiet, instant fix for plain IPv4 TCP: delete each ESTABLISHED connection's TCB
// directly. The app sees a reset and reconnects, now through the new route. No visible glitch.
//
// Step 2 covers everything that step 1 can't reach: IPv6 TCP (Windows has no delete-TCB call for
// it) and, above all, UDP/QUIC (Windows' UDP table has no remote address, so a single flow can't be
// targeted). Briefly unbinding and rebinding IPv4 and IPv6 on the physical adapters forces every
// socket on them closed at once and fires the network-change notification that browsers (Chrome's
// QUIC stack included) watch for, so they tear down and rebuild their whole connection pool against
// the current routes. The VPN's own link to the server rides the same adapter, so it also drops for
// that instant; it reconnects by itself the next time something uses the tunnel. This step is
// opt-in (cfg.NetBounce) and off by default - on one real machine the rebind left the IPv4 stack
// broken badly enough that even a full adapter disable/enable cycle didn't recover it, needing a
// netsh stack reset and reboot. Always runs inside try/finally so the adapters can never be left
// unbound on the Go side, and only runs when something is actually
// using the network, to keep the blip rare.
func resetExistingConnections(cfg *Config) int {
	host, _, _ := net.SplitHostPort(cfg.Server)
	if cfg.ConnectIP != "" {
		host = cfg.ConnectIP
	}
	skip := map[uint32]bool{}
	if ips, err := net.LookupIP(host); err == nil {
		for _, ip := range ips {
			if v4 := ip.To4(); v4 != nil {
				skip[*(*uint32)(unsafe.Pointer(&v4[0]))] = true
			}
		}
	}
	dropped := resetTCP4(skip)
	if cfg.NetBounce != nil && *cfg.NetBounce {
		if n := bounceStack(); n {
			dropped++ // at least one flow (likely UDP/QUIC or IPv6) existed and was reset by the bounce
		}
	}
	return dropped
}

func resetTCP4(skip map[uint32]bool) int {
	var size uint32
	var buf []byte
	for i := 0; i < 4; i++ {
		var p unsafe.Pointer
		if len(buf) > 0 {
			p = unsafe.Pointer(&buf[0])
		}
		r, _, _ := procGetTCPTable.Call(uintptr(p), uintptr(unsafe.Pointer(&size)), 0, 2 /*AF_INET*/, 5 /*OWNER_PID_ALL*/, 0)
		if r == 0 {
			break
		}
		if r != uintptr(windows.ERROR_INSUFFICIENT_BUFFER) {
			return 0
		}
		buf = make([]byte, size+4096)
	}
	if len(buf) < 4 {
		return 0
	}
	n := *(*uint32)(unsafe.Pointer(&buf[0]))
	rows := unsafe.Slice((*tcpRowPID)(unsafe.Pointer(&buf[4])), int(n))
	dropped := 0
	for _, r := range rows {
		if r.state != 5 { // ESTABLISHED only
			continue
		}
		rip := net.IP{byte(r.raddr), byte(r.raddr >> 8), byte(r.raddr >> 16), byte(r.raddr >> 24)}
		if skip[r.raddr] || rip.IsLoopback() || rip.IsPrivate() || rip.IsLinkLocalUnicast() || rip.IsUnspecified() {
			continue
		}
		row := tcpRow{12 /*DELETE_TCB*/, r.laddr, r.lport, r.raddr, r.rport}
		if ret, _, _ := procSetTCPEntry.Call(uintptr(unsafe.Pointer(&row))); ret == 0 {
			dropped++
		}
	}
	return dropped
}

// bounceStack flaps the IPv4 and IPv6 bindings on the physical adapters and reports whether
// anything outside the LAN was using the network at the time (TCP of either family, or any UDP
// socket at all, since a QUIC/HTTP3 session can't be told apart from other UDP traffic here).
func bounceStack() bool {
	out, err := ps(`$tcp = @(Get-NetTCPConnection -State Established -ErrorAction SilentlyContinue | Where-Object { $_.RemoteAddress -notmatch '^(127\.|::1|fe80|fd|fc|10\.|192\.168\.|169\.254\.)' -and $_.RemoteAddress -notmatch '^172\.(1[6-9]|2[0-9]|3[0-1])\.' }).Count
$udp = @(Get-NetUDPEndpoint -ErrorAction SilentlyContinue | Where-Object { $_.OwningProcess -ne $PID }).Count
if ($tcp -gt 0 -or $udp -gt 0) {
  $ad = @(Get-NetAdapter -Physical -ErrorAction SilentlyContinue | Where-Object { $_.Status -eq 'Up' })
  try {
    foreach ($a in $ad) { Disable-NetAdapterBinding -Name $a.Name -ComponentID ms_tcpip,ms_tcpip6 -ErrorAction SilentlyContinue }
    Start-Sleep -Milliseconds 700
  } finally {
    foreach ($a in $ad) { Enable-NetAdapterBinding -Name $a.Name -ComponentID ms_tcpip,ms_tcpip6 -ErrorAction SilentlyContinue }
    # Enable-NetAdapterBinding can report success while the binding stays off (seen in the wild:
    # IPv6 came back fine, IPv4 stayed disabled, stranding the machine with no IPv4 networking at
    # all until someone noticed and fixed it by hand). Verify it actually took, and keep retrying
    # whichever ones didn't - this must not come back from a single silently-swallowed failure.
    for ($i = 0; $i -lt 8; $i++) {
      $stuck = @()
      foreach ($a in $ad) {
        foreach ($comp in 'ms_tcpip','ms_tcpip6') {
          $b = Get-NetAdapterBinding -Name $a.Name -ComponentID $comp -ErrorAction SilentlyContinue
          if ($b -and -not $b.Enabled) { $stuck += , @($a.Name, $comp) }
        }
      }
      if ($stuck.Count -eq 0) { break }
      foreach ($s in $stuck) { Enable-NetAdapterBinding -Name $s[0] -ComponentID $s[1] -ErrorAction SilentlyContinue }
      if ($i -eq 4) {
        # A plain re-enable isn't taking after several tries - fall back to a full adapter
        # restart (a different, often more reliable recovery path than toggling the binding
        # again) for whichever adapters are still stuck, before giving the simple retry a few
        # more chances.
        $stuckAdapters = $stuck | ForEach-Object { $_[0] } | Select-Object -Unique
        foreach ($name in $stuckAdapters) { Restart-NetAdapter -Name $name -Confirm:$false -ErrorAction SilentlyContinue }
        Start-Sleep -Milliseconds 800
      } else {
        Start-Sleep -Milliseconds 400
      }
    }
    # Windows is supposed to re-request a DHCP lease on its own once a binding comes back, but
    # that doesn't always happen promptly either. Force it explicitly so the machine is never
    # left without connectivity because of this flap.
    foreach ($a in $ad) { & ipconfig /renew "$($a.Name)" 2>$null | Out-Null }
  }
  Clear-DnsClientCache
  "1"
} else { "0" }`)
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "1"
}

var _ = fmt.Sprint
