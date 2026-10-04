#!/bin/sh
# Builds the three release binaries into dist/. No C compiler needed.
set -eu
cd "$(dirname "$0")"
mkdir -p dist
for target in darwin/arm64 darwin/amd64 windows/amd64; do
  os=${target%/*}; arch=${target#*/}
  out="dist/owlka-whatsapp-$os-$arch"
  [ "$os" = windows ] && out="$out.exe"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w" -o "$out" .
  echo "built $out"
done
