#!/usr/bin/env bash
# Deploys Waypoint on its production host. One code path for CD
# (WP-D21) and `make deploy-remote`.
#
#   remote-deploy.sh deploy VERSION     a `git archive` tarball on stdin becomes
#                                       the live tree; rolls back by itself if
#                                       the new version never gets healthy
#   remote-deploy.sh deploy VERSION --staged
#                                       the tree is already in ~/waypoint.new
#                                       (make deploy-remote); argv only
#   remote-deploy.sh rollback           swap back to ~/waypoint.prev
#   remote-deploy.sh status             running version, previous version, containers
#   remote-deploy.sh restart SERVICE    restart one helper service (meaning, opencode)
#
# The deploy key in ~/.ssh/authorized_keys is pinned to this file:
#   command="$HOME/bin/waypoint-deploy",no-pty,no-agent-forwarding,...
# so under SSH the request arrives in SSH_ORIGINAL_COMMAND and is validated
# strictly; nothing from it reaches a shell. Layout under $HOME:
#   waypoint/        live tree (deploy/.env stays here across deploys)
#   waypoint.prev/   the tree before it (one kept)
#   waypoint.failed/ the last tree that never became healthy (one kept)
# Only the compose project `waypoint` is ever touched: the host may run
# other compose projects too.
#
# Exit status: 0 done, 1 refused or failed before changing anything,
# 2 the new version failed and the previous one is running again,
# 3 failed and the previous one could not be restored.
set -euo pipefail

ROOT=${WAYPOINT_DEPLOY_ROOT:-$HOME}
DOCKER=${WAYPOINT_DEPLOY_DOCKER:-docker}
HEALTH_WAIT=${WAYPOINT_DEPLOY_HEALTH_WAIT:-180}
MAX_TARBALL=$((512 * 1024 * 1024))
PROJECT=waypoint
LIVE=$ROOT/waypoint
NEW=$ROOT/waypoint.new
PREV=$ROOT/waypoint.prev
FAILED=$ROOT/waypoint.failed

say() { printf '[deploy] %s\n' "$*"; }
die() {
  printf '[deploy] error: %s\n' "$*" >&2
  exit 1
}

