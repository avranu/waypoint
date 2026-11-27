#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."

REPO=git.jmann.me/claude/waypoint-ci
WORKFLOW=.forgejo/workflows/ci.yml

go_version() { sed -n 's/^go \([0-9][0-9.]*\)$/\1/p' go.mod | head -n 1; }
playwright_version() { jq -r '.packages["node_modules/playwright"].version' e2e/package-lock.json; }
tool_pins() { grep -E '^[A-Z_]+_VERSION \?=' Makefile; }

tag() {
  {
    cat .forgejo/ci-image/Dockerfile
    echo "go $(go_version)"
    echo "playwright $(playwright_version)"
    tool_pins
  } | sha256sum | cut -c1-12
}

if [ "${1:-}" = --tag ]; then
  tag
  exit 0
fi

GO_VERSION=$(go_version)
PLAYWRIGHT_VERSION=$(playwright_version)
TAG=$(tag)
[ -n "$GO_VERSION" ] && [ -n "$PLAYWRIGHT_VERSION" ] && [ "$PLAYWRIGHT_VERSION" != null ] || {
  echo "could not read the Go version from go.mod or Playwright's from e2e/package-lock.json" >&2
  exit 2
}
REF=$REPO:$TAG

remote_digest() { docker buildx imagetools inspect "$REF" --format '{{json .Manifest.Digest}}' 2>/dev/null | tr -d '"' || true; }

LOCAL=false
[ "${1:-}" = --local ] && LOCAL=true
digest=
$LOCAL || digest=$(remote_digest)
if [ -n "$digest" ]; then
  echo "the registry already has $REF ($digest)"
else
  GO_SHA256=$(curl -fsS 'https://go.dev/dl/?mode=json&include=all' |
    jq -r --arg f "go${GO_VERSION}.linux-amd64.tar.gz" '[.[].files[] | select(.filename == $f) | .sha256] | first // empty')
  [ -n "$GO_SHA256" ] || { echo "go.dev lists no checksum for go${GO_VERSION}.linux-amd64.tar.gz" >&2; exit 2; }
  ctx=$(mktemp -d)
  trap 'rm -rf "$ctx"' EXIT
  cp .forgejo/ci-image/Dockerfile Makefile "$ctx/"
  docker build --pull -t "$REF" \
    --build-arg "GO_VERSION=$GO_VERSION" --build-arg "GO_SHA256=$GO_SHA256" \
    --build-arg "PLAYWRIGHT_VERSION=$PLAYWRIGHT_VERSION" --build-arg "IMAGE_TAG=$TAG" \
    "$ctx"
  if $LOCAL; then
    echo "built $REF locally; not pushed"
    exit 0
  fi
  docker push "$REF"
  digest=$(remote_digest)
  [ -n "$digest" ] || { echo "pushed $REF but the registry reports no digest for it" >&2; exit 1; }
fi

sed -i -E "s#(image: )${REPO}:[0-9a-f]+(@sha256:[0-9a-f]+)?#\1${REF}@${digest}#" "$WORKFLOW" "${WORKFLOW%/*}/advisories.yml"
if git diff --quiet -- "$WORKFLOW" "${WORKFLOW%/*}/advisories.yml"; then
  echo "$WORKFLOW already uses $REF@$digest"
else
  echo "$WORKFLOW now uses $REF@$digest: commit it"
fi
