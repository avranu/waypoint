#!/usr/bin/env bash
# Advisory checks (WP-1660): npm audit and govulncheck fail where someone can
# act on them. A new advisory used to turn every branch's full run red for code
# it didn't touch (#500, #501, #504, #506: 8 to 21 hours each, R-411, R-412).
# This script has no waiver list and doesn't lower the bar of an advisory;
# what changes is where it fails (main's npm audit is web/scripts/audit.mjs,
# which keeps its own reviewed waiver list; CI's govulncheck on main is `main`
# below):
#
#   advisories.sh check KIND pr    a pull request or merge queue stack: fail only
#                                  for advisories the tree has and origin/main
#                                  lacks (compared by id). The tree is checked
#                                  first; clean means main isn't looked at.
#                                  Findings that main has too pass with a
#                                  ::warning:: (main's `advisories` job fails).
#   advisories.sh check KIND main  fail on any finding.
#   advisories.sh ids KIND         read a tool's JSON on stdin, print its
#                                  findings as "ID<TAB>detail" lines.
#
# KIND is npm (npm audit, high and critical, from web/package-lock.json) or go
# (govulncheck, the findings whose vulnerable code is called: the level the
# plain `govulncheck` run exits 3 on).
#
# For tests: ADVISORIES_TREE_JSON and ADVISORIES_MAIN_JSON name files holding a
# tool's JSON output, read instead of running the tool on the tree and on main.
set -euo pipefail

# Run from the repository root whatever the caller's directory (npm run audit
# runs in web/). deploy/ is in the public mirror, which has no .forgejo/ (WP-1624):
# the mirror's `make vuln` and `npm run audit` run this too.
cd "$(git rev-parse --show-toplevel)"

govuln_pkgs=(./cmd/... ./internal/... ./web)

# ids KIND: the tool's JSON on stdin to sorted unique "ID<TAB>detail" lines.
ids() {
  case "$1" in
    npm)
      # A string in `via` names another vulnerable package; its advisories are
      # listed under that package.
      jq -r 'if .error then error("npm audit failed: " + (.error | tostring)) else . end
        | (.vulnerabilities // {}) | to_entries[].value.via[]
        | select(type == "object" and (.severity == "high" or .severity == "critical"))
        | "\((.url // .title) | split("/") | last)\t\(.name) \(.range): \(.title)"' | sort -u
      ;;
    go)
      # govulncheck -format json is a stream of objects. A finding whose first
      # trace frame has a function is called code; module and package level
      # findings are imported-only and don't fail the plain run.
      jq -rs '(map(select(.osv) | .osv | {key: .id, value: (.summary // "")}) | from_entries) as $sum
        | .[] | select(.finding) | .finding | select(.trace[0].function != null)
        | "\(.osv)\t\($sum[.osv] // "")"' | sort -u
      ;;
    *)
      echo "advisories.sh: unknown kind '$1' (npm or go)" >&2
      exit 2
      ;;
  esac
}

# scan KIND DIR OUT: run the tool on the tree in DIR, write its ids to OUT.
scan() {
  local kind=$1 dir=$2 out=$3 raw
  raw=$(mktemp)
  case "$kind" in
    npm)
      # npm audit exits 1 when it finds anything; the JSON is on stdout either way.
      (cd "$dir/web" && npm audit --json >"$raw") || [ -s "$raw" ] || {
        echo "advisories.sh: npm audit produced no output in $dir/web" >&2
        exit 2
      }
      ;;
    go)
      # -format json exits 0 whatever it finds; a non-zero status is a failed run.
      (cd "$dir" && govulncheck -format json "${govuln_pkgs[@]}" >"$raw") || {
        echo "advisories.sh: govulncheck failed in $dir" >&2
        exit 2
      }
      ;;
  esac
  ids "$kind" <"$raw" >"$out"
  rm -f "$raw"
}

# main_tree KIND: a copy of origin/main's tip to check, printed as a path.
# npm needs only web's package.json and package-lock.json; go needs the tree.
main_tree() {
  local kind=$1 dir
  git fetch -q --no-tags ${depth:+"$depth"} origin main
  dir=$(mktemp -d)
  case "$kind" in
    npm)
      mkdir "$dir/web"
      git show FETCH_HEAD:web/package.json >"$dir/web/package.json"
      git show FETCH_HEAD:web/package-lock.json >"$dir/web/package-lock.json"
      ;;
    go)
      rmdir "$dir"
      git worktree add -q --detach "$dir" FETCH_HEAD
      ;;
  esac
  echo "$dir"
}

check() {
  local kind=$1 mode=$2 work tree main new known
  work=$(mktemp -d)
  tree="$work/tree.ids" main="$work/main.ids"
  if [ -n "${ADVISORIES_TREE_JSON:-}" ]; then ids "$kind" <"$ADVISORIES_TREE_JSON" >"$tree"; else scan "$kind" . "$tree"; fi

  if [ ! -s "$tree" ]; then
    echo "$kind advisories: none"
    return 0
  fi
  if [ "$mode" = main ]; then
    echo "::error title=Advisories on main::$kind: $(cut -f1 "$tree" | paste -sd, -)"
    cat "$tree"
    return 1
  fi

  if [ -n "${ADVISORIES_MAIN_JSON:-}" ]; then
    ids "$kind" <"$ADVISORIES_MAIN_JSON" >"$main"
  else
    if [ "$(git rev-parse --is-shallow-repository)" = true ]; then depth=--depth=1; else depth=; fi
    if ! dir=$(main_tree "$kind") || ! scan "$kind" "$dir" "$main"; then
      echo "::error title=Advisories::$kind has findings and main's could not be read to compare; failing as plain $kind would"
      cat "$tree"
      return 1
    fi
    if [ "$kind" = go ]; then git worktree remove --force "$dir" || true; fi
  fi

  new=$(comm -23 <(cut -f1 "$tree") <(cut -f1 "$main"))
  if [ -n "$new" ]; then
    echo "::error title=New advisories::$kind: this change adds $(paste -sd, - <<<"$new") over main"
    grep -F -f <(printf '%s\n' "$new") "$tree" || true
    return 1
  fi
  known=$(cut -f1 "$tree" | paste -sd, -)
  echo "::warning title=Advisory on main::$kind: $known also on main, not added here; main's advisories job reports it."
  cat "$tree"
}

case "${1:-}" in
  ids) ids "${2:?kind}" ;;
  check)
    case "${3:-}" in pr | main) ;; *) echo "usage: advisories.sh check npm|go pr|main" >&2; exit 2 ;; esac
    check "${2:?kind}" "$3"
    ;;
  *)
    echo "usage: advisories.sh check npm|go pr|main | ids npm|go" >&2
    exit 2
    ;;
esac