# A tag (v1.2.3, v1.2.3-4-gabc1234) or a commit sha: nothing else gets near
# a compose build arg or a path.
valid_version() {
  [[ $1 =~ ^(v[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.]+)*|[0-9a-f]{7,40})$ ]] && ((${#1} <= 64))
}

compose() {
  local dir=$1
  shift
  # fd 9 is the deploy lock, held by this script; docker gets no copy of it.
  (cd "$dir" && "$DOCKER" compose -p "$PROJECT" -f deploy/compose.yaml "$@" 9>&-)
}

# env_get DIR KEY DEFAULT: a value from DIR/deploy/.env, read without sourcing.
env_get() {
  local v
  v=$(grep -E "^$2=" "$1/deploy/.env" 2>/dev/null | tail -n 1 | cut -d= -f2- || true)
  v=${v%\"}
  v=${v#\"}
  printf '%s' "${v:-$3}"
}

health_url() {
  local bind port
  bind=$(env_get "$LIVE" WAYPOINT_BIND 127.0.0.1)
  [[ $bind == 0.0.0.0 ]] && bind=127.0.0.1
  port=$(env_get "$LIVE" WAYPOINT_PORT 8080)
  [[ $port =~ ^[0-9]+$ ]] || die "WAYPOINT_PORT in deploy/.env is not a number"
  printf 'http://%s:%s/healthz' "$bind" "$port"
}

# running_version prints the version /healthz reports, or nothing.
running_version() {
  curl -fsS --max-time 3 "$(health_url)" 2>/dev/null |
    sed -n 's/.*"version" *: *"\([^"]*\)".*/\1/p' || true
}

tree_version() { cat "$1/.deployed-version" 2>/dev/null || true; }

# wait_healthy WANT: up to HEALTH_WAIT seconds for /healthz to report WANT
# (any version when WANT is empty), twice in a row so a crash loop that
# answers once doesn't pass.
wait_healthy() {
  local want=$1 deadline got streak=0
  deadline=$((SECONDS + HEALTH_WAIT))
  while ((SECONDS < deadline)); do
    got=$(running_version)
    if [[ -n $got && ( -z $want || $got == "$want" ) ]]; then
      streak=$((streak + 1))
      if ((streak >= 2)); then
        say "healthy: version ${got}"
        return 0
      fi
      sleep 3
    else
      streak=0
      sleep 3
    fi
  done
  say "not healthy after ${HEALTH_WAIT}s (last seen: ${got:-no answer})"
  return 1
}

# Migrations only move forward: warn when going back past one.
warn_schema() {
  local newer=$1 older=$2 extra
  extra=$(comm -23 <(find "$newer/internal/db/migrations" -name "*.sql" -printf "%f\n" 2>/dev/null | sort) \
    <(find "$older/internal/db/migrations" -name "*.sql" -printf "%f\n" 2>/dev/null | sort) || true)
  if [[ -n $extra ]]; then
    say "warning: the version being left has migrations the other one lacks ($(echo "$extra" | tr '\n' ' '))."
    say "         If it misbehaves, restore a backup from the data volume's backups/ (docs/DEPLOY.md)."
  fi
}

pre_deploy_backup() {
  local out="/data/backups/pre-deploy-$1.db"
  if compose "$LIVE" ps --status running --services 2>/dev/null | grep -qx waypoint; then
    if compose "$LIVE" exec -T waypoint /waypoint backup "$out" >/dev/null 2>&1; then
      say "database copied to $out in the data volume"
    else
      say "no pre-deploy database copy (already exists, or backup failed); continuing"
    fi
  fi
}

show_logs() { compose "$1" logs --no-color --tail 40 waypoint 2>&1 || true; }

# up DIR VERSION: build and start the compose project from DIR.
up() {
  WAYPOINT_VERSION=$2 compose "$1" up -d --build
}

stage_from_stdin() {
  rm -rf "$NEW"
  mkdir -p "$NEW"
  say "unpacking the tarball from stdin"
  head -c "$MAX_TARBALL" | tar -x -C "$NEW" --no-same-owner
}

do_deploy() {
  local version=$1 staged=$2 had_live=0
  valid_version "$version" || die "bad version '$version' (a v1.2.3-style tag or a commit sha)"
  mkdir -p "$ROOT"
  if [[ $staged == 0 ]]; then
    stage_from_stdin
  fi
  [[ -f $NEW/deploy/compose.yaml && -f $NEW/deploy/Dockerfile ]] || die "no deploy/compose.yaml and deploy/Dockerfile in the tree"
  [[ -d $LIVE ]] && had_live=1
  if [[ -f $LIVE/deploy/.env ]]; then
    cp -p "$LIVE/deploy/.env" "$NEW/deploy/.env"
  fi
  [[ -f $NEW/deploy/.env ]] || die "no deploy/.env on this host (see docs/DEPLOY.md)"
  printf '%s\n' "$version" >"$NEW/.deployed-version"

  ((had_live)) && pre_deploy_backup "$version"
  rm -rf "$PREV"
  if ((had_live)); then mv "$LIVE" "$PREV"; fi
  mv "$NEW" "$LIVE"
  say "deploying $version (previous tree kept as waypoint.prev)"

  if up "$LIVE" "$version" && wait_healthy "$version"; then
    say "deployed $version"
    do_status || true
    return 0
  fi

  say "deploy of $version failed"
  show_logs "$LIVE"
  if ((!had_live)); then
    say "there was no previous version to go back to"
    return 3
  fi
  rm -rf "$FAILED"
  mv "$LIVE" "$FAILED"
  mv "$PREV" "$LIVE"
  local old
  old=$(tree_version "$LIVE")
  say "rolling back to ${old:-the previous tree}"
  if up "$LIVE" "${old:-dev}" && wait_healthy "$old"; then
    say "rolled back to ${old:-the previous tree}; the failed tree is in waypoint.failed"
    return 2
  fi
  say "ROLLBACK FAILED: Waypoint may be down. Look at: docker compose -p waypoint logs"
  return 3
}

do_rollback() {
  [[ -d $PREV/deploy ]] || die "nothing to roll back to (no ~/waypoint.prev)"
  [[ -d $LIVE ]] || die "no live tree"
  local old swap=$ROOT/waypoint.swap
  old=$(tree_version "$PREV")
  say "rolling back from $(tree_version "$LIVE") to ${old:-the previous tree}"
  warn_schema "$LIVE" "$PREV"
  rm -rf "$swap"
  mv "$LIVE" "$swap"
  mv "$PREV" "$LIVE"
  mv "$swap" "$PREV"
  if up "$LIVE" "${old:-dev}" && wait_healthy "$old"; then
    say "rolled back"
    do_status || true
    return 0
  fi
  show_logs "$LIVE"
  say "rollback did not become healthy"
  return 3
}

do_status() {
  local got
  [[ -d $LIVE ]] || die "no live tree at $LIVE"
  got=$(running_version)
  say "running version: ${got:-not answering}"
  say "live tree:       $(tree_version "$LIVE" | grep . || echo unknown)"
  if [[ -d $PREV ]]; then say "previous tree:   $(tree_version "$PREV" | grep . || echo unknown)"; else say "previous tree:   none"; fi
  compose "$LIVE" ps --format 'table {{.Service}}\t{{.State}}\t{{.Status}}' 2>&1 | sed 's/^/[deploy] /' || true
  [[ -n $got ]]
}

# The services restart may touch: the helpers beside Waypoint, which keep
# no state a restart loses. Waypoint itself restarts by a deploy.
RESTARTABLE=(meaning opencode)

do_restart() {
  local svc=$1 ok=0 s
  for s in "${RESTARTABLE[@]}"; do [[ $svc == "$s" ]] && ok=1; done
  ((ok)) || die "refused: restart takes one of: ${RESTARTABLE[*]}"
  [[ -d $LIVE ]] || die "no live tree at $LIVE"
  say "restarting $svc"
  compose "$LIVE" restart "$svc"
  compose "$LIVE" ps --format 'table {{.Service}}\t{{.State}}\t{{.Status}}' "$svc" 2>&1 | sed 's/^/[deploy] /' || true
}

usage() {
  cat >&2 <<'EOF'
usage: waypoint-deploy deploy VERSION   (tarball on stdin)
       waypoint-deploy rollback
       waypoint-deploy status
       waypoint-deploy restart SERVICE   (meaning or opencode)
EOF
  exit 1
}

main() {
  local -a words
  local staged=0
  if [[ -n ${SSH_ORIGINAL_COMMAND:-} ]]; then
    # Forced command: strictly words from a safe alphabet, no flags.
    [[ $SSH_ORIGINAL_COMMAND =~ ^[A-Za-z0-9._\ -]{1,100}$ ]] || die "refused: unexpected characters in the command"
    read -r -a words <<<"$SSH_ORIGINAL_COMMAND"
  else
    words=("$@")
    if [[ ${#words[@]} -eq 3 && ${words[2]} == --staged ]]; then
      staged=1
      words=("${words[@]:0:2}")
    fi
  fi
  # One deploy at a time (a second would swap directories under the first).
  mkdir -p "$ROOT"
  exec 9>"$ROOT/.waypoint-deploy.lock"
  flock -n 9 || die "another deploy is running"
  case "${words[0]:-}" in
  deploy)
    [[ ${#words[@]} -eq 2 ]] || usage
    do_deploy "${words[1]}" "$staged"
    ;;
  rollback)
    [[ ${#words[@]} -eq 1 ]] || usage
    do_rollback
    ;;
  status)
    [[ ${#words[@]} -eq 1 ]] || usage
    do_status
    ;;
  restart)
    [[ ${#words[@]} -eq 2 ]] || usage
    do_restart "${words[1]}"
    ;;
  *) usage ;;
  esac
}

main "$@"
