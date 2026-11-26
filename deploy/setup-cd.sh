#!/usr/bin/env bash
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
origin=$(git -C "$HERE" remote get-url origin 2>/dev/null || true)
origin=${origin%.git}
[[ $origin == https://*/*/* ]] || origin=''
FORGE_URL=${FORGE_URL:-${origin%/*/*}}
REPO=${REPO:-${origin#"${origin%/*/*}/"}}
DEPLOY_HOST_ALIAS=${DEPLOY_HOST_ALIAS:-$(sed -n 's/^DEPLOY_HOST *?\{0,1\}= *//p' "$HERE/site.mk" 2>/dev/null | tail -n 1)}
DEPLOY_USER=${DEPLOY_USER:-}
WAYPOINT_URL=${WAYPOINT_URL:-}
KEY=${HOME}/.ssh/waypoint_deploy
STEPS=8

DRY=0
case "${1:-}" in
--dry-run) DRY=1 ;;
"") ;;
-h | --help)
  sed -n '2,/^set -e/p' "$0" | sed '$d; s/^# \{0,1\}//'
  exit 0
  ;;
*)
  echo "usage: $0 [--dry-run]" >&2
  exit 2
  ;;
esac

if [[ -t 1 ]]; then
  B=$'\e[1m' D=$'\e[2m' G=$'\e[32m' Y=$'\e[33m' R=$'\e[31m' Z=$'\e[0m'
else
  B='' D='' G='' Y='' R='' Z=''
fi
n=0
step() {
  n=$((n + 1))
  printf '\n%s== Step %d of %d: %s%s\n' "$B" "$n" "$STEPS" "$1" "$Z"
}
say() { printf '%s\n' "$*"; }
note() { printf '%s%s%s\n' "$D" "$*" "$Z"; }
ok() { printf '%s  ok:%s %s\n' "$G" "$Z" "$*"; }
warn() { printf '%s  note:%s %s\n' "$Y" "$Z" "$*"; }
fail() {
  printf '%s  error:%s %s\n' "$R" "$Z" "$*" >&2
  exit 1
}
dry() { printf '%s  [dry-run]%s %s\n' "$Y" "$Z" "$*"; }

ask() {
  local def=${2:-n} hint ans
  hint='[y/N]'
  [[ $def == y ]] && hint='[Y/n]'
  if ((DRY)); then return 1; fi
  read -r -p "  $1 $hint " ans || ans=
  ans=${ans:-$def}
  [[ $ans == [yY]* ]]
}

for tool in ssh curl jq; do
  command -v "$tool" >/dev/null || fail "$tool is required on this machine"
done
for v in FORGE_URL REPO DEPLOY_HOST_ALIAS WAYPOINT_URL; do
  [[ -n ${!v} ]] || fail "set $v (see the settings at the top of $0)"
done
[[ -n $DEPLOY_USER ]] || DEPLOY_USER=$(ssh -n -G "$DEPLOY_HOST_ALIAS" 2>/dev/null | sed -n 's/^user //p')

TOKEN=${FORGEJO_ADMIN_TOKEN:-}
SIGNED=0
BODY=$(mktemp)
PAYLOAD=$(mktemp)
trap 'rm -f "$BODY" "$PAYLOAD"' EXIT
forge() {
  local method=$1 path=$2 json=${3:-}
  local -a args=(-sS -K - -o "$BODY" -w '%{http_code}' -X "$method" -H 'Accept: application/json')
  if [[ -n $json ]]; then
    printf '%s' "$json" >"$PAYLOAD"
    args+=(-H 'Content-Type: application/json' --data-binary @"$PAYLOAD")
  fi
  printf 'header = "Authorization: token %s"\n' "$TOKEN" | curl "${args[@]}" "$FORGE_URL/api/v1$path"
}
need_token() {
  ((DRY)) && return 0
  if [[ -z $TOKEN ]]; then
    say "  Paste a Forgejo access token for your admin account (scopes: write:repository)."
    say "  Create one at $FORGE_URL/user/settings/applications. It is used for this run only"
    say "  and is not stored; you can revoke it afterwards."
    read -rs -p "  Token: " TOKEN
    echo
  fi
  [[ -n $TOKEN ]] || fail "no token"
  ((SIGNED)) && return 0
  local code
  code=$(forge GET /user)
  [[ $code == 200 ]] || fail "the forge refused that token ($code)"
  SIGNED=1
  ok "signed in to $FORGE_URL as $(jq -r .login "$BODY")"
}
set_secret() {
  local name=$1 value=$2 json code
  json=$(jq -n --arg d "$value" '{data: $d}')
  code=$(forge PUT "/repos/$REPO/actions/secrets/$name" "$json")
  case $code in
  201 | 204) ok "secret $name is set" ;;
  *) fail "setting $name failed ($code): $(head -c 300 "$BODY")" ;;
  esac
}

