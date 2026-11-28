#!/usr/bin/env bash
# Builds dist/veil.exe, dist/veil-cli.exe and the installer dist/Veil-Setup.exe
set -euo pipefail
cd "$(dirname "$0")"
mkdir -p dist; cp -n assets/wintun.dll dist/wintun.dll
VER=$(tr -d '[:space:]' < VERSION)
export GOOS=windows GOARCH=amd64 CGO_ENABLED=0
# icon + manifest + version info resources (.syso files are picked up by go build)
WINRES=$(command -v go-winres || echo "$(go env GOPATH)/bin/go-winres")
[ -x "$WINRES" ] || GOOS= GOARCH= go install github.com/tc-hib/go-winres@v0.3.3
for d in cmd/veil-client cmd/veil-setup; do
  "$WINRES" make --in winres/winres.json --arch amd64 --product-version "$VER.0" --file-version "$VER.0" --out "$d/rsrc"
done
go build -ldflags "-s -w -H=windowsgui -X main.appVersion=$VER" -o dist/veil.exe ./cmd/veil-client
go build -ldflags "-s -w -X main.appVersion=$VER" -o dist/veil-cli.exe ./cmd/veil-client
cp dist/veil.exe dist/wintun.dll cmd/veil-setup/payload/
go build -ldflags "-s -w -H=windowsgui -X main.appVer=$VER" -o dist/Veil-Setup.exe ./cmd/veil-setup
rm -f cmd/veil-setup/payload/veil.exe cmd/veil-setup/payload/wintun.dll cmd/veil-client/rsrc_*.syso cmd/veil-setup/rsrc_*.syso
ls -la dist
