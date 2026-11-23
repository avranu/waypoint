#!/usr/bin/env bash
# Advisory checks (WP-1660): npm audit and govulncheck fail where someone can
# act on them. A new advisory used to turn every branch's full run red for code
# it didn't touch (#500, #501, #504, #506: 8 to 21 hours each, R-411, R-412).
# This script doesn't lower the bar of an advisory; what changes is where it
# fails. Its only waivers are web/scripts/audit.mjs's reviewed list (with an
# expiry, a person's decision), which a pull request that adds a waived npm
# advisory is judged by, and which main's npm audit (`npm run audit`) keeps;
# CI's govulncheck on main is `main` below:
#
#   advisories.sh check KIND pr    a pull request or merge queue stack: fail only
#                                  for advisories this change adds (compared
#                                  by id): ones the tree has, origin/main's
#                                  tip lacks, and the commit the branch left
#                                  main at lacks too (a branch cut before main
#                                  fixed one didn't add it). The tree is
#                                  checked first; clean means main isn't
#                                  looked at. Findings the change didn't add
#                                  pass with a ::warning:: (main's
#                                  `advisories` job fails on main's). An npm
#                                  advisory the change adds passes when
#                                  `npm run audit -- --only ID,ID` does, with
#                                  the ids the change adds and no others: the
#                                  change waives each by web/scripts/audit.mjs's
#                                  rules, whatever else the tree has. npm
#                                  is skipped when web/package.json and
#                                  web/package-lock.json are unchanged from
#                                  where the branch left main (nothing it could
#                                  add); go never is, as govulncheck reports
#                                  called code, which Go changes alone can add.
#   advisories.sh check KIND main  fail on any finding.
#   advisories.sh ids KIND         read a tool's JSON on stdin, print its
#                                  findings as "ID<TAB>detail" lines.
#
# KIND is npm (npm audit, high and critical, from web/package-lock.json) or go
# (govulncheck, the findings whose vulnerable code is called: the level the
# plain `govulncheck` run exits 3 on).
#
# ADVISORIES_BASE_SHA: the commit the branch left main at, for a checkout too
# shallow to work it out (CI passes the pull request's merge base).
#
# Needs jq, git and (for a scan) npm or govulncheck.
#
# For tests: ADVISORIES_TREE_JSON, ADVISORIES_MAIN_JSON and ADVISORIES_BASE_JSON
# name files holding a tool's JSON output, read instead of running the tool on
# the tree, on main and on the branch's base (no ADVISORIES_BASE_JSON with
# ADVISORIES_MAIN_JSON: no base is looked at; with ADVISORIES_TREE_JSON the
# unchanged-dependency-files skip is off); ADVISORIES_AUDIT_CMD replaces
# `npm --prefix web run -s audit` and gets the same `-- --only IDS` appended.
set -euo pipefail

# Every kind runs jq on the tool's JSON; say so before the first use fails.
command -v jq >/dev/null 2>&1 || {
  echo "advisories.sh: jq is needed" >&2
  exit 2
}

# Run from the repository root whatever the caller's directory (npm run audit
# runs in web/). deploy/ is in the public mirror, which has no .forgejo/ (WP-1624):
# the mirror's `make vuln` runs this, and this runs `npm run audit` for an npm
# advisory a change adds.
cd "$(git rev-parse --show-toplevel)"

# The Makefile's GOPKGS (internal/cilint checks they agree).
govuln_pkgs=(./cmd/... ./internal/... ./web)