say "${B}Waypoint continuous delivery: one-time setup${Z}"
say "Repo $REPO on $FORGE_URL; production host '$DEPLOY_HOST_ALIAS' (user $DEPLOY_USER)."
((DRY)) && warn "dry run: nothing is asked and nothing is changed."
note "What this sets up: a deployer on the production host that pulls each release from"
note "main (CI keeps no key to production), and (if you say yes) the branch rules that"
note "make every change go through a pull request."

step "A read-only forge token for the deployer"
say "  Production pulls its releases (WP-406): a deployer on $DEPLOY_HOST_ALIAS fetches main and the release"
say "  tags from the forge every two minutes, so CI holds no key to production."
say "  It needs a token that can only read: at $FORGE_URL/user/settings/applications make one named"
say "  'waypoint deployer' with ${B}only read:repository${Z} (better: from an account that can read $REPO and"
say "  nothing it shouldn't). It is stored on $DEPLOY_HOST_ALIAS in ~/.config/waypoint-deployer/forge-token (0600)."
if ((DRY)); then
  dry "GET $FORGE_URL/api/v1/repos/$REPO with the token (it must read the repo)"
  dry "ssh $DEPLOY_HOST_ALIAS: write ~/.config/waypoint-deployer/forge-token (mode 0600)"
elif ssh -n -o BatchMode=yes "$DEPLOY_HOST_ALIAS" 'test -s ~/.config/waypoint-deployer/forge-token' &&
  ! ask "$DEPLOY_HOST_ALIAS already has a deployer token. Replace it?" n; then
  ok "keeping the deployer token on $DEPLOY_HOST_ALIAS"
elif ask "Paste the deployer's token now?" y; then
  read -rs -p "  Read-only token: " rotok
  echo
  [[ -n $rotok ]] || fail "empty token"
  code=$(printf 'header = "Authorization: token %s"\n' "$rotok" |
    curl -sS -K - -o /dev/null -w '%{http_code}' "$FORGE_URL/api/v1/repos/$REPO")
  [[ $code == 200 ]] || fail "that token can't read $REPO ($code)"
  printf '%s\n' "$rotok" | ssh "$DEPLOY_HOST_ALIAS" 'set -eu; umask 077; mkdir -p ~/.config/waypoint-deployer
    cat > ~/.config/waypoint-deployer/forge-token.new; mv ~/.config/waypoint-deployer/forge-token.new ~/.config/waypoint-deployer/forge-token'
  unset rotok
  ok "stored on $DEPLOY_HOST_ALIAS"
else
  fail "the deployer can't read releases without it"
fi

step "Let the deployer record deployments in Waypoint (optional)"
say "  After each deploy the deployer tells Waypoint (project WP, environment production) what shipped."
say "  It needs an API token with write access to project WP:"
say "    1. Open $WAYPOINT_URL, Settings -> API tokens, make a token named 'deployer'."
say "    2. Paste it here. It is stored on $DEPLOY_HOST_ALIAS in ~/.config/waypoint-deployer/waypoint-token (0600)."
say "  Without it, deploys still happen; Waypoint then sees them on /healthz and records them as detected."
if ((DRY)); then
  dry "ssh $DEPLOY_HOST_ALIAS: write ~/.config/waypoint-deployer/waypoint-token (mode 0600)"
