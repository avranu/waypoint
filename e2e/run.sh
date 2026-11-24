#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"
# PORT defaults to a free one picked by the OS: CI runners host several jobs at
# once, and a fixed port let one job's suites talk to another job's server.
free_port() { node -e 'const s=require("net").createServer().listen(0,"127.0.0.1",()=>{console.log(s.address().port);s.close()})'; }
export BIN=${BIN:-$(realpath ../bin/waypoint)}
export DIR=${DIR:-$PWD/out}
rm -rf "$DIR" && mkdir -p "$DIR"
[ -d node_modules/playwright ] || npm ci --no-audit --no-fund
# a --shard lane runs after its parent installed: two installs side by side
# into ~/.cache/ms-playwright could race
[ -n "${E2E_LANE:-}" ] || npx playwright install chromium >/dev/null
if [ "${1:-}" = --shard ]; then
  # Each lane is its own run.sh with its own server, database and port, so lanes can run side by side.
  lanes=${E2E_LANES:-2}
  [[ $lanes =~ ^[1-9][0-9]*$ ]] || { echo "E2E_LANES=$lanes?" >&2; exit 2; }
  [[ ${2:-} =~ ^([0-9]+)/([0-9]+)$ ]] || { echo "--shard I/N, not '${2:-}'" >&2; exit 2; }
  i=${BASH_REMATCH[1]} n=${BASH_REMATCH[2]}
  pids=() ports=" "
  # a cancelled run takes its lanes, their browsers and servers with it
  # (all in this process group)
  trap 'trap - TERM INT; kill 0 2>/dev/null; exit 143' TERM INT
  for j in $(seq 1 "$lanes"); do
    # a later lane's failure must not leave the earlier lanes running: kill
    # the group, but not through the TERM trap, which would exit 143
    list=$(node shard.mjs "$(((i - 1) * lanes + j))/$((n * lanes))") || { trap '' TERM INT; kill 0 2>/dev/null; exit 2; }
    mapfile -t picked <<<"$list"
    [ -n "$list" ] || picked=()
    [ ${#picked[@]} -gt 0 ] || { echo "[lane $j] no suites"; continue; }
    port=$(free_port)
    while [[ $ports == *" $port "* ]]; do port=$(free_port); done
    ports+="$port "
    (E2E_LANE=$j DIR="$DIR/lane$j" PORT=$port ./run.sh "${picked[@]}" 2>&1 | sed -u "s/^/[lane $j] /") &
    pids+=($!)
  done
  [ ${#pids[@]} -gt 0 ] || { echo "shard.mjs picked no suites" >&2; exit 2; }
  fail=0
  for pid in "${pids[@]}"; do wait "$pid" || fail=1; done
  cat "$DIR"/lane*/timings.txt >"$DIR/timings.txt" 2>/dev/null || true
  [ ! -s "$DIR/timings.txt" ] || echo "total $(awk '{s+=$1} END {print s}' "$DIR/timings.txt")s over $(wc -l <"$DIR/timings.txt") suites in ${#pids[@]} lanes"
  exit $fail
fi
export PORT=${PORT:-$(free_port)}
trap './server.sh stop' EXIT
./server.sh start
base=http://127.0.0.1:$PORT
invite=$(grep -o "$base/invite/[A-Za-z0-9_-]*" "$DIR/server.log" | head -1)
fail=0
for f in [0-9]*.mjs; do
  if [ $# -gt 0 ] && [ "$f" != 01-signup-flow.mjs ]; then
    match=0; for p in "$@"; do [[ $f == "$p"* ]] && match=1; done
    [ $match = 1 ] || continue
  fi
  echo "== $f"
  case $f in
  01-*) args=("$base" "$invite" "$DIR/${f%.mjs}") ;;
  04-*) args=("$base" "$DIR/${f%.mjs}" "$PWD/server.sh restart") ;;
  *) args=("$base" "$DIR/${f%.mjs}") ;;
  esac
  mkdir -p "$DIR/${f%.mjs}"
  start=$SECONDS
  if ! node --import ./guard.mjs "$f" "${args[@]}" 2>&1 | tee "$DIR/${f%.mjs}.log"; then
    echo "FAILED: $f"
    fail=1
  fi
  echo "$((SECONDS - start)) ${f%.mjs}" >>"$DIR/timings.txt"
  echo "-- $f took $((SECONDS - start))s"
done
[ ! -f "$DIR/timings.txt" ] || echo "total $(awk '{s+=$1} END {print s}' "$DIR/timings.txt")s over $(wc -l <"$DIR/timings.txt") suites"
exit $fail
