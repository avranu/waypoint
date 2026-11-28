#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/../.."

declare -A seconds=(
  [cmd/waypoint]=5
  [internal/api]=110
  [internal/cilint]=35
  [internal/codeindex]=80
  [internal/core/email]=5
  [internal/core/kernel]=5
  [internal/core/kernel/kerneltest]=5
  [internal/core/meaning]=5
  [internal/core/numbering]=5
  [internal/core/strength]=5
  [internal/core/udiff]=45
  [internal/core/users]=10
  [internal/coremap]=5
  [internal/db]=50
  [internal/depage]=5
  [internal/hermes]=5
  [internal/hermes/hermestest]=5
  [internal/hermes/hermestest/door]=5
  [internal/repocheck]=10
  [internal/sandbox/pushgate]=40
  [internal/sandbox/sandboxd]=15
  [internal/tracing]=5
  [internal/wplint]=60
  [internal/wql]=5
  [web]=10
)
splits=(internal/core cmd/wp)
weights=.forgejo/scripts/gotest-weights.txt
mod=$(go list -m)

tests() {
  grep -h -E '^func Test[A-Z_0-9][A-Za-z0-9_]*\(' "$1"/*_test.go |
    grep -v -E '^func TestMain\([^)]*\*[^)]*\.M\)' |
    sed -E 's/^func (Test[A-Z_0-9][A-Za-z0-9_]*).*/\1/' | LC_ALL=C sort
}

if [ "${1:-}" = -check ]; then
  tmpbin=$(mktemp -d)
  trap 'rm -rf "$tmpbin"' EXIT
  go build -o "$tmpbin/ci-evidence" ./cmd/ci-evidence
  for p in "${splits[@]}"; do
    want=$(tests "$p")
    got=$("$tmpbin/ci-evidence" -list-tests "./$p" | LC_ALL=C sort)
    if [ -z "$got" ]; then
      echo "AST test list found no tests in $p" >&2
      exit 1
    fi
    missing=$(LC_ALL=C comm -13 <(echo "$want") <(echo "$got"))
    if [ -n "$missing" ]; then
      echo "$missing"
      echo "AST test list has tests of $p the grep misses: no shard would run them" >&2
      exit 1
    fi
    echo "$(wc -l <<<"$got") tests in $p, all found by the grep"
  done
  exit 0
fi

shard=${1:?usage: gotest-shard.sh I N [go test flags...]}
shards=${2:?usage: gotest-shard.sh I N [go test flags...]}
shift 2
if ! [[ $shard =~ ^[0-9]+$ && $shards =~ ^[0-9]+$ ]] || [ "$shard" -lt 1 ] || [ "$shard" -gt "$shards" ]; then
  echo "shard $shard of $shards?" >&2
  exit 2
fi
shard=$((shard - 1))

# Shard N-1 runs govulncheck and suppression scanning after tests in ci.yml,
# so seed its rest-package load so it gets the lightest extra packages.
plan=$(
  {
    go list ./cmd/... ./internal/... ./web | sed "s|^$mod/||" | grep -v -x -F "${splits[@]/#/-e}" |
      while read -r p; do echo "- ${seconds[$p]:-20} $p"; done
    for p in "${splits[@]}"; do
      tests "$p" | awk -v p="$p" 'FILENAME != "-" { if ($2 == p) w[$3] = $1; next } { print p, (w[$1] ? w[$1] : 1), $1 }' \
        <(grep -v '^#' "$weights") -
    done
  } | LC_ALL=C sort -k1,1 -k2,2nr -k3,3 |
    awk -v n="$shards" '
      BEGIN { if (n > 1) load["-", n - 1] = 60 }
      { k = $1; best = 0; for (i = 1; i < n; i++) if (load[k, i] < load[k, best]) best = i
        load[k, best] += $2; print $1, $3, best, $2 }'
)

declare -a mine=()
declare -A run=()
declare -a core_weighted=()
declare -a wp_weighted=()
while read -r kind a b wt; do
  if [ "$b" -ne "$shard" ]; then continue; fi
  case $kind in
  -) mine+=("./$a") ;;
  internal/core)
    run[$kind]+="${run[$kind]:+|}$a"
    core_weighted+=("$wt $a")
    ;;
  cmd/wp)
    run[$kind]+="${run[$kind]:+|}$a"
    wp_weighted+=("$wt $a")
    ;;
  *) run[$kind]+="${run[$kind]:+|}$a" ;;
  esac
done <<<"$plan"

summary=
for p in "${splits[@]}"; do
  n=0
  if [ -n "${run[$p]:-}" ]; then n=$(tr '|' '\n' <<<"${run[$p]}" | wc -l); fi
  summary+="$p tests $n, "
