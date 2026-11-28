#!/usr/bin/env bash
# End-to-end test of the Windows client on a real Windows machine (GitHub Actions runner, Git Bash).
set -uo pipefail
BIN=$1
W=$RUNNER_TEMP/veil-e2e; rm -rf "$W"; mkdir -p "$W"; cd "$W"
FAIL=0
ok()  { echo "PASS  $*"; }
bad() { echo "FAIL  $*"; FAIL=1; }
psq() { powershell -NoProfile -NonInteractive -Command "$1" 2>&1 | tr -d '\r'; }

LAN=$(psq "(Get-NetIPConfiguration | Where-Object { \$_.IPv4DefaultGateway } | Select-Object -First 1).IPv4Address.IPAddress")
echo "LAN=$LAN"; [ -n "$LAN" ] || exit 1
cp "$BIN/wintun.dll" .
"$BIN/veil-server.exe" init --local --domain "$LAN" --listen 0.0.0.0:18443 --user ci > init.log 2>&1 || { cat init.log; exit 1; }
python - "$LAN" <<'PY'
import json,sys
lan=sys.argv[1]
s=json.load(open("server.json")); s["listen_http"]="0.0.0.0:18080"; s["admin_listen"]="127.0.0.1:18088"; s["allow_private"]=False
json.dump(s,open("server.json","w"),indent=1)
c=json.load(open("client.json")); c["server"]=lan+":18443"; c["ws_addr"]=lan+":18080"
json.dump(c,open("client.json","w"),indent=1)
PY
"$BIN/veil-server.exe" run -c server.json > server.log 2>&1 &
sleep 3
routes() { psq "Get-NetRoute -DestinationPrefix 0.0.0.0/1,128.0.0.0/1,::/1,8000::/1 -ErrorAction SilentlyContinue | Measure-Object | Select-Object -ExpandProperty Count"; }
rules()  { psq "Get-NetFirewallRule -DisplayName 'Veil DNS guard*' -ErrorAction SilentlyContinue | Measure-Object | Select-Object -ExpandProperty Count"; }

for car in tls ws; do
  echo; echo "=========== $car"
  python -c "import json,sys;c=json.load(open('client.json'));c['carrier']='$car';json.dump(c,open('client_$car.json','w'))"
  cp "$BIN/veil-client.exe" "veil_$car.exe"
  t0=$(date +%s%N)
  "./veil_$car.exe" -c "client_$car.json" -tun > "client_$car.log" 2>&1 &
  for i in $(seq 1 120); do grep -q "VPN up" "client_$car.log" && break; sleep 0.25; done
  t1=$(date +%s%N)
  grep -q "VPN up" "client_$car.log" && ok "$car: tunnel up in $(( (t1-t0)/1000000 )) ms" || { bad "$car: tunnel up"; cat "client_$car.log"; continue; }
  [ "$(routes)" = 4 ] && ok "$car: 4 catch-all routes" || bad "$car: catch-all routes ($(routes))"
  [ "$(rules)" = 2 ] && ok "$car: DNS guard rules" || bad "$car: DNS guard rules ($(rules))"
  wh=$(psq "(Get-NetIPInterface -InterfaceAlias Veil -AddressFamily IPv4).WeakHostReceive")
  [ "$wh" = Enabled ] && ok "$car: weak-host receive on the adapter" || bad "$car: weak-host receive is '$wh'"
  curl -s -o /dev/null --max-time 5 http://10.99.99.99/
  for i in $(seq 1 10); do grep -q "tcp 10.99.99.99:80" "client_$car.log" && break; sleep 1; done
  grep -q "tcp 10.99.99.99:80" "client_$car.log" && ok "$car: traffic went through the tunnel" || { bad "$car: no tunnel traffic"; tail -20 "client_$car.log"; }
  # hard kill (like a crash or Task Manager) -> the guardian must restore the network
  taskkill //F //IM "veil_$car.exe" > /dev/null
  for i in $(seq 1 20); do [ "$(routes)$(rules)" = 00 ] && break; sleep 1; done
  [ "$(routes)" = 0 ] && ok "$car: guardian removed routes after kill" || bad "$car: routes left after kill ($(routes))"
  [ "$(rules)" = 0 ] && ok "$car: guardian removed firewall rules after kill" || bad "$car: rules left after kill"
  curl -s -o /dev/null --max-time 15 https://example.com && ok "$car: internet works after kill" || bad "$car: internet broken after kill"
done
echo; [ $FAIL = 0 ] && echo "ALL CHECKS PASSED" || echo "SOME CHECKS FAILED"
exit $FAIL
