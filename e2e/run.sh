#!/bin/bash
# Runs every suite against a fresh server with an empty database.
#   ./run.sh                 all suites (BIN defaults to ../bin/waypoint; `make build` first)
#   ./run.sh 05 07           only suites whose names start with these
#   ./run.sh --shard 2/5     the second of five time-balanced shards (shard.mjs), in two lanes
#                            side by side (E2E_LANES, below); CI runs one shard per job
# E2E_LOOKS=one runs each suite's functional checks at one viewport and theme
# instead of all four looks (looks.mjs); CI does that, and weekly with `all`.
# Set WAYPOINT_AI_OPENCODE_URL to an `opencode serve` to include the AI checks.
# Screenshots and the server log land in e2e/out/ (out/laneJ/ for a --shard).
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
  # A shard's suites run in E2E_LANES lanes side by side (default 2). Each
  # lane is a run.sh of its own: its own server, database, port, out/laneJ
  # and 01-signup-flow. The suites mostly wait on the browser and the server,
  # so two lanes take a little over half the time of one (WP-402). Lane J of
  # shard I/N is shard (I-1)*LANES+J of N*LANES, so shard.mjs balances every
  # lane of every shard at once. Each output line starts with "[lane J]".
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
    # an empty lane must not turn into "run everything"
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
  # pipefail: a failing suite fails the pipeline despite tee.
  if ! node --import ./guard.mjs "$f" "${args[@]}" 2>&1 | tee "$DIR/${f%.mjs}.log"; then
    echo "FAILED: $f"
    fail=1
  fi
  echo "$((SECONDS - start)) ${f%.mjs}" >>"$DIR/timings.txt"
  echo "-- $f took $((SECONDS - start))s"
done
[ ! -f "$DIR/timings.txt" ] || echo "total $(awk '{s+=$1} END {print s}' "$DIR/timings.txt")s over $(wc -l <"$DIR/timings.txt") suites"
exit $fail