done
echo "shard $((shard + 1)) of $shards: ${summary}packages: ${mine[*]:-none}"

[ -z "${DRY_RUN:-}" ] || exit 0
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
pids=()
flags() {
  if [ -n "${EVIDENCE:-}" ]; then
    echo -json "-coverprofile=$tmp/$1.cov"
  fi
}

# Split go test flags into build-time vs binary-run-time for compiled test binaries.
build_flags=()
plain_build_flags=()
run_flags=()
has_cover=0
has_timeout=0
argv=("$@")
skip_next=0
for idx in "${!argv[@]}"; do
  if [ "$skip_next" -eq 1 ]; then
    skip_next=0
    continue
  fi
  arg=${argv[$idx]}
  case "$arg" in
  -count)
    run_flags+=("-test.count=${argv[$((idx + 1))]}")
    skip_next=1
    ;;
  -count=*)
    run_flags+=("-test.count=${arg#-count=}")
    ;;
  -timeout)
    has_timeout=1
    run_flags+=("-test.timeout=${argv[$((idx + 1))]}")
    skip_next=1
    ;;
  -timeout=*)
    has_timeout=1
    run_flags+=("-test.timeout=${arg#-timeout=}")
    ;;
  -cpu)
    run_flags+=("-test.cpu=${argv[$((idx + 1))]}")
    skip_next=1
    ;;
  -cpu=*)
    run_flags+=("-test.cpu=${arg#-cpu=}")
    ;;
  -parallel)
    run_flags+=("-test.parallel=${argv[$((idx + 1))]}")
    skip_next=1
    ;;
  -parallel=*)
    run_flags+=("-test.parallel=${arg#-parallel=}")
    ;;
  -short)
    run_flags+=("-test.short")
    ;;
  -failfast)
    run_flags+=("-test.failfast")
    ;;
  -v)
    run_flags+=("-test.v")
    ;;
  -covermode)
    has_cover=1
    build_flags+=("$arg" "${argv[$((idx + 1))]}")
    skip_next=1
    ;;
  -cover | -cover=* | -covermode=*)
    has_cover=1
    build_flags+=("$arg")
    ;;
  *)
    build_flags+=("$arg")
    plain_build_flags+=("$arg")
    ;;
  esac
done
if [ -n "${EVIDENCE:-}" ] && [ "$has_cover" -eq 0 ]; then
  build_flags+=(-cover)
fi
if [ "$has_timeout" -eq 0 ]; then
  run_flags+=(-test.timeout=10m)
fi

status=0
core_build_pid=
core_lanes=${CORE_LANES:-3}
wp_lanes=${WP_LANES:-2}

fan_out_binary() {
  local pkg_dir=$1 bin=$2 prefix=$3 lanes=$4
  shift 4
  local -a weighted=("$@")
  local -a sub_run=()
  while read -r lane tname; do
    sub_run[lane]+="${sub_run[lane]:+|}$tname"
  done < <(
    printf '%s\n' "${weighted[@]}" | awk -v n="$lanes" '
      { best = 0; for (i = 1; i < n; i++) if (load[i] < load[best]) best = i
        load[best] += $1; print best, $2 }'
  )
  local t0 lane_pids=() lane_status=0
  t0=$(date +%s%3N)
  for lane in "${!sub_run[@]}"; do
    if [ -n "${EVIDENCE:-}" ]; then
      (cd "$pkg_dir" && go tool test2json -p "$mod/$pkg_dir" -t "$bin" -test.paniconexit0 -test.v=test2json "-test.coverprofile=$tmp/$prefix-$lane.cov" ${run_flags[@]+"${run_flags[@]}"} -test.run="^(${sub_run[$lane]})\$") >"$tmp/$prefix-$lane.out" 2>&1 &
    else
      (cd "$pkg_dir" && "$bin" -test.paniconexit0 ${run_flags[@]+"${run_flags[@]}"} -test.run="^(${sub_run[$lane]})\$") >"$tmp/$prefix-$lane.out" 2>&1 &
    fi
    lane_pids+=($!)
  done
  for pid in "${lane_pids[@]}"; do wait "$pid" || lane_status=1; done
  local ms sec pkg_line
  ms=$(($(date +%s%3N) - t0))
  sec=$(awk -v ms="$ms" 'BEGIN { printf "%.3f", ms / 1000 }')
  if [ "$lane_status" -eq 0 ]; then
    pkg_line="ok  \t$mod/$pkg_dir\t${sec}s"
  else
    pkg_line="FAIL\t$mod/$pkg_dir\t${sec}s"
  fi
  if [ -n "${EVIDENCE:-}" ]; then
    printf '{"Action":"output","Package":"%s/%s","Output":"%s\\n"}\n' "$mod" "$pkg_dir" "$pkg_line" >"$tmp/$prefix-pkg.out"
  else
    printf '%b\n' "$pkg_line" >"$tmp/$prefix-pkg.out"
  fi
  return "$lane_status"
}

