#!/bin/sh
set -eu
out=${1:?usage: build-all.sh OUTDIR [VERSION]}
version=${2:-dev}
jobs=${BUILD_JOBS:-3}
rm -rf "$out"
mkdir -p "$out"

pids=""
running=0
rc=0

wait_all() {
  for p in $pids; do
    wait "$p" || rc=1
  done
  pids=""
  running=0
}

run_bg() {
  "$@" &
  pids="$pids $!"
  running=$((running + 1))
  if [ "$running" -ge "$jobs" ]; then
    wait_all
  fi
}

for target in ${WP_TARGETS:-linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64}; do
  os=${target%/*}
  arch=${target#*/}
  ext=
  [ "$os" = windows ] && ext=.exe
  run_bg env CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
    -ldflags "-s -w -X main.version=$version" -o "$out/wp-$os-$arch$ext" ./cmd/wp
done
for arch in amd64 arm64; do
  run_bg env CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
    -ldflags "-s -w -X main.version=$version" -o "$out/waypoint-sandboxd-linux-$arch" ./cmd/waypoint-sandboxd
done
wait_all
[ "$rc" -eq 0 ] || exit "$rc"

cd "$out"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum wp-* waypoint-sandboxd-* > SHA256SUMS
else
  shasum -a 256 wp-* waypoint-sandboxd-* > SHA256SUMS
fi
