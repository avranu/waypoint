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
# A suite that launches Chromium itself (no launchEngine, or one that needs a
# CDP session, which only Chromium has) would run on Chromium and be reported
# as a WebKit run: refuse it. 01-signup-flow, which run.sh always runs first,
# is the setup that makes the account, not a check of the editor.
for a in "${@:-85-rich-typing}"; do
  for f in "$a"*.mjs; do
    [ -e "$f" ] || { echo "webkit.sh: no suite $a" >&2; exit 1; }
    [ "$f" != 01-signup-flow.mjs ] || continue
    if ! grep -q 'await launchEngine()' "$f" || grep -Eq '\bchromium\b|newCDPSession' "$f"; then
      echo "webkit.sh: $f does not run on launchEngine's browser alone, so it cannot run under WebKit" >&2
      exit 1
    fi
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