# Start compiling the test-augmented internal/core binary immediately so it
# overlaps with cmd/wp and rest. When both cmd/wp and a rest package (e.g.
# internal/api or cmd/waypoint) import plain internal/core, pre-warm plain
# internal/core once into GOCACHE before launching both so they don't duplicate
# that compile concurrently.
if [ -n "${run[internal/core]:-}" ] && [ "$core_lanes" -gt 1 ] && [ "${#core_weighted[@]}" -gt 1 ]; then
  go test -c ${build_flags[@]+"${build_flags[@]}"} -o "$tmp/core.test" ./internal/core >"$tmp/core-build.out" 2>&1 &
  core_build_pid=$!
fi

if [ -n "${run[cmd/wp]:-}" ] && [ ${#mine[@]} -gt 0 ] &&
  go list -f '{{range .Deps}}{{if eq . "'"$mod"'/internal/core"}}1{{end}}{{end}}' "${mine[@]}" 2>/dev/null | grep -q 1; then
  go build ${plain_build_flags[@]+"${plain_build_flags[@]}"} ./internal/core >/dev/null 2>&1 || true
fi

if [ -n "${run[cmd/wp]:-}" ]; then
  if [ "$wp_lanes" -gt 1 ] && [ "${#wp_weighted[@]}" -gt 1 ]; then
    (
      if go test -c ${build_flags[@]+"${build_flags[@]}"} -o "$tmp/wp.test" ./cmd/wp >"$tmp/wp-build.out" 2>&1; then
        fan_out_binary "cmd/wp" "$tmp/wp.test" "cmd-wp" "$wp_lanes" "${wp_weighted[@]}"
      else
        cat "$tmp/wp-build.out" >&2
        printf 'FAIL\t%s/cmd/wp [build failed]\n' "$mod" >"$tmp/cmd-wp-pkg.out"
        exit 1
      fi
    ) &
    pids+=($!)
  else
    # shellcheck disable=SC2046 # flags prints separate words
    go test "$@" $(flags cmd-wp) -run "^(${run[cmd/wp]})\$" ./cmd/wp >"$tmp/cmd-wp.out" 2>&1 &
    pids+=($!)
  fi
fi

if [ ${#mine[@]} -gt 0 ]; then
  # shellcheck disable=SC2046
  go test "$@" $(flags rest) "${mine[@]}" >"$tmp/rest.out" 2>&1 &
  pids+=($!)
fi

if [ -n "${run[internal/core]:-}" ]; then
  if [ -n "$core_build_pid" ]; then
    if wait "$core_build_pid"; then
      fan_out_binary "internal/core" "$tmp/core.test" "internal-core" "$core_lanes" "${core_weighted[@]}" || status=1
    else
      cat "$tmp/core-build.out" >&2
      printf 'FAIL\t%s/internal/core [build failed]\n' "$mod" >"$tmp/internal-core-pkg.out"
      status=1
    fi
  else
    # shellcheck disable=SC2046
    go test "$@" $(flags internal-core) -run "^(${run[internal/core]})\$" ./internal/core >"$tmp/internal-core.out" 2>&1 &
    pids+=($!)
  fi
fi

for pid in "${pids[@]}"; do wait "$pid" || status=1; done
if [ -n "${EVIDENCE:-}" ]; then
  args=(-shard "$((shard + 1))/$shards")
  for f in "$tmp"/*.cov; do
    if [ -s "$f" ]; then args+=(-cover "$f"); fi
  done
  if [ -n "${SUPPRESSIONS:-}" ]; then args+=(-suppressions); fi
  outs=()
  for f in "$tmp"/*.out; do
    if [ "$f" != "$tmp/core-build.out" ] && [ "$f" != "$tmp/wp-build.out" ] && [ -s "$f" ]; then outs+=("$f"); fi
  done
  if [ ${#outs[@]} -gt 0 ] && go run ./cmd/ci-evidence "${args[@]}" "${outs[@]}" >"$tmp/evidence" 2>"$tmp/evidence.err"; then
    cat "$tmp/evidence"
    exit $status
  fi
  cat "$tmp/evidence.err" >&2 2>/dev/null || true
fi
for f in "$tmp"/*.out; do
  if [ "$f" != "$tmp/core-build.out" ] && [ "$f" != "$tmp/wp-build.out" ]; then cat "$f"; fi
done
exit $status