elif ask "Store a Waypoint token now?" n; then
  read -rs -p "  Waypoint API token: " wtok
  echo
  [[ -n $wtok ]] || fail "empty token"
  printf '%s\n' "$wtok" | ssh "$DEPLOY_HOST_ALIAS" 'set -eu; umask 077; mkdir -p ~/.config/waypoint-deployer
    cat > ~/.config/waypoint-deployer/waypoint-token.new; mv ~/.config/waypoint-deployer/waypoint-token.new ~/.config/waypoint-deployer/waypoint-token'
  unset wtok
  ok "stored on $DEPLOY_HOST_ALIAS"
else
  warn "skipped"
fi

step "Install the deployer on the production host"
say "  Copies deploy/pull-deploy.sh to ~/bin/waypoint-deployer, deploy/remote-deploy.sh to ~/bin/waypoint-deploy"
say "  and deploy/record-deployment.sh to ~/bin/waypoint-record-deployment on $DEPLOY_HOST_ALIAS, and adds the"
say "  systemd user units waypoint-deployer.service and .timer (not started yet: step 5)."
say "  The deployer deploys the newest release only if its commit is on main and descends from what runs now;"
say "  remote-deploy.sh builds, checks health and rolls back by itself, touching only the compose project 'waypoint'."
for f in pull-deploy.sh remote-deploy.sh record-deployment.sh systemd/waypoint-deployer.service systemd/waypoint-deployer.timer; do
  [[ -f $HERE/$f ]] || fail "$HERE/$f is missing"
done
put() {
  # shellcheck disable=SC2029 # the paths are ours and expand on the host on purpose
  ssh "$DEPLOY_HOST_ALIAS" "set -eu; mkdir -p \"\$(dirname $2)\"; cat > $2.new; chmod $3 $2.new; mv $2.new $2" <"$HERE/$1"
}
if ((DRY)); then
  dry "ssh $DEPLOY_HOST_ALIAS: write ~/bin/waypoint-deployer, ~/bin/waypoint-deploy, ~/bin/waypoint-record-deployment (0755)"
  dry "ssh $DEPLOY_HOST_ALIAS: write ~/.config/systemd/user/waypoint-deployer.{service,timer}; systemctl --user daemon-reload"
  dry "ssh $DEPLOY_HOST_ALIAS: write ~/.config/waypoint-deployer/settings (FORGE_URL=$FORGE_URL REPO=$REPO WAYPOINT_URL=$WAYPOINT_URL)"
  dry "ssh $DEPLOY_HOST_ALIAS: ~/bin/waypoint-deployer status"
elif ask "Install on $DEPLOY_HOST_ALIAS?" y; then
  ssh -n -o BatchMode=yes "$DEPLOY_HOST_ALIAS" true || fail "cannot ssh to $DEPLOY_HOST_ALIAS without a prompt"
  put pull-deploy.sh bin/waypoint-deployer 0755
  put remote-deploy.sh bin/waypoint-deploy 0755
  put record-deployment.sh bin/waypoint-record-deployment 0755
  put systemd/waypoint-deployer.service .config/systemd/user/waypoint-deployer.service 0644
  put systemd/waypoint-deployer.timer .config/systemd/user/waypoint-deployer.timer 0644
  printf 'FORGE_URL=%s\nREPO=%s\nWAYPOINT_URL=%s\n' "$FORGE_URL" "$REPO" "$WAYPOINT_URL" |
    ssh "$DEPLOY_HOST_ALIAS" 'set -eu; mkdir -p ~/.config/waypoint-deployer; cat > ~/.config/waypoint-deployer/settings'
  ssh -n "$DEPLOY_HOST_ALIAS" 'systemctl --user daemon-reload'
  [[ $(ssh -n "$DEPLOY_HOST_ALIAS" 'loginctl show-user "$USER" -p Linger --value' 2>/dev/null) == yes ]] ||
    warn "lingering is off for that user: the timer only runs while they are logged in (sudo loginctl enable-linger $DEPLOY_USER)"
  ok "installed"
  say "  What the deployer sees:"
  ssh -n "$DEPLOY_HOST_ALIAS" 'bin/waypoint-deployer status' | sed 's/^/    /' || fail "the deployer could not read the forge (see above)"
