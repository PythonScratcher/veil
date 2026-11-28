#!/usr/bin/env bash
# End-to-end test of the Windows client on a real Windows machine (GitHub Actions runner, Git Bash).
set -uo pipefail
BIN=$1
W=$RUNNER_TEMP/veil-e2e; rm -rf "$W"; mkdir -p "$W"; cd "$W"
FAIL=0
ok()  { echo "PASS  $*"; }
bad() { echo "FAIL  $*"; FAIL=1; }
psq() { timeout 90 powershell -NoProfile -NonInteractive -Command "$1" < /dev/null 2>&1 | tr -d '\r'; }
step() { echo "[$(date +%T)] $*"; }

LAN=$(psq "(Get-NetRoute -DestinationPrefix 0.0.0.0/0 | Sort-Object RouteMetric | Select-Object -First 1 | Get-NetIPAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue | Select-Object -First 1).IPAddress" | grep -Eo '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$' | head -1)
echo "LAN=$LAN"; [ -n "$LAN" ] || exit 1
cp "$BIN/wintun.dll" .
step init
timeout 60 "$BIN/veil-server.exe" init --local --domain "$LAN" --listen 0.0.0.0:18443 --user ci > init.log 2>&1 || { cat init.log; exit 1; }
python - "$LAN" <<'PY'
import json,sys
lan=sys.argv[1]
s=json.load(open("server.json")); s["listen_http"]="0.0.0.0:18080"; s["admin_listen"]="127.0.0.1:18088"; s["allow_private"]=False
json.dump(s,open("server.json","w"),indent=1)
c=json.load(open("client.json")); c["server"]=lan+":18443"; c["ws_addr"]=lan+":18080"
json.dump(c,open("client.json","w"),indent=1)
PY
step server; "$BIN/veil-server.exe" run -c server.json > server.log 2>&1 < /dev/null &
sleep 3
routes() { psq "Get-NetRoute -DestinationPrefix 0.0.0.0/1,128.0.0.0/1,::/1,8000::/1 -ErrorAction SilentlyContinue | Measure-Object | Select-Object -ExpandProperty Count"; }
rules()  { psq "Get-NetFirewallRule -DisplayName 'Veil DNS guard*' -ErrorAction SilentlyContinue | Measure-Object | Select-Object -ExpandProperty Count"; }

