#!/usr/bin/env bash
# Build "Claude Usage.app" — a no-dock menu-bar agent (LSUIElement).
#
# Usage: ./build/make-app.sh
# Produces ./Claude Usage.app, which you can double-click or `open`.
set -euo pipefail

cd "$(dirname "$0")/.."

APP="Claude Usage.app"
CONTENTS="$APP/Contents"
MACOS="$CONTENTS/MacOS"

rm -rf "$APP"
mkdir -p "$MACOS"

echo "Building binary…"
# systray links Cocoa, so cgo must be enabled (it is by default on macOS).
CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" -o "$MACOS/claudeusage" .

cp build/Info.plist "$CONTENTS/Info.plist"

echo "Built \"$APP\""
echo "Launch it with:  open \"$APP\"   (or double-click it in Finder)"
