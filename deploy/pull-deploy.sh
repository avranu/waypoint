#!/usr/bin/env bash
# CI holds no key to production: the deployer deploys the newest vX.Y.Z tag only if it is on main, descends from the live commit, is published, and was not tried before.
set -euo pipefail

CONF=${WAYPOINT_DEPLOYER_CONF:-$HOME/.config/waypoint-deployer}
STATE=${WAYPOINT_DEPLOYER_STATE:-$HOME/.local/state/waypoint-deployer}
BIN=${WAYPOINT_DEPLOYER_BIN:-$HOME/bin}
ROOT=${WAYPOINT_DEPLOY_ROOT:-$HOME}
DEPLOY=${WAYPOINT_DEPLOY_PROGRAM:-$BIN/waypoint-deploy}
RECORD=${WAYPOINT_RECORD_PROGRAM:-$BIN/waypoint-record-deployment}
MIRROR=$STATE/repo.git
TRIED=$STATE/tried
LOG=$STATE/deployer.log
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

# Read, never sourced, so the settings file cannot run code.
setting() {
  [[ -r $2 ]] || return 0
  sed -n "s/^$1=//p" "$2" | tail -n 1 | sed "s/^[\"']//; s/[\"']\$//"
}
FORGE_URL=${FORGE_URL:-$(setting FORGE_URL "$CONF/settings")}
REPO=${REPO:-$(setting REPO "$CONF/settings")}
WAYPOINT_URL=${WAYPOINT_URL:-$(setting WAYPOINT_URL "$CONF/settings")}
# Only an origin URL (scheme://host[/path]/owner/name) names the forge's root:
# an ssh one (git@host:owner/name) names no URL the API answers on.
if [[ -z $FORGE_URL || -z $REPO ]] && origin=$(git --git-dir="$MIRROR" remote get-url origin 2>/dev/null) &&
  [[ ${origin%.git} =~ ^[a-z][a-z0-9+.-]*://[^/]+(/[^/]+)*/[^/]+/[^/]+$ ]]; then
  origin=${origin%.git}
  FORGE_URL=${FORGE_URL:-${origin%/*/*}}
  REPO=${REPO:-${origin#"${origin%/*/*}/"}}
fi
WAYPOINT_URL=${WAYPOINT_URL:-$(setting WAYPOINT_BASE_URL "$ROOT/waypoint/deploy/.env")}

say() {
  local line
  line="[deployer] $(date -u +%FT%TZ) $*"
  printf '%s\n' "$line"
  printf '%s\n' "$line" >>"$LOG" 2>/dev/null || true
}
die() {
  say "error: $*" >&2
  exit 1
}

valid_tag() { [[ $1 =~ ^v[0-9]{1,6}\.[0-9]{1,6}\.[0-9]{1,6}$ ]]; }
valid_sha() { [[ $1 =~ ^[0-9a-f]{40}$ ]]; }

forge_token() {
  [[ -r $CONF/forge-token ]] || die "no $CONF/forge-token (run deploy/setup-cd.sh)"
  tr -d '[:space:]' <"$CONF/forge-token"
}

# The token travels in the environment, never argv; no token, no fetch.
mgit() {
  local token
  token=$(forge_token) || exit 1
  GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=http.extraHeader \
    GIT_CONFIG_VALUE_0="Authorization: token $token" GIT_TERMINAL_PROMPT=0 \
    git --git-dir="$MIRROR" "$@"
}

fetch() {
  [[ -n $FORGE_URL && -n $REPO ]] || die "no forge: set FORGE_URL and REPO in $CONF/settings (deploy/setup-cd.sh writes it)"
  if [[ ! -d $MIRROR ]]; then
    git init -q --bare "$MIRROR"
    git --git-dir="$MIRROR" remote add origin "$FORGE_URL/$REPO.git"
  fi
  mgit fetch -q --no-tags --prune origin '+refs/heads/main:refs/heads/main' '+refs/tags/v*:refs/tags/v*'
}

newest_tag() {
  local t
  while IFS= read -r t; do
    if valid_tag "$t"; then
      printf '%s\n' "$t"
      return 0
    fi
  done < <(git --git-dir="$MIRROR" tag --list 'v*' --sort=-v:refname)
}

release_ready() {
  local body
  body=$(printf 'header = "Authorization: token %s"\n' "$(forge_token)" |
    curl -fsS -K - --max-time 20 -H 'Accept: application/json' \
      "$FORGE_URL/api/v1/repos/$REPO/releases/tags/$1" 2>/dev/null) || return 1
  jq -e '(.draft | not) and ((.assets // []) | length > 0)' <<<"$body" >/dev/null
}

live_tag() { cat "$ROOT/waypoint/.deployed-version" 2>/dev/null || true; }

live_commit() {
  local v
  v=$(live_tag)
  [[ -n $v ]] || return 0
  git --git-dir="$MIRROR" rev-parse -q --verify "$v^{commit}" 2>/dev/null || true
}

deploy_lock_busy() {
  local lock=$ROOT/.waypoint-deploy.lock
  [[ -e $lock ]] || return 1
  ! flock -n "$lock" true
}

decide() {
  local live
  TAG='' SHA='' REASON='' CANDIDATE=''
  TAG=$(newest_tag)
  if [[ -z $TAG ]]; then
    REASON='no release tags'
    return 0
  fi
  SHA=$(git --git-dir="$MIRROR" rev-parse -q --verify "$TAG^{commit}" 2>/dev/null || true)
  valid_sha "$SHA" || die "$TAG resolved to '$SHA'"
  CANDIDATE=$TAG
  if [[ $(live_tag) == "$TAG" ]]; then
    REASON="$TAG is live"
  elif [[ -e $TRIED/$TAG ]]; then
    REASON="$TAG was tried already ($(cat "$TRIED/$TAG")); waiting for the next release"
  elif ! git --git-dir="$MIRROR" merge-base --is-ancestor "$SHA" refs/heads/main; then
    REASON="REFUSED: $TAG ($SHA) is not on main"
  elif live=$(live_commit) && [[ -n $(live_tag) && -z $live ]]; then
    # A live commit the mirror doesn't have can't be shown to be in the tag, so refuse until it is on main.
    REASON="REFUSED: the live $(live_tag) is not on main yet, so $TAG may not contain it; merge it to main (or deploy a release by hand)"
  elif [[ -n $live ]] && ! git --git-dir="$MIRROR" merge-base --is-ancestor "$live" "$SHA"; then
    REASON="REFUSED: $TAG ($SHA) does not descend from the live $(live_tag) ($live)"
  elif ! release_ready "$TAG"; then
    REASON="$TAG has no published release with binaries yet"
  elif [[ -e $STATE/paused ]]; then
    REASON="paused ($(cat "$STATE/paused")); would deploy $TAG"
  elif deploy_lock_busy; then
    REASON="another deploy is running; would deploy $TAG"
  else
    return 0
  fi
  TAG=''
}

record() {
  local tok
  [[ -x $RECORD && -r $CONF/waypoint-token && -n $WAYPOINT_URL ]] || return 0
  tok=$(tr -d '[:space:]' <"$CONF/waypoint-token")
  WAYPOINT_DEPLOY_TOKEN=$tok WAYPOINT_URL=$WAYPOINT_URL VERSION=$TAG COMMIT=$SHA BRANCH=main \
    RUN_URL="$FORGE_URL/$REPO/releases/tag/$TAG" "$RECORD" "$@" || say "recording '$1' in Waypoint failed (the deploy stands)"
}

# Installs by rename: a running bash script must not be overwritten in place.
self_update() {
  local pair path dest tmp
  for pair in deploy/pull-deploy.sh:waypoint-deployer deploy/remote-deploy.sh:waypoint-deploy deploy/record-deployment.sh:waypoint-record-deployment; do
    path=${pair%%:*}
    dest=$BIN/${pair##*:}
    tmp=$(mktemp "$BIN/.update.XXXXXX")
    if git --git-dir="$MIRROR" show "$SHA:$path" >"$tmp" 2>/dev/null && [[ -s $tmp ]]; then
      chmod 0755 "$tmp"
      if cmp -s "$tmp" "$dest"; then
        rm -f "$tmp"
      else
        mv -f "$tmp" "$dest"
        say "updated $dest from $TAG"
      fi
    else
      rm -f "$tmp"
    fi
  done
}

deploy() {
  local rc=0
  mkdir -p "$TRIED"
  printf '%s %s\n' "$SHA" "$(date -u +%FT%TZ)" >"$TRIED/$TAG"
  say "deploying $TAG ($SHA)"
  record started
  mgit archive --format=tar "$SHA" | env -u SSH_ORIGINAL_COMMAND "$DEPLOY" deploy "$TAG" || rc=$?
  case $rc in
  0)
    say "deployed $TAG"
    record succeeded "pull deploy"
    self_update
    ;;
  2)
    say "$TAG never became healthy; the previous version is running again (exit 2)"
    record failed "never became healthy; rolled back (pull deploy)"
    ;;
  3)
    say "$TAG failed and the previous version could not be restored (exit 3): Waypoint may be down"
    record failed "failed and rollback failed: look at docker compose -p waypoint logs (pull deploy)"
    ;;
  *)
    say "deploy of $TAG failed before changing anything (exit $rc)"
    record failed "refused or failed before changing anything (pull deploy)"
    ;;
  esac
  return "$rc"
}

