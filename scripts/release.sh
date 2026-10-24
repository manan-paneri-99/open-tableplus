#!/usr/bin/env bash
# Builds standalone binaries for every platform into dist/. No Go needed to run them.
# Usage: scripts/release.sh [version]   (default: git describe, else "dev")
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION="${1:-$(git describe --tags --always 2>/dev/null || echo dev)}"
rm -rf dist && mkdir -p dist

for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64; do
  os="${target%/*}" arch="${target#*/}"
  name="open-tableplus-$VERSION-$os-$arch"
  bin="open-tableplus"; [ "$os" = windows ] && bin="open-tableplus.exe"
  mkdir -p "dist/$name"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION" -o "dist/$name/$bin" .
  cp README.md "dist/$name/"
  if [ "$os" = windows ]; then
    (cd dist && zip -qr "$name.zip" "$name")
  else
    tar -C dist -czf "dist/$name.tar.gz" "$name"
  fi
  rm -rf "dist/$name"
done

(cd dist && shasum -a 256 *.tar.gz *.zip > SHA256SUMS)
ls -lh dist
