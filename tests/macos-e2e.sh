#!/usr/bin/env bash
# End-to-end test of the macOS client on a real Mac (GitHub Actions runner).
# Server and client run on the same machine; the server's own address is the runner's LAN IP.
set -uo pipefail
BIN=$1
W=${RUNNER_TEMP:-/tmp}/veil-e2e
rm -rf "$W"; mkdir -p "$W"; cd "$W"
FAIL=0
ok()   { echo "PASS  $*"; }
bad()  { echo "FAIL  $*"; FAIL=1; }
chk()  { local name=$1; shift; if "$@" >/dev/null 2>&1; then ok "$name"; else bad "$name"; fi; }

IFACE=$(route -n get default | awk '/interface:/{print $2}')
LAN=$(ipconfig getifaddr "$IFACE")
echo "default iface=$IFACE lan=$LAN"; sw_vers; uname -m
[ -n "$LAN" ] || { echo "no LAN IP"; exit 1; }

dnsdump() { networksetup -listallnetworkservices | tail -n +2 | grep -v '^\*' | while read -r s; do echo "$s: $(networksetup -getdnsservers "$s" | tr '\n' ' ')"; done; }
dnsdump > dns_before.txt; echo "--- DNS before"; cat dns_before.txt

# ---- server
"$BIN/veil-server" init --local --domain "$LAN" --listen 0.0.0.0:18443 --user ci > init.log 2>&1 || { cat init.log; exit 1; }
python3 - "$LAN" <<'PY'
import json,sys
lan=sys.argv[1]
s=json.load(open("server.json")); s["listen_http"]="0.0.0.0:18080"; s["listen_quic"]="0.0.0.0:18443"; s["admin_listen"]="127.0.0.1:18088"; s["allow_private"]=False
json.dump(s,open("server.json","w"),indent=1)
c=json.load(open("client.json")); c["server"]=lan+":18443"; c["ws_addr"]=lan+":18080"
json.dump(c,open("client.json","w"),indent=1)
PY
"$BIN/veil-server" run -c server.json > server.log 2>&1 &
sleep 3
kill -0 $! 2>/dev/null && ok "server running" || { bad "server running"; cat server.log; exit 1; }

routes4() { netstat -rn -f inet | awk '$1=="0/1"||$1=="128.0/1"'; }
routes6() { netstat -rn -f inet6 | awk '$1=="::/1"||$1=="8000::/1"'; }

run_carrier() {
  local car=$1
  echo; echo "=================== carrier: $car"
  python3 - "$car" <<'PY'
import json,sys
c=json.load(open("client.json")); c["carrier"]=sys.argv[1]; json.dump(c,open("client_"+sys.argv[1]+".json","w"))
PY
  sudo "$BIN/veil-client" -c "client_$car.json" -tun > "client_$car.log" 2>&1 &
  local pid=$!
  for i in $(seq 1 40); do grep -q "VPN up" "client_$car.log" && break; kill -0 $pid 2>/dev/null || break; sleep 1; done
  if ! grep -q "VPN up" "client_$car.log"; then bad "$car: tunnel came up"; cat "client_$car.log"; sudo kill -9 $pid 2>/dev/null; sudo "$BIN/veil-client" -c client.json -cleanup; return; fi
  ok "$car: tunnel came up"
  local ut; ut=$(ifconfig | awk '/^utun[0-9]+:/{n=$1} /10\.77\.0\.2/{print n}' | tr -d :)
  [ -n "$ut" ] && ok "$car: interface $ut has 10.77.0.2" || bad "$car: tunnel interface address"
  echo "--- routes"; routes4; routes6
  [ "$(routes4 | grep -c "$ut")" = 2 ] && ok "$car: IPv4 catch-all routes on $ut" || bad "$car: IPv4 catch-all routes"
  [ "$(routes6 | wc -l | tr -d ' ')" -ge 1 ] && ok "$car: IPv6 catch-all routes" || echo "NOTE  $car: no IPv6 routes (runner may have no IPv6)"
  netstat -rn -f inet | grep -q "^$LAN" && ok "$car: server address pinned" || echo "NOTE  $car: no pinned host route (see log)"
  echo "--- DNS during"; dnsdump | tee "dns_during_$car.txt"
  grep -q "1.1.1.1" "dns_during_$car.txt" && ok "$car: DNS points at tunnel resolvers" || bad "$car: DNS not changed"
  # a packet to a private address must enter the tunnel and be refused by the server (path: utun -> client -> carrier -> server)
  for try in 1 2 3; do
    curl -s -o /dev/null --max-time 5 http://10.99.99.99/
    for i in $(seq 1 8); do grep -q "tcp 10.99.99.99:80" "client_$car.log" && break 2; sleep 1; done
    echo "probe attempt $try: nothing yet"
  done
  grep -q "tcp 10.99.99.99:80" "client_$car.log" && ok "$car: traffic entered the tunnel and reached the server" || { bad "$car: no tunnel traffic seen"; tail -20 "client_$car.log"; }
  echo "--- client log"; tail -5 "client_$car.log"
  # clean disconnect
  sudo pkill -TERM -f "veil-client -c client_$car.json"; for i in $(seq 1 15); do kill -0 $pid 2>/dev/null || break; sleep 1; done
  kill -0 $pid 2>/dev/null && { bad "$car: client exited on SIGTERM"; sudo kill -9 $pid; } || ok "$car: client exited on SIGTERM"
  [ -z "$(routes4)$(routes6)" ] && ok "$car: catch-all routes removed" || { bad "$car: routes left behind"; routes4; routes6; }
  dnsdump > "dns_after_$car.txt"; diff -q dns_before.txt "dns_after_$car.txt" >/dev/null && ok "$car: DNS restored" || { bad "$car: DNS not restored"; diff dns_before.txt "dns_after_$car.txt"; }
  ifconfig | grep -q "10.77.0.2" && bad "$car: tunnel interface still present" || ok "$car: tunnel interface gone"
}
for c in tls ws poll quic; do run_carrier $c; done

