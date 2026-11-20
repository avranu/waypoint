#!/bin/bash
# Starts, restarts or stops the throwaway server the suites run against.
#   server.sh start|restart|stop        (DIR and PORT come from run.sh)
set -euo pipefail
: "${DIR:?}" "${PORT:?}" "${BIN:?}"
stop() { [ -f "$DIR/pid" ] && kill "$(cat "$DIR/pid")" 2>/dev/null || true; rm -f "$DIR/pid"; sleep 0.3; }
case "${1:-start}" in
stop) stop; exit 0 ;;
restart) stop ;;
esac
if [ "${1:-start}" = start ] && curl -sf "127.0.0.1:$PORT/healthz" >/dev/null; then
  echo "port $PORT is already served by something else; set PORT to a free one" >&2
  exit 1
fi
WAYPOINT_DB="$DIR/wp.db" WAYPOINT_ADDR="127.0.0.1:$PORT" WAYPOINT_BASE_URL="http://127.0.0.1:$PORT" \
  WAYPOINT_BACKUP_DIR=off WAYPOINT_LOG_FORMAT=text WAYPOINT_TRUST_PROXY=127.0.0.1 \
  "$BIN" serve >>"$DIR/server.log" 2>&1 </dev/null &
echo $! >"$DIR/pid"
for _ in $(seq 1 50); do
  kill -0 "$(cat "$DIR/pid")" 2>/dev/null || { echo "server exited at start; see $DIR/server.log" >&2; exit 1; }
  curl -sf "127.0.0.1:$PORT/healthz" >/dev/null && exit 0; sleep 0.2; done
echo "server didn't start; see $DIR/server.log" >&2
exit 1
