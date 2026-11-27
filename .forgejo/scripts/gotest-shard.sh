#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/../.."

declare -A seconds=(
  [cmd/waypoint]=5
  [internal/api]=190
  [internal/cilint]=40
  [internal/codeindex]=100
  [internal/core/kernel]=5
  [internal/core/meaning]=5
  [internal/core/numbering]=5
  [internal/core/udiff]=5
  [internal/db]=125
  [internal/depage]=5
  [internal/hermes]=5
  [internal/hermes/hermestest]=5
  [internal/hermes/hermestest/door]=5
  [internal/repocheck]=10
  [internal/sandbox/pushgate]=50
  [internal/sandbox/sandboxd]=65
  [internal/tracing]=5
  [internal/wplint]=110
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
while read -r kind a b wt; do
  if [ "$b" -ne "$shard" ]; then continue; fi
  case $kind in
  -) mine+=("./$a") ;;
  internal/core)
    run[$kind]+="${run[$kind]:+|}$a"
    core_weighted+=("$wt $a")
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
run_flags=()
has_cover=0
has_timeout=0
skip_next=0
argv=("$@")
for ((idx = 0; idx < ${#argv[@]}; idx++)); do
  if [ "$skip_next" -eq 1 ]; then
    skip_next=0
    continue
  fi
  arg=${argv[$idx]}
  case "$arg" in
  -timeout)
    has_timeout=1
    run_flags+=("-test.timeout=${argv[$((idx + 1))]}")
    skip_next=1
    ;;
  -timeout=*)
    has_timeout=1
    run_flags+=("-test.timeout=${arg#-timeout=}")
    ;;
  -count)
    run_flags+=("-test.count=${argv[$((idx + 1))]}")
    skip_next=1
    ;;
  -count=*)
    run_flags+=("-test.count=${arg#-count=}")
    ;;
  -parallel)
    run_flags+=("-test.parallel=${argv[$((idx + 1))]}")
    skip_next=1
    ;;
  -parallel=*)
    run_flags+=("-test.parallel=${arg#-parallel=}")
    ;;
  -cpu)
    run_flags+=("-test.cpu=${argv[$((idx + 1))]}")
    skip_next=1
    ;;
  -cpu=*)
    run_flags+=("-test.cpu=${arg#-cpu=}")
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
  -cover | -cover=* | -covermode | -covermode=*)
    has_cover=1
    build_flags+=("$arg")
    ;;
  *)
    build_flags+=("$arg")
    ;;
  esac
done
if [ -n "${EVIDENCE:-}" ] && [ "$has_cover" -eq 0 ]; then
  build_flags+=(-cover)
fi
if [ "$has_timeout" -eq 0 ]; then
  run_flags+=(-test.timeout=10m)
fi

# Run cmd/wp and rest packages while compiling internal/core once, then fan
# internal/core across CORE_LANES isolated sub-processes so multi-core runners
# don't sit idle on a single sequential core process. (go test -c builds the
# test-augmented internal/core package, which cmd/wp and internal/api cannot
# reuse anyway, so launching them immediately overlaps their work.)
if [ -n "${run[cmd/wp]:-}" ]; then
  # shellcheck disable=SC2046 # flags prints separate words
  go test "$@" $(flags cmd-wp) -run "^(${run[cmd/wp]})\$" ./cmd/wp >"$tmp/cmd-wp.out" 2>&1 &
  pids+=($!)
fi
if [ ${#mine[@]} -gt 0 ]; then
  # shellcheck disable=SC2046
  go test "$@" $(flags rest) "${mine[@]}" >"$tmp/rest.out" 2>&1 &
  pids+=($!)
fi

status=0
core_pids=()
core_t0=0
core_lanes=${CORE_LANES:-2}
if [ -n "${run[internal/core]:-}" ]; then
  if [ "$core_lanes" -gt 1 ] && [ "${#core_weighted[@]}" -gt 1 ]; then
    if go test -c ${build_flags[@]+"${build_flags[@]}"} -o "$tmp/core.test" ./internal/core >"$tmp/core-build.out" 2>&1; then
      declare -a sub_run=()
      while read -r lane tname; do
        sub_run[lane]+="${sub_run[lane]:+|}$tname"
      done < <(
        printf '%s\n' "${core_weighted[@]}" | awk -v n="$core_lanes" '
          { best = 0; for (i = 1; i < n; i++) if (load[i] < load[best]) best = i
            load[best] += $1; print best, $2 }'
      )
      core_t0=$(date +%s%3N)
      for lane in "${!sub_run[@]}"; do
        if [ -n "${EVIDENCE:-}" ]; then
          (cd internal/core && go tool test2json -p "$mod/internal/core" -t "$tmp/core.test" -test.paniconexit0 -test.v=test2json "-test.coverprofile=$tmp/internal-core-$lane.cov" ${run_flags[@]+"${run_flags[@]}"} -test.run="^(${sub_run[$lane]})\$") >"$tmp/internal-core-$lane.out" 2>&1 &
        else
          (cd internal/core && "$tmp/core.test" -test.paniconexit0 ${run_flags[@]+"${run_flags[@]}"} -test.run="^(${sub_run[$lane]})\$") >"$tmp/internal-core-$lane.out" 2>&1 &
        fi
        core_pids+=($!)
      done
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

if [ ${#core_pids[@]} -gt 0 ]; then
  core_status=0
  for pid in "${core_pids[@]}"; do wait "$pid" || core_status=1; done
  [ "$core_status" -eq 0 ] || status=1
  core_ms=$(($(date +%s%3N) - core_t0))
  core_sec=$(awk -v ms="$core_ms" 'BEGIN { printf "%.3f", ms / 1000 }')
  if [ "$core_status" -eq 0 ]; then
    pkg_line="ok  \t$mod/internal/core\t${core_sec}s"
  else
    pkg_line="FAIL\t$mod/internal/core\t${core_sec}s"
  fi
  if [ -n "${EVIDENCE:-}" ]; then
    printf '{"Action":"output","Package":"%s/internal/core","Output":"%s\\n"}\n' "$mod" "$pkg_line" >"$tmp/internal-core-pkg.out"
  else
    printf '%b\n' "$pkg_line" >"$tmp/internal-core-pkg.out"
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
    if [ "$f" != "$tmp/core-build.out" ] && [ -s "$f" ]; then outs+=("$f"); fi
  done
  if [ ${#outs[@]} -gt 0 ] && go run ./cmd/ci-evidence "${args[@]}" "${outs[@]}" >"$tmp/evidence" 2>"$tmp/evidence.err"; then
    cat "$tmp/evidence"
    exit $status
  fi
  echo "ci-evidence failed, so this is the raw output and Waypoint keeps no evidence of it:" >&2
  cat "$tmp/evidence.err" 2>/dev/null >&2 || true
fi
cat "$tmp"/*.out 2>/dev/null || true
exit $status