# ---- hard kill, then -cleanup
echo; echo "=================== kill -9 then -cleanup"
sudo "$BIN/veil-client" -c client.json -tun > client_kill.log 2>&1 &
pid=$!
for i in $(seq 1 40); do grep -q "VPN up" client_kill.log && break; sleep 1; done
sudo pkill -9 -f "veil-client -c client.json"; sleep 2
echo "--- right after kill -9:"; routes4; routes6; dnsdump
for i in $(seq 1 15); do [ -z "$(routes4)$(routes6)" ] && break; sleep 1; done
[ -z "$(routes4)$(routes6)" ] && ok "guardian removed the routes by itself" || bad "guardian did not remove routes"
dnsdump > dns_after_guardian.txt; diff -q dns_before.txt dns_after_guardian.txt >/dev/null && ok "guardian restored DNS by itself" || { bad "guardian did not restore DNS"; diff dns_before.txt dns_after_guardian.txt; }
sudo "$BIN/veil-client" -c client.json -cleanup
[ -z "$(routes4)$(routes6)" ] && ok "cleanup removed routes" || bad "cleanup left routes"
dnsdump > dns_after_kill.txt; diff -q dns_before.txt dns_after_kill.txt >/dev/null && ok "cleanup restored DNS" || { bad "cleanup did not restore DNS"; diff dns_before.txt dns_after_kill.txt; }
curl -s -o /dev/null --max-time 15 https://example.com && ok "internet works after cleanup" || bad "internet broken after cleanup"

# ---- GUI + menu bar item (runs as root like the real app after the password prompt)
echo; echo "=================== GUI and menu bar"
sudo env HOME="$HOME" VEIL_UID="$(id -u)" "$BIN/veil-client" -gui -no-open > gui.log 2>&1 &
gpid=$!
sleep 8
if kill -0 $gpid 2>/dev/null; then ok "GUI process alive after 8 s (menu-bar code did not crash)"; else bad "GUI process died"; fi
cat gui.log
URL=$(grep -o 'http://127.0.0.1:[0-9]*/?t=[0-9a-f]*' gui.log | head -1)
[ -n "$URL" ] && chk "GUI web page served" curl -sfL -c jar -b jar -o /dev/null "$URL"
screencapture -x screen.png 2>screen.err && ok "screenshot taken (see artifact)" || echo "NOTE  no screenshot: $(cat screen.err)"
sudo pkill -TERM -f "veil-client -gui" ; sleep 2; sudo pkill -9 -f "veil-client -gui" 2>/dev/null

echo; [ $FAIL = 0 ] && echo "ALL CHECKS PASSED" || echo "SOME CHECKS FAILED"
exit $FAIL
