#!/bin/sh
set -eu
out=${1:?usage: build-all.sh OUTDIR [VERSION]}
version=${2:-dev}
rm -rf "$out"
mkdir -p "$out"
for target in ${WP_TARGETS:-linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64}; do
  os=${target%/*}
  arch=${target#*/}
  ext=
  [ "$os" = windows ] && ext=.exe
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X main.version=$version" -o "$out/wp-$os-$arch$ext" ./cmd/wp
done
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X main.version=$version" -o "$out/waypoint-sandboxd-linux-$arch" ./cmd/waypoint-sandboxd
done
cd "$out"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum wp-* waypoint-sandboxd-* > SHA256SUMS
else
  shasum -a 256 wp-* waypoint-sandboxd-* > SHA256SUMS
fi