# Everything temporary is under $work; worktrees made for a scan are listed in
# $work/worktrees and removed with it, however the script ends.
work=$(mktemp -d)
cleanup() {
  if [ -f "$work/worktrees" ]; then
    while IFS= read -r w; do git worktree remove --force "$w" >/dev/null 2>&1 || true; done <"$work/worktrees"
  fi
  rm -rf "$work"
}
trap cleanup EXIT

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
# Non-zero when the tool didn't run or its output can't be read.
scan() {
  local kind=$1 dir=$2 out=$3 raw
  raw=$(mktemp -p "$work")
  case "$kind" in
    npm)
      # npm audit exits 1 when it finds anything; the JSON is on stdout either way.
      (cd "$dir/web" && npm audit --json >"$raw") || [ -s "$raw" ] || {
        echo "advisories.sh: npm audit produced no output in $dir/web" >&2
        return 2
      }
      ;;
    go)
      # -format json exits 0 whatever it finds; a non-zero status is a failed run.
      (cd "$dir" && govulncheck -format json "${govuln_pkgs[@]}" >"$raw") || {
        echo "advisories.sh: govulncheck failed in $dir" >&2
        return 2
      }
      ;;
  esac
  ids "$kind" <"$raw" >"$out" || return 2
}

# ref_tree KIND REF DIR: a copy of commit REF to check, in DIR. npm needs only
# web's package.json and package-lock.json; go needs the tree.
ref_tree() {
  local kind=$1 ref=$2 dir=$3
  case "$kind" in
    npm)
      mkdir -p "$dir/web" || return 1
      git show "$ref:web/package.json" >"$dir/web/package.json" || return 1
      git show "$ref:web/package-lock.json" >"$dir/web/package-lock.json" || return 1
      ;;
    go)
      echo "$dir" >>"$work/worktrees"
      git worktree add -q --detach "$dir" "$ref" || return 1
      ;;
  esac
}

# ref_ids KIND REF NAME: the ids of commit REF, to $work/NAME.ids.
ref_ids() {
  ref_tree "$1" "$2" "$work/$3" && scan "$1" "$work/$3" "$work/$3.ids"
}

# base_commit MAIN: the commit the branch left main at: ADVISORIES_BASE_SHA,
# else the merge base with MAIN (a ref or commit; none when empty), fetched
# when the checkout lacks it. Fails when it can't be found.
base_commit() {
  local main=$1 base
  base=${ADVISORIES_BASE_SHA:-}
  if [ -z "$base" ] && [ -n "$main" ]; then base=$(git merge-base HEAD "$main" 2>/dev/null || true); fi
  [ -n "$base" ] || return 1
  git cat-file -e "$base^{commit}" 2>/dev/null || git fetch -q --no-tags ${depth:+"$depth"} origin "$base" 2>/dev/null || return 1
  git rev-parse "$base^{commit}"
}

# base_sha MAIN: the commit the branch left main at, when it isn't MAIN or
# HEAD (then main's tip, or the tree, is the base already) and can be read.
base_sha() {
  local main=$1 base head
  head=$(git rev-parse HEAD)
  base=$(base_commit "$main") || return 1
  [ "$base" != "$main" ] && [ "$base" != "$head" ] || return 1
  echo "$base"
}

# only_in A B: the ids of A that B lacks.
only_in() {
  comm -23 <(cut -f1 "$1" | sort -u) <(cut -f1 "$2" | sort -u)
}

# deps_unchanged: web/package.json and web/package-lock.json are the same at
# HEAD as where the branch left main, so an npm audit can't find anything the
# change added (WP-1660: the check runs "when it changes a dependency file").
# False when the base can't be found: the scan then runs as it always did.
# Not for go: govulncheck reports vulnerable code that is called, so a change
# to Go code alone can add a finding.
deps_unchanged() {
  local main='' base
  if git rev-parse -q --verify refs/remotes/origin/main >/dev/null 2>&1; then main=origin/main; fi
  base=$(base_commit "$main" 2>/dev/null) || return 1
  git diff --quiet "$base" HEAD -- web/package.json web/package-lock.json
}

# details IDS FILE: FILE's lines whose id is one of IDS (one a line).
details() {
  awk -F'\t' 'NR == FNR { want[$1]; next } $1 in want' <(printf '%s\n' "$1") "$2"
}

