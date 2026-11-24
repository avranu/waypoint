#!/usr/bin/env bash
# The same script runs bare and in the VM, so the two timings compare like for like.
set -u

workload=${1:?workload}
phase=${2:?phase}
export GOTOOLCHAIN=local CGO_ENABLED=1
export PATH="$HOME/go/bin:${PATH}"

case "$workload/$phase" in
waypoint/warmup) dir=$HOME/waypoint cmd='go mod download && go build ./cmd/... ./internal/... && (cd web && npm ci --no-audit --no-fund)' ;;
waypoint/timed) dir=$HOME/waypoint cmd='make check' ;;
node/warmup) dir=$HOME/node cmd='npm ci --no-audit --no-fund' ;;
node/timed) dir=$HOME/node cmd='npm ci --no-audit --no-fund && npm test' ;;
*)
	echo "bench.sh: unknown $workload/$phase" >&2
	exit 2
	;;
esac

log=$HOME/bench-$workload-$phase.log
secs() { awk -v a="$1" -v b="$(date +%s.%N)" 'BEGIN{printf "%.1f", b-a}'; }

# golangci-lint's cache is emptied first, so bare runs start cold like each VM.
if [ "$workload/$phase" = waypoint/timed ]; then
	rm -rf "$HOME/.cache/golangci-lint"
	start=$(date +%s.%N)
	rc=0
	: >"$log"
	for t in test lint sast vuln; do
		t0=$(date +%s.%N)
		(cd "$dir" && make "$t") >>"$log" 2>&1
		r=$?
		echo "SPIKE_RESULT bench_waypoint_timed_${t}_secs $(secs "$t0")"
		[ "$r" = 0 ] || rc=$r
	done
	echo "SPIKE_RESULT bench_waypoint_timed_secs $(secs "$start")"
	echo "SPIKE_RESULT bench_waypoint_timed_rc $rc"
	[ "$rc" = 0 ] || tail -n 15 "$log" | sed 's/^/SPIKE_LOG /'
	exit 0
fi

start=$(date +%s.%N)
(cd "$dir" && bash -c "$cmd") >"$log" 2>&1
rc=$?
end=$(date +%s.%N)

echo "SPIKE_RESULT bench_${workload}_${phase}_secs $(awk -v a="$start" -v b="$end" 'BEGIN{printf "%.1f", b-a}')"
echo "SPIKE_RESULT bench_${workload}_${phase}_rc $rc"
if [ "$rc" != 0 ]; then
	tail -n 15 "$log" | sed 's/^/SPIKE_LOG /'
fi
exit 0
