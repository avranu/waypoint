#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"
free_port() { node -e 'const s=require("net").createServer().listen(0,"127.0.0.1",()=>{console.log(s.address().port);s.close()})'; }
export BIN=${BIN:-$(realpath ../bin/waypoint)}
export DIR=${DIR:-$PWD/out}
if [ "${E2E_SEEDED:-}" = 1 ]; then
  mkdir -p "$DIR"
else
  rm -rf "$DIR" && mkdir -p "$DIR"
fi
[ -d node_modules/playwright ] || npm ci --no-audit --no-fund
[ -n "${E2E_LANE:-}" ] || npx playwright install chromium >/dev/null
if [ "${1:-}" = --shard ]; then
  lanes=${E2E_LANES:-3}
  [[ $lanes =~ ^[1-9][0-9]*$ ]] || { echo "E2E_LANES=$lanes?" >&2; exit 2; }
  [[ ${2:-} =~ ^([0-9]+)/([0-9]+)$ ]] || { echo "--shard I/N, not '${2:-}'" >&2; exit 2; }
  i=${BASH_REMATCH[1]} n=${BASH_REMATCH[2]}
  pids=() ports=" " browser_pid=""
  cleanup_shard() {
    local code=$?
    trap - TERM INT EXIT
    if [ -n "$browser_pid" ]; then
      kill "$browser_pid" 2>/dev/null || true
      wait "$browser_pid" 2>/dev/null || true
    fi
    for pid in "${pids[@]}"; do
      kill "$pid" 2>/dev/null || true
    done
    exit "$code"
  }
  trap 'cleanup_shard' EXIT
  trap 'trap - TERM INT; kill 0 2>/dev/null; exit 143' TERM INT

  # Start one shared Chromium browser server for the shard so suites connect
  # instead of cold-launching Chromium 25-55 times per shard.
  node -e '
    import("playwright").then(async ({ chromium }) => {
      const srv = await chromium.launchServer();
      process.stdout.write(srv.wsEndpoint() + "\n");
      const stop = async () => { await srv.close().catch(() => {}); process.exit(0); };
      process.on("SIGTERM", stop);
      process.on("SIGINT", stop);
    }).catch((err) => {
      console.error(err);
      process.exit(1);
    });
  ' >"$DIR/browser.ws" 2>"$DIR/browser.err" &
  browser_pid=$!
  ws=""
  for _ in $(seq 1 150); do
    if [ -s "$DIR/browser.ws" ]; then
      ws=$(head -n 1 "$DIR/browser.ws")
      break
    fi
    kill -0 "$browser_pid" 2>/dev/null || break
    sleep 0.1
  done
  if [ -n "$ws" ]; then
    export E2E_CHROMIUM_WS="$ws"
  else
    echo "warning: shared chromium server did not start; falling back to per-suite launch" >&2
    cat "$DIR/browser.err" >&2 || true
  fi

  # Run 01-signup-flow once per shard and copy its cleanly closed SQLite DB to
  # each parallel lane instead of running signup once per lane.
  seed_port=$(free_port)
  ports+="$seed_port "
  E2E_LANE=seed DIR="$DIR/seed" PORT=$seed_port ./run.sh 01-signup-flow.mjs 2>&1 | sed -u "s/^/[seed] /"

  for j in $(seq 1 "$lanes"); do
    list=$(node shard.mjs "$(((i - 1) * lanes + j))/$((n * lanes))") || { trap '' TERM INT EXIT; kill 0 2>/dev/null; exit 2; }
    mapfile -t picked <<<"$list"
    [ -n "$list" ] || picked=()
    [ ${#picked[@]} -gt 0 ] || { echo "[lane $j] no suites"; continue; }
    port=$(free_port)
    while [[ $ports == *" $port "* ]]; do port=$(free_port); done
    ports+="$port "
    mkdir -p "$DIR/lane$j"
    cp "$DIR/seed/wp.db" "$DIR/lane$j/wp.db"
    for ext in -wal -shm; do
      [ -f "$DIR/seed/wp.db$ext" ] && cp "$DIR/seed/wp.db$ext" "$DIR/lane$j/wp.db$ext" || true
    done
    (E2E_LANE=$j E2E_SEEDED=1 DIR="$DIR/lane$j" PORT=$port ./run.sh "${picked[@]}" 2>&1 | sed -u "s/^/[lane $j] /") &
    pids+=($!)
  done
  [ ${#pids[@]} -gt 0 ] || { echo "shard.mjs picked no suites" >&2; exit 2; }
  fail=0
  for pid in "${pids[@]}"; do wait "$pid" || fail=1; done
  cat "$DIR"/seed/timings.txt "$DIR"/lane*/timings.txt >"$DIR/timings.txt" 2>/dev/null || true
  [ ! -s "$DIR/timings.txt" ] || echo "total $(awk '{s+=$1} END {print s}' "$DIR/timings.txt")s over $(wc -l <"$DIR/timings.txt") suites in ${#pids[@]} lanes"
  exit $fail
fi
export PORT=${PORT:-$(free_port)}
trap './server.sh stop' EXIT
./server.sh start
base=http://127.0.0.1:$PORT
invite=$(grep -o "$base/invite/[A-Za-z0-9_-]*" "$DIR/server.log" | head -1 || true)
fail=0
for f in [0-9]*.mjs; do
  if [ "${E2E_SEEDED:-}" = 1 ] && [ "$f" = 01-signup-flow.mjs ]; then
    continue
  fi
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