else
  fail "the next steps need the deployer"
fi

step "Retire CI's deploy key"
say "  CI used to deploy over ssh with DEPLOY_SSH_KEY, which any job on the runner pool could read (WP-406)."
say "  This deletes the repo secrets DEPLOY_SSH_KEY, DEPLOY_KNOWN_HOSTS and WAYPOINT_DEPLOY_TOKEN (CI's deploy job"
say "  then skips itself), removes the key's line (command=.../waypoint-deploy) from ~/.ssh/authorized_keys on"
say "  $DEPLOY_HOST_ALIAS (copied to authorized_keys.bak-waypoint first), and deletes $KEY here."
if ((DRY)); then
  for name in DEPLOY_SSH_KEY DEPLOY_KNOWN_HOSTS WAYPOINT_DEPLOY_TOKEN; do
    dry "DELETE $FORGE_URL/api/v1/repos/$REPO/actions/secrets/$name"
  done
  dry "ssh $DEPLOY_HOST_ALIAS: drop authorized_keys lines with command=\"…/bin/waypoint-deploy\""
  dry "rm -f $KEY $KEY.pub"
elif ask "Retire it now?" y; then
  need_token
  for name in DEPLOY_SSH_KEY DEPLOY_KNOWN_HOSTS WAYPOINT_DEPLOY_TOKEN; do
    code=$(forge DELETE "/repos/$REPO/actions/secrets/$name")
    case $code in
    204 | 200) ok "deleted secret $name" ;;
    404) ok "no secret $name" ;;
    *) fail "deleting $name failed ($code): $(head -c 300 "$BODY")" ;;
    esac
  done
  # shellcheck disable=SC2016 # runs on the host
  ssh -n "$DEPLOY_HOST_ALIAS" '
    set -eu
    f=~/.ssh/authorized_keys
    [ -f "$f" ] || { echo "  no authorized_keys"; exit 0; }
    if ! grep -qE "^command=\"[^\"]*/bin/waypoint-deploy\"" "$f"; then echo "  no deploy key in authorized_keys"; exit 0; fi
    cp -p "$f" "$f.bak-waypoint"
    tmp=$(mktemp "$f.XXXXXX")
    grep -vE "^command=\"[^\"]*/bin/waypoint-deploy\"" "$f" >"$tmp" || true
    cat "$tmp" >"$f"
    rm -f "$tmp"
    echo "  removed the deploy key from authorized_keys"
  '
  rm -f "$KEY" "$KEY.pub"
  ok "CI holds no key to production"
else
  warn "skipped: CI can still deploy, and a pool job can still read its key. Don't start the timer while it can (step 5)."
fi

step "Start the deployer"
say "  Enables waypoint-deployer.timer on $DEPLOY_HOST_ALIAS: every two minutes it deploys a new release if there is one."
say "  Pause it in an incident with: ssh $DEPLOY_HOST_ALIAS bin/waypoint-deployer pause WHY (and later: ... resume)."
if ((DRY)); then
  dry "ssh $DEPLOY_HOST_ALIAS: systemctl --user enable --now waypoint-deployer.timer"
elif ask "Start it?" y; then
  ssh -n "$DEPLOY_HOST_ALIAS" 'systemctl --user enable --now waypoint-deployer.timer && systemctl --user list-timers waypoint-deployer.timer --no-pager' | sed 's/^/    /'
  ok "started"
else
  warn "skipped; nothing deploys until it runs"
