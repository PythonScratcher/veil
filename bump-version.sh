#!/usr/bin/env bash
# Usage: ./bump-version.sh [patch|minor|major]   (default: patch). Writes VERSION; build-windows.sh
# and the Android Gradle build both read it, so every release gets a higher number.
set -euo pipefail
cd "$(dirname "$0")"
IFS=. read -r MA MI PA < VERSION
case "${1:-patch}" in
  major) MA=$((MA+1)); MI=0; PA=0;;
  minor) MI=$((MI+1)); PA=0;;
  patch) PA=$((PA+1));;
  *) echo "usage: $0 [patch|minor|major]"; exit 1;;
esac
echo "$MA.$MI.$PA" > VERSION
cat VERSION
