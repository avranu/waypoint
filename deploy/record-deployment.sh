#!/usr/bin/env bash
# Records a deployment of Waypoint in Waypoint itself (WP-D19/D21), from CI:
#   deploy/record-deployment.sh started|succeeded|failed [NOTES]
# Environment: WAYPOINT_DEPLOY_TOKEN (an API token with write on project WP)
# and WAYPOINT_URL (the Waypoint to record in; without either this does
# nothing), VERSION, COMMIT, BRANCH, RUN_URL, and optionally WAYPOINT_PROJECT
# (WP), WAYPOINT_ENV (production). The CI step calling it is continue-on-error:
# a missing record must never fail or undo a deploy.
set -euo pipefail

status=${1:?usage: record-deployment.sh started|succeeded|failed [NOTES]}
notes=${2:-}
case $status in started | succeeded | failed) ;; *)
  echo "record-deployment: bad status '$status'" >&2
  exit 2
  ;;
esac

if [[ -z ${WAYPOINT_DEPLOY_TOKEN:-} ]]; then
  echo "record-deployment: WAYPOINT_DEPLOY_TOKEN is not set; not recording '$status'."
  exit 0
fi

if [[ -z ${WAYPOINT_URL:-} ]]; then
  echo "record-deployment: WAYPOINT_URL is not set; not recording '$status'."
  exit 0
fi
base=$WAYPOINT_URL
project=${WAYPOINT_PROJECT:-WP}
env_name=${WAYPOINT_ENV:-production}

payload=$(jq -n \
  --arg commit "${COMMIT:-}" --arg branch "${BRANCH:-main}" --arg version "${VERSION:-}" \
  --arg status "$status" --arg url "${RUN_URL:-}" --arg notes "$notes" \
  '{commit: $commit, branch: $branch, version: $version, status: $status, url: $url, notes: $notes}')

# The token goes through a curl config on stdin, not argv.
code=$(printf 'header = "Authorization: Bearer %s"\n' "$WAYPOINT_DEPLOY_TOKEN" |
  curl -sS -K - -o /dev/null -w '%{http_code}' --max-time 20 \
    -X POST -H 'Content-Type: application/json' -d "$payload" \
    "$base/api/v1/projects/$project/environments/$env_name/deployments")
if [[ $code != 2* ]]; then
  echo "record-deployment: Waypoint answered $code" >&2
  exit 1
fi
echo "recorded deployment '$status' of ${VERSION:-?} in $project/$env_name"