status() {
  fetch
  decide
  say "newest release: $(newest_tag | grep . || echo none)${SHA:+ ($SHA)}"
  say "live:           $(live_tag | grep . || echo unknown)"
  [[ -e $STATE/paused ]] && say "paused:         $(cat "$STATE/paused")"
  if [[ -n $TAG ]]; then say "next run:       deploys $TAG"; else say "next run:       nothing ($REASON)"; fi
}

usage() {
  cat >&2 <<'EOF'
usage: waypoint-deployer           deploy the newest release that passes the checks
       waypoint-deployer status    what it sees and would do
       waypoint-deployer pause [WHY] | resume
EOF
  exit 1
}

main() {
  mkdir -p "$STATE"
  case "${1:-}" in
  pause)
    shift
    printf '%s by %s at %s\n' "${*:-no reason given}" "${USER:-?}" "$(date -u +%FT%TZ)" >"$STATE/paused"
    say "paused: no deploys until 'waypoint-deployer resume'"
    return 0
    ;;
  resume)
    [[ $# -eq 1 ]] || usage
    rm -f "$STATE/paused"
    say "resumed"
    return 0
    ;;
  status | "") ;;
  *) usage ;;
  esac
  exec 8>"$STATE/.lock"
  if [[ ${1:-} == status ]]; then
    [[ $# -eq 1 ]] || usage
    flock -w 60 8 || die "a deployer run is still going"
    status
    return 0
  fi
  [[ $# -eq 0 ]] || usage
  flock -n 8 || exit 0
  fetch
  decide
  if [[ -z $TAG ]]; then
    if [[ $REASON == REFUSED* && ! -e $STATE/refused-$CANDIDATE ]]; then
      say "$REASON"
      : >"$STATE/refused-$CANDIDATE"
    fi
    return 0
  fi
  deploy
}

main "$@"
