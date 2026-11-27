# Veil

A from-scratch censorship-resistant VPN. Linux server, Windows, macOS and Android clients.
To the network it is a Chrome-fingerprinted HTTPS/HTTP2 session to a normal website.

## How it hides
| Layer | What it does |
|---|---|
| Real TLS + real domain | Server holds a genuine certificate (Let's Encrypt via built-in autocert). No fake handshakes, no "unknown protocol" tells. |
| uTLS Chrome ClientHello | Client's JA3/JA4-style TLS fingerprint is Chrome's, ALPN h2. |
| Chrome-style HTTP/2 | Own h2 client (not Go's): Chrome's SETTINGS values/order, 15663105 window boost, exclusive-priority HEADERS, `:method :authority :scheme :path` order, Chrome header order + client hints. Verified on the wire. |
| Decoy website | Everything that is not a valid Veil request (probes, crawlers, wrong path/key, empty or garbage POSTs, replays) gets an ordinary nginx-style site, or a reverse-proxy of a *real* site (`decoy_url`). |
| Secret path + key | Tunnel needs POST to a secret path *and* a valid hello. |
| Inner E2E crypto | X25519 + ChaCha20-Poly1305 inside the stream, server key pinned in client. A TLS-intercepting middlebox or untrusted CDN sees nothing useful. Forward secret. |
| Replay protection | Timestamp window + seen-ephemeral cache. |
| Padding | Random record padding, heavier on the first 12 records. |
| Anti-SSRF | Authenticated users still cannot reach server loopback/LAN/metadata IPs (`allow_private` to disable). |

## Server (Linux)
```
./veil-server init --domain vpn.example.com        # writes server.json + client.json
sudo ./veil-server run -c server.json              # needs :443 and DNS pointed here
```
Certificates are obtained automatically (TLS-ALPN-01 on :443). Optional: `--decoy-url https://some.real.site`.

## Admin web UI (keys & limits)
`veil-server init` prints a one-time admin password. By default the UI listens on `0.0.0.0:8088` over HTTPS using the server's real certificate: open TCP 8088 and browse to `https://your.domain:8088` (use the domain, not the IP).
Want it private? Set `"admin_listen": "127.0.0.1:8088"` and use `ssh -L 8088:127.0.0.1:8088 you@server`. Reset the password with `veil-server passwd -c server.json`.

Per key you can set, and change at any time (changes cut/relax open connections instantly):
* **Expiry** - never, a date, or one-click +1 / 7 / 30 / 90 / 365 days
* **Data cap** (up+down) - unlimited or N MB/GB/TB, one-click top-ups, reset, optional auto-reset every N days
* **Bandwidth cap** - Mb/s per direction, or unlimited
* **Max devices** - distinct client IPs at once (behind a CDN set `real_ip_header`, e.g. `CF-Connecting-IP`)
* Enable/disable, rename, notes, regenerate secret, delete, download `client.json`

Whatever limits are set show up in the Windows app itself: a "time left / data used / speed limit" panel appears under the stats as soon as any of those three are non-zero for the connected key, re-checked automatically about once a minute while connected (needs server 1.6+), so a change made in the admin panel shows up live. If the server blocks the key (disabled, expired, data cap reached) the Windows and macOS apps keep re-checking every 2 minutes and reconnect by themselves once the key works again; the Android app shows the current limits live but needs a manual reconnect. A key with no limits set shows nothing extra.
Live view: online IPs, current speed, total traffic, last seen.
**Usage charts:** total traffic for all keys plus a per-key ranking on the dashboard, and a per-key Usage dialog; ranges 1h / 6h / 24h / 7d / 30d (per-minute for 6 h, per-hour for 35 days, kept in `history.json`; deleting a key keeps its past traffic in the total). Clients get a clear message when their key is expired / capped / disabled.
Keys and usage live in `users.json` (mode 0600, saved every 10 s and on shutdown). Login is bcrypt + lockout after 5 bad tries, session cookie is HttpOnly/SameSite=Strict with CSRF tokens, strict CSP.

## macOS app

`Veil-macOS-arm64.zip` (Apple Silicon) or `Veil-macOS-intel.zip` (Intel). Unzip, drag `Veil.app` to Applications. First launch: right-click > Open (or `xattr -dr com.apple.quarantine /Applications/Veil.app`). Veil asks for your Mac login password once, to create the VPN adapter, then opens its window in your browser. Same GUI, keys and connection methods as Windows. Menu-bar icon (V, filled when connected) with Connect/Disconnect, Open Veil and Quit Veil. Routing uses four catch-all routes (IPv4 and IPv6) and DNS is set via `networksetup`, restored on disconnect; `-cleanup` undoes leftovers after a crash. Built on Linux, not yet run on a real Mac.

## Windows app (GUI + tray)

Double-click `veil.exe` (keep `wintun.dll` next to it). Windows asks for Administrator once at launch - needed to create the VPN adapter.
First run: paste your `client.json` (or choose the file) and press Import. Then press the big power button: **everything on the computer, including DNS, goes through the tunnel** (Full VPN only).

Veil lives in the system tray (notification area): left-click opens the window, right-click gives Connect / Disconnect, Open Veil, Quit. The icon is grey (off), amber (connecting), purple/green (connected), red (error).
Closing the window does not disconnect; use Quit in the tray menu (or the Quit link in the window) to disconnect and exit.
**No app restarts needed.** The instant the tunnel adapter comes up, the client briefly flaps the IPv4/IPv6 bindings on your physical network adapters (half a second, only when something outside the LAN was actually using the network at that moment). That forces every socket on the machine closed at once and fires the network-change notification apps watch for, so Chrome (and everything else, including long-lived QUIC/HTTP3 connections) picks up the tunnel immediately instead of needing to be closed and reopened. The client verifies each binding actually came back on (retrying, then falling back to a full adapter restart if it doesn't) and forces a DHCP renew afterward, since Windows doesn't always do either promptly on its own. If this ever causes trouble on a particular machine's network driver, turn "Instant app switchover" off in Settings - the tunnel works the same either way, you'd just need to close and reopen apps with long-lived connections (Chrome included) once after connecting.
**Account status is live, not just at connect.** If the server admin disables a key, lets it expire, or it runs out of its data cap while you're already connected, the app notices within about a second - even if you weren't actively browsing anything at the time - and shows a plain-English reason ("this key has been disabled by the server admin", etc.) instead of just quietly dropping out and leaving you guessing.
**Multiple servers:** use "＋ Add" to import more `client.json` files, pick one from the Server list, rename or remove it under "This server". Each server keeps its own settings. Switch servers while disconnected.
Settings (transport, ClientHello fragmentation, rotation, cover traffic, DNS) and a log are in the window.
`veil-cli.exe` is the same engine for terminals/scripts (`veil-cli.exe -c client.json -tun`, or proxy mode without -tun).

## Client (Windows, command line)
Put `veil-client.exe`, `wintun.dll` and `client.json` in one folder.
* Proxy mode (no admin): `veil-client.exe -c client.json` -> SOCKS5 `127.0.0.1:1080`, HTTP `127.0.0.1:8080`
* Full VPN (admin PowerShell): `veil-client.exe -c client.json -tun`
  Creates adapter `Veil`, routes all IPv4+IPv6 (+DNS) through the tunnel, pins the server IP to your real gateway. Ctrl+C removes the pin.

`client.json` extras: `connect_ip` (dial an IP but use `server` as SNI/Host, e.g. a CDN edge), `ca_file` (testing).

## DNS
* **Full VPN mode (`-tun`)**: every UDP and TCP DNS query rides the tunnel (verified), and the adapter's DNS is set to `tun_dns` (default `1.1.1.1`; comma list, IPv4/IPv6 ok). Because Windows also queries the DNS servers of your physical adapters in parallel (and LAN resolvers bypass the tunnel routes), the client adds a Windows Firewall rule blocking outbound port 53 on wired/wireless NICs while connected, and flushes the DNS cache. Rules are removed on exit; if the client is killed hard run `veil-client.exe -c client.json -cleanup` (Administrator) to remove them.
* **Proxy mode**: SOCKS5 and HTTP-proxy hostnames are resolved on the server. Make sure your app uses remote DNS (`socks5h://`, Firefox "Proxy DNS when using SOCKS v5").
* The client must resolve the Veil server's own hostname before the tunnel is up. Put the server IP in `connect_ip` to avoid even that lookup.
* Test at https://dnsleaktest.com (extended test): only your Veil server's resolver choice should appear.

## No domain / port 443 busy (local certificate)

`veil-server init --local --domain <server-ip> [--listen :8443] [--sni updates.example.com]` makes a self-signed
certificate (10 years), listens on 8443 and puts the certificate into `client.json` (`ca_pem`), so the client pins it.
No domain or Let's Encrypt needed. Weaker camouflage than a real domain on 443: the certificate is not publicly trusted and the port is unusual.

## Stealth features

**Hidden admin panel.** The panel stays on port 8088, but only under a secret path
(`admin_prefix`, printed by `init`; older configs get one on first start). Everything else on :8088 - including `/` -
is the same decoy company website as on :443, so a scanner sees an ordinary business site.

**Decoy website.** Anything that is not a valid Veil request gets a fictional enterprise software company's marketing website (home, products, solutions, pricing, security, company pages). Rebrand it with `update_site` in the config (`product` = company name, `vendor` = short name), or serve your own with `decoy_url` / `decoy_dir`.
`/releases.json`, `/releases/<ver>`, `/download/<ver>/<file>` (stable, checksum-matching packages),
`/api/v1/update/check`, `/status`, nginx-style 404s. Rebrand with `"update_site": {"product": "...", "vendor": "..."}`,
or use `decoy_url` / `decoy_dir` to show a real site instead.

**Carrier transport (client.json).**
- `"transport": "stream"` (default): one long bidirectional POST carries a multiplexed session.
- `"transport": "split"`: CDN-friendly. Downloads are one long response, uploads are many short POSTs, reassembled by sequence number. Use this behind Cloudflare-style proxies that buffer or cut bidirectional streams.
- `"rotate_minutes"` (default 20, jittered +/-30%, -1 = off): a fresh TLS connection takes over; the old one drains. Long downloads survive rotation.
- `"cover"` (default true): pad frames inside the tunnel plus occasional update-site fetches (`/releases.json`, ...) on the same HTTP/2 connection, like a real updater.
- `"frag"`: `both` (default) | `record` | `tcp` | `none`. Splits the TLS ClientHello across TLS records and/or TCP segments inside the SNI, to defeat SNI-matching DPI. Set `none` if a middlebox chokes on it.

Limits: ECH cannot be done by the server alone; traffic shaping blurs but does not erase statistical fingerprints.

## Honest limitations
* Windows TUN, route and firewall (DNS guard) code is untested on real Windows (built here on Linux). The packet path (TUN -> gVisor -> tunnel, TCP+UDP) was tested on a Linux TUN.
* The h2 preface matches Chrome 131 as best I can reproduce it from memory of its wire format; I could not diff against a live Chrome capture here, so verify with a real browser pcap (Akamai h2 fingerprint) and bump `chromeVer`/headers as Chrome changes.
* Long-lived bidirectional POST streams are not how browsers behave; heavy statistical DPI could notice. No cover traffic yet.
* Server must be reachable by IP and a domain you control. IP-blocked servers need a CDN (`connect_ip`) and the CDN must not buffer streaming POSTs.
* The admin UI is a login page on its own port: keep it on localhost/SSH or behind a firewall allow-list if you can; a public panel is a fingerprint of its own.
* Not audited. The handshake is custom; treat accordingly.

## Connection methods

HTTPS (TLS) is the default. Others are chosen per server in the client Settings (or `"carrier"` in the key) and never switch automatically:

| `carrier` | What it is | Server setting |
|---|---|---|
| `tls` | HTTPS on 443 (default) | - |
| `ws` | WebSocket over plain HTTP, port 80, sealed records with padding | `"listen_http": "0.0.0.0:80"` |
| `poll` | Plain HTTP request/response only (no Upgrade), works through proxies that strip WebSockets; slower | `"listen_http": "0.0.0.0:80"` |
| `quic` | QUIC / HTTP3 on UDP 443; fast, but many networks drop UDP | `"listen_quic": "0.0.0.0:443"` |

`ws_addr` (optional) overrides the plain-HTTP address (default: server host, port 80). `ws` and `poll` also serve the decoy site on port 80.

## Android: QR codes and server management

- Admin panel: a key's **Config** dialog shows a QR code. In the app choose **Add server → Scan QR code**.
- Manage server (server screen ⋮ menu): sign in with the admin address (including its secret path) and password, then list, create, edit, reset, delete keys, show their QR codes and change the admin password.