fi

step "A release token, only if CI's own token may not publish releases (optional)"
say "  CI first tries its automatic token to tag and publish releases. If the first run says it was"
say "  refused (403), create a token for a user with write access (scope write:repository) and"
say "  store it as RELEASE_TOKEN. Skip this now; come back if you see that error."
if ((DRY)); then
  dry "PUT $FORGE_URL/api/v1/repos/$REPO/actions/secrets/RELEASE_TOKEN   {\"data\": \"<token, redacted>\"}"
elif ask "Set RELEASE_TOKEN now?" n; then
  need_token
  read -rs -p "  Release token: " rtok
  echo
  [[ -n $rtok ]] || fail "empty token"
  set_secret RELEASE_TOKEN "$rtok"
  unset rtok
else
  warn "skipped"
fi

step "Protect main (optional)"
say "  Changes reach main only by pull request: no direct pushes (admins included), the CI checks"
say "  must pass, no approvals required (the review gate stands in), rejected reviews block."
say "  ${B}Be sure first:${Z} requiring 'waypoint/review' only works once Waypoint's review gate posts that"
say "  status on this repo (the 'Review gate' switch); otherwise no PR can ever merge. You'll be asked."
contexts=('ci / backend (pull_request)' 'ci / lint (pull_request)' 'ci / e2e (pull_request)')
if ((DRY)); then
  contexts+=('waypoint/review')
elif ask "Require the 'waypoint/review' status too (only if the review gate is on)?" n; then
  contexts+=('waypoint/review')
fi
protect=$(printf '%s\n' "${contexts[@]}" | jq -R . | jq -s \
  '{rule_name: "main", enable_push: false, enable_push_whitelist: false, enable_status_check: true,
    status_check_contexts: ., required_approvals: 0, block_on_rejected_reviews: true, apply_to_admins: true}')
say "  The rule:"
printf '%s\n' "$protect" | sed 's/^/    /'
if ((DRY)); then
  dry "GET $FORGE_URL/api/v1/repos/$REPO/branch_protections/main  -> PATCH it if it exists, else POST /branch_protections with the JSON above"
elif ask "Apply this rule to main?" n; then
  need_token
  code=$(forge GET "/repos/$REPO/branch_protections/main")
  if [[ $code == 200 ]]; then
    code=$(forge PATCH "/repos/$REPO/branch_protections/main" "$(jq 'del(.rule_name)' <<<"$protect")")
    [[ $code == 200 ]] || fail "updating the rule failed ($code): $(head -c 300 "$BODY")"
    ok "updated the existing rule for main"
  else
    code=$(forge POST "/repos/$REPO/branch_protections" "$protect")
    [[ $code == 201 ]] || fail "creating the rule failed ($code): $(head -c 300 "$BODY")"
    ok "created the rule for main"
  fi
else
  warn "skipped; main stays open to direct pushes"
fi

step "Delete merged branches by default (optional)"
say "  Turns on 'delete branch after merge' as the repo default, so merged PR branches disappear."
edit='{"default_delete_branch_after_merge": true}'
say "  PATCH /repos/$REPO  $edit"
if ((DRY)); then
  dry "PATCH $FORGE_URL/api/v1/repos/$REPO  $edit"
elif ask "Turn it on?" y; then
  need_token
  code=$(forge PATCH "/repos/$REPO" "$edit")
  [[ $code == 200 ]] || fail "changing the repo setting failed ($code): $(head -c 300 "$BODY")"
  ok "merged branches will be deleted by default"
else
  warn "skipped"
fi

echo
if ((DRY)); then
  say "${B}Dry run finished.${Z} Nothing was changed. Run without --dry-run to do it."
  exit 0
fi
say "${B}Done.${Z} The next push to main that passes CI is released, and the deployer deploys it."
note "Rollback: make rollback-remote. Status: make deploy-status. See docs/DEPLOY.md, Continuous delivery."
note "Revoke the Forgejo admin token you used if it was only for this."