echo "plain HTTP check: $(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://$LAN:18080/)"
python -c "import json;c=json.load(open('client.json'));c['carrier']='ws';c['listen_socks']='127.0.0.1:19080';c['listen_http']='127.0.0.1:19081';json.dump(c,open('client_wsproxy.json','w'))"
"$BIN/veil-client.exe" -c client_wsproxy.json > client_wsproxy.log 2>&1 < /dev/null &
sleep 6; echo "ws proxy-mode log:"; cat client_wsproxy.log; taskkill //F //IM veil-client.exe > /dev/null 2>&1
for car in ws tls; do
  echo; echo "=========== $car"
  python -c "import json,sys;c=json.load(open('client.json'));c['carrier']='$car';json.dump(c,open('client_$car.json','w'))"
  cp "$BIN/veil-client.exe" "veil_$car.exe"
  t0=$(date +%s%N)
  step "client $car"; "./veil_$car.exe" -c "client_$car.json" -tun > "client_$car.log" 2>&1 < /dev/null &
  for i in $(seq 1 120); do grep -q "VPN up" "client_$car.log" && break; sleep 0.25; done
  t1=$(date +%s%N)
  grep -q "VPN up" "client_$car.log" && ok "$car: tunnel up in $(( (t1-t0)/1000000 )) ms" || { bad "$car: tunnel up"; cat "client_$car.log"; continue; }
  [ "$(routes)" = 4 ] && ok "$car: 4 catch-all routes" || bad "$car: catch-all routes ($(routes))"
  [ "$(rules)" = 2 ] && ok "$car: DNS guard rules" || bad "$car: DNS guard rules ($(rules))"
  wh=$(psq "(Get-NetIPInterface -InterfaceAlias Veil -AddressFamily IPv4).WeakHostReceive")
  [ "$wh" = Enabled ] && ok "$car: weak-host receive on the adapter" || bad "$car: weak-host receive is '$wh'"
  hit=""
  for ip in 10.99.99.99 172.31.254.1 100.100.100.100 192.168.254.1; do
    echo "route to $ip: $(psq "(Find-NetRoute -RemoteIPAddress $ip | Select-Object -First 1).InterfaceAlias")"
    curl -s -o /dev/null --max-time 4 http://$ip/
    for i in $(seq 1 6); do grep -q "tcp $ip:80" "client_$car.log" && { hit=$ip; break 2; }; sleep 1; done
  done
  [ -n "$hit" ] && ok "$car: traffic went through the tunnel ($hit)" || { bad "$car: no tunnel traffic"; tail -20 "client_$car.log"; }
  # hard kill (like a crash or Task Manager) -> the guardian must restore the network
  # kill only the VPN process (like a crash); its guardian is a separate process and must survive
  WPID=$(python -c "import json,os;print(json.load(open(os.path.join(os.environ['APPDATA'],'Veil','session.json')))['pid'])")
  echo "session.json pid=$WPID; processes:"; tasklist //FI "IMAGENAME eq veil_$car.exe" | tail -n +3
  step "kill pid $WPID"; timeout 30 taskkill //F //PID "$WPID"
  sleep 1; echo "after kill:"; tasklist //FI "IMAGENAME eq veil_$car.exe" | tail -n +3
  for i in $(seq 1 20); do [ "$(routes)$(rules)" = 00 ] && break; sleep 1; done
  [ "$(routes)" = 0 ] && ok "$car: guardian removed routes after kill" || bad "$car: routes left after kill ($(routes))"
  [ "$(rules)" = 0 ] && ok "$car: guardian removed firewall rules after kill" || bad "$car: rules left after kill"
  curl -s -o /dev/null --max-time 15 https://example.com && ok "$car: internet works after kill" || bad "$car: internet broken after kill"
  echo "leftover host routes: $(psq "Get-NetRoute -DestinationPrefix $LAN/32 -ErrorAction SilentlyContinue | Format-Table -HideTableHeaders DestinationPrefix,NextHop,InterfaceAlias | Out-String")"
  echo "plain HTTP check after: $(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://$LAN:18080/)"
  "$BIN/veil-client.exe" -c client.json -cleanup > /dev/null 2>&1   # never leave the runner broken for the next step
done
# ---- native window screenshot
step gui
cp "$BIN/veil-client.exe" veil.exe
APPDATA_DIR=$(cygpath "$APPDATA")/Veil; mkdir -p "$APPDATA_DIR"
python - "$APPDATA_DIR" <<'PY'
import json,sys,os
c=json.load(open("client.json"))
json.dump({"selected":"p1","profiles":[{"id":"p1","name":"CI server","cfg":c}]},open(os.path.join(sys.argv[1],"profiles.json"),"w"))
PY
./veil.exe -gui > gui.log 2>&1 < /dev/null &
sleep 12
psq "Add-Type -AssemblyName System.Windows.Forms,System.Drawing; \$b=[System.Windows.Forms.SystemInformation]::VirtualScreen; \$bmp=New-Object System.Drawing.Bitmap \$b.Width,\$b.Height; \$g=[System.Drawing.Graphics]::FromImage(\$bmp); \$g.CopyFromScreen(\$b.Left,\$b.Top,0,0,\$bmp.Size); \$bmp.Save('$(cygpath -w "$W")\\gui.png')"
ls -la gui.png 2>&1; cat gui.log | tail -5
timeout 20 taskkill //F //IM veil.exe > /dev/null 2>&1
echo; [ $FAIL = 0 ] && echo "ALL CHECKS PASSED" || echo "SOME CHECKS FAILED"
exit $FAIL
