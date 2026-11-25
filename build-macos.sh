#!/usr/bin/env bash
# Builds dist/Veil-macOS-arm64.zip (Apple Silicon) and dist/Veil-macOS-intel.zip, each containing Veil.app.
set -euo pipefail
cd "$(dirname "$0")"
VER=$(tr -d '[:space:]' < VERSION)
export GOOS=darwin CGO_ENABLED=0
W=$(mktemp -d)
python3 -I - "$W/Veil.icns" <<'PY'
import sys
from PIL import Image, ImageDraw, ImageFont
S=1024
im=Image.new("RGBA",(S,S),(0,0,0,0)); d=ImageDraw.Draw(im)
d.rounded_rectangle((60,60,S-60,S-60),radius=220,fill=(26,30,48,255))
try: f=ImageFont.truetype("/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",640)
except Exception: f=ImageFont.load_default()
d.text((S/2,S/2+20),"V",font=f,fill=(255,255,255,255),anchor="mm")
im.save(sys.argv[1],format="ICNS")
PY
for pair in arm64:arm64 amd64:intel; do
  arch=${pair%%:*}; tag=${pair##*:}
  D="$W/$tag/Veil.app/Contents"; mkdir -p "$D/MacOS" "$D/Resources"
  GOARCH=$arch go build -ldflags "-s -w -X main.appVersion=$VER" -o "$D/MacOS/Veil" ./cmd/veil-client
  cp "$W/Veil.icns" "$D/Resources/Veil.icns"
  cat > "$D/Info.plist" <<PL
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleName</key><string>Veil</string>
<key>CFBundleDisplayName</key><string>Veil</string>
<key>CFBundleIdentifier</key><string>net.veil.client</string>
<key>CFBundleExecutable</key><string>Veil</string>
<key>CFBundleIconFile</key><string>Veil</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleShortVersionString</key><string>$VER</string>
<key>CFBundleVersion</key><string>$VER</string>
<key>LSMinimumSystemVersion</key><string>11.0</string>
<key>LSUIElement</key><true/>
<key>NSHighResolutionCapable</key><true/>
</dict></plist>
PL
  cat > "$W/$tag/READ ME FIRST.txt" <<TX
Veil $VER for macOS

1. Drag Veil.app into Applications.
2. First launch: macOS says it cannot verify the app (it is not from the App Store).
   Right-click Veil.app > Open > Open.   Or in Terminal:
       xattr -dr com.apple.quarantine /Applications/Veil.app
3. Veil asks for your Mac login password once: that is needed to create the VPN network adapter.
4. A window opens in your browser. Import your key (paste it or load the file), pick a connection method, press Connect.

Veil lives in the menu bar (V icon, filled when connected): Connect/Disconnect, Open Veil, Quit Veil. Opening Veil.app again also shows the window.
If a crash ever leaves your internet broken, open Terminal and run:
    sudo /Applications/Veil.app/Contents/MacOS/Veil -c ~/Library/Application\ Support/Veil/profile.json -cleanup
TX
  rm -f "dist/Veil-macOS-$tag.zip"
  (cd "$W/$tag" && zip -qry "$OLDPWD/dist/Veil-macOS-$tag.zip" Veil.app "READ ME FIRST.txt")
done
rm -rf "$W"; ls -la dist/Veil-macOS-*