check() {
  local kind=$1 mode=$2 tree main new known main_sha base depth
  tree="$work/tree.ids" main="$work/main.ids"
  if [ "$(git rev-parse --is-shallow-repository)" = true ]; then depth=--depth=1; else depth=; fi
  # ADVISORIES_TREE_JSON stands in for the tree in tests: they test the comparison.
  if [ "$kind" = npm ] && [ "$mode" = pr ] && [ -z "${ADVISORIES_TREE_JSON:-}" ] && deps_unchanged; then
    echo "npm advisories: web/package.json and web/package-lock.json unchanged from main; nothing this change could add"
    return 0
  fi
  if [ -n "${ADVISORIES_TREE_JSON:-}" ]; then ids "$kind" <"$ADVISORIES_TREE_JSON" >"$tree"; else scan "$kind" . "$tree"; fi

  if [ ! -s "$tree" ]; then
    echo "$kind advisories: none"
    return 0
  fi
  if [ "$mode" = main ]; then
    echo "::error title=Advisories on main::$kind: $(cut -f1 "$tree" | sort -u | paste -sd, -)"
    cat "$tree"
    return 1
  fi

  base=
  if [ -n "${ADVISORIES_MAIN_JSON:-}" ]; then
    ids "$kind" <"$ADVISORIES_MAIN_JSON" >"$main"
  else
    # A fetch that failed must not leave an older FETCH_HEAD to stand in for main.
    if ! git fetch -q --no-tags ${depth:+"$depth"} origin main || ! main_sha=$(git rev-parse FETCH_HEAD) || ! ref_ids "$kind" "$main_sha" main; then
      echo "::error title=Advisories::$kind has findings and main's could not be read to compare; failing as plain $kind would"
      cat "$tree"
      return 1
    fi
    base=$(base_sha "$main_sha") || base=
  fi

  new=$(only_in "$tree" "$main")
  # Ids main's tip lacks: the branch added them, or it left main before main
  # fixed them. The commit it left main at tells which.
  if [ -n "$new" ]; then
    if [ -n "${ADVISORIES_BASE_JSON:-}" ]; then
      ids "$kind" <"$ADVISORIES_BASE_JSON" >"$work/base.ids"
    elif [ -z "$base" ] || ! ref_ids "$kind" "$base" base; then
      rm -f "$work/base.ids"
    fi
    if [ -f "$work/base.ids" ]; then
      details "$new" "$tree" >"$work/new.ids"
      known=$(comm -12 <(cut -f1 "$work/new.ids" | sort -u) <(cut -f1 "$work/base.ids" | sort -u) | paste -sd, -)
      new=$(only_in "$work/new.ids" "$work/base.ids")
      if [ -n "$known" ]; then
        echo "::warning title=Advisory fixed on main::$kind: $known is in this branch from before it left main, and main has fixed it since; not added here. Merging main drops it."
      fi
    fi
  fi
  if [ -n "$new" ] && [ "$kind" = npm ]; then
    # The change may add an advisory together with its reviewed waiver. Only
    # the ids it adds are judged: another one main has too, and no waiver, must
    # not fail them.
    if ${ADVISORIES_AUDIT_CMD-npm --prefix web run -s audit} -- --only "$(paste -sd, - <<<"$new")"; then
      echo "::warning title=Waived advisories::npm: this change adds $(paste -sd, - <<<"$new") over main, waived by web/scripts/audit.mjs"
      new=
    fi
  fi
  if [ -n "$new" ]; then
    echo "::error title=New advisories::$kind: this change adds $(paste -sd, - <<<"$new") over main"
    details "$new" "$tree"
    return 1
  fi
  known=$(comm -12 <(cut -f1 "$tree" | sort -u) <(cut -f1 "$main" | sort -u) | paste -sd, -)
  if [ -n "$known" ]; then
    echo "::warning title=Advisory on main::$kind: $known also on main, not added here; main's advisories job reports it."
  fi
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
