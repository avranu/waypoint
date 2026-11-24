#!/bin/bash
# Runs the rich editor suites under WebKit when the host lacks its system
# libraries (libgtk-4, gstreamer, ...): the browser runs in Playwright's own
# Docker image (same version as e2e/package.json) behind `playwright run-server`,
# the suites and the Waypoint server stay on the host (--network host).
#   ./webkit.sh                 suite 85
#   ./webkit.sh 83 84 85        these suites
# Needs docker and a built bin/waypoint (`make build`); `make e2e-webkit` does both.
set -euo pipefail
cd "$(dirname "$0")"
# A suite that launches Chromium itself (no launchEngine, e.g. one that needs a
# CDP session) would run on Chromium and be reported as a WebKit run: refuse it.
for a in "${@:-85-rich-typing}"; do
  for f in "$a"*.mjs; do
    [ -e "$f" ] || { echo "webkit.sh: no suite $a" >&2; exit 1; }
    grep -q launchEngine "$f" || { echo "webkit.sh: $f does not use launchEngine, so it cannot run under WebKit" >&2; exit 1; }
  done
done
[ -d node_modules/playwright ] || npm ci --no-audit --no-fund
ver=$(node -p 'require("./node_modules/playwright/package.json").version')
port=$(node -e 'const s=require("net").createServer().listen(0,"127.0.0.1",()=>{console.log(s.address().port);s.close()})')
name=pw-webkit-$$
trap 'docker stop "$name" >/dev/null 2>&1 || true' EXIT
docker run -d --rm --name "$name" --network host --ipc host --init \
  "mcr.microsoft.com/playwright:v$ver-noble" \
  npx -y "playwright@$ver" run-server --port "$port" --host 127.0.0.1 >/dev/null
for _ in $(seq 1 60); do
  docker logs "$name" 2>&1 | grep -q Listening && break
  sleep 1
done
docker logs "$name" 2>&1 | grep -q Listening || { docker logs "$name" >&2; exit 1; }
export RICH_TYPING_BROWSER=webkit RICH_TYPING_WS="ws://127.0.0.1:$port/"
./run.sh "${@:-85-rich-typing}"
