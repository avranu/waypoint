#!/usr/bin/env bash
set -euo pipefail

mode=dry
only=
while (($#)); do
  case $1 in
  --dry-run) mode=dry ;;
  --publish) mode=publish ;;
  --version-only) only=version ;;
  --notes-only) only=notes ;;
  -h | --help)
    sed -n '2,/^set -e/p' "$0" | sed '$d; s/^# \{0,1\}//'
    exit 0
    ;;
  *)
    echo "release.sh: unknown argument $1" >&2
    exit 2
    ;;
  esac
  shift
done

die() {
  echo "release.sh: $*" >&2
  exit 1
}

origin=$(git remote get-url origin 2>/dev/null || true)
origin=${origin%.git}
[[ $origin == https://*/*/* ]] || origin=''
SERVER=${GITHUB_SERVER_URL:-${origin%/*/*}}
REPO=${GITHUB_REPOSITORY:-${origin#"${origin%/*/*}/"}}
[[ $mode != publish || (-n $SERVER && -n $REPO) ]] || die "GITHUB_SERVER_URL and GITHUB_REPOSITORY are not set"
TOKEN=${RELEASE_TOKEN:-${GITHUB_TOKEN:-}}
HEAD_SHA=$(git rev-parse HEAD)

if [[ $(git rev-parse --is-shallow-repository) == true ]]; then
  die "shallow clone: fetch the full history and tags (checkout with fetch-depth: 0)"
fi

[[ $mode != publish ]] || git fetch --quiet --tags --force origin 2>/dev/null || true

current=$(git tag --points-at HEAD --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | head -n 1)
describe=(git describe --tags --abbrev=0 --match 'v[0-9]*.[0-9]*.[0-9]*')
[[ -n $current ]] && describe+=(--exclude "$current")
last=$("${describe[@]}" HEAD 2>/dev/null || true)
if [[ -n $last ]]; then
  [[ $last =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+)$ ]] || die "last tag '$last' is not vMAJOR.MINOR.PATCH"
  major=${BASH_REMATCH[1]} minor=${BASH_REMATCH[2]} patch=${BASH_REMATCH[3]}
  range="$last..HEAD"
else
  range=HEAD
fi

declare -a subjects=() bodies=()
while IFS= read -r -d $'\x1e' rec; do
  rec=${rec#$'\n'}
  subjects+=("${rec%%$'\x1f'*}")
  bodies+=("${rec#*$'\x1f'}")
done < <(git log --format='%s%x1f%b%x1e' "$range")
ncommits=${#subjects[@]}

breaking=0 feat=0 migration=0
reason=
for i in "${!subjects[@]}"; do
  s=${subjects[$i]}
  if [[ $s =~ ^[a-zA-Z]+(\([^\)]*\))?!: ]] || grep -qE '^BREAKING[ -]CHANGE:' <<<"${bodies[$i]}"; then
    breaking=1
    reason=${reason:-"breaking change: $s"}
  fi
  if [[ $s =~ ^(feat|feature)(\([^\)]*\))?!?: ]]; then
    feat=1
    reason=${reason:-"feature: $s"}
  fi
done
if [[ -n $last ]]; then
  added=$(git diff --diff-filter=A --name-only "$last" HEAD -- internal/db/migrations | head -n 1)
else
  added=$(git ls-tree --name-only -r HEAD internal/db/migrations | head -n 1)
fi
if [[ -n $added ]]; then
  migration=1
  reason=${reason:-"new migration: $(basename "$added")"}
fi

if [[ -n $current ]]; then
  version=$current
  bump="already tagged"
elif [[ -z $last ]]; then
  version=v0.1.0
  bump="first release"
else
  if ((breaking)); then
    if ((major == 0)); then
      minor=$((minor + 1)) patch=0 bump="minor (breaking while 0.x)"
    else
      major=$((major + 1)) minor=0 patch=0 bump="major (breaking)"
    fi
  elif ((feat || migration)); then
    minor=$((minor + 1)) patch=0 bump="minor"
  else
    patch=$((patch + 1)) bump="patch"
  fi
  version=v$major.$minor.$patch
  [[ $bump == patch ]] || bump="$bump: ${reason:-}"
fi

declare -a feats=() fixes=() others=() breaks=() flat=()
conventional=0
while IFS= read -r -d $'\x1e' rec; do
  rec=${rec#$'\n'}
  sha=${rec%%$'\x1f'*}
  rest=${rec#*$'\x1f'}
  s=${rest%%$'\x1f'*}
  b=${rest#*$'\x1f'}
  if [[ $s =~ ^Merge\ pull\ request\ \'(.*)\'\ \((#[0-9]+)\)\ from ]]; then
    s="${BASH_REMATCH[1]} (${BASH_REMATCH[2]})"
  fi
  type='' scope='' desc=$s isbreaking=0
  if [[ $s =~ ^([a-zA-Z]+)(\(([^\)]*)\))?(!)?:\ (.*)$ ]]; then
    type=${BASH_REMATCH[1],,}
    scope=${BASH_REMATCH[3]}
    [[ -n ${BASH_REMATCH[4]} ]] && isbreaking=1
    desc=${BASH_REMATCH[5]}
    conventional=1
  fi
  grep -qE '^BREAKING[ -]CHANGE:' <<<"$b" && isbreaking=1
  if [[ -z $type ]] && git rev-parse -q --verify "$sha^2" >/dev/null; then
    inner=$(git log --format=%s "$sha^1..$sha^2")
    if grep -qiE '^(feat|feature)(\([^)]*\))?!?:' <<<"$inner"; then
      type=feat conventional=1
    elif grep -qiE '^fix(\([^)]*\))?!?:' <<<"$inner"; then
      type=fix conventional=1
    fi
  fi
  line="- "
  [[ -n $scope ]] && line+="**$scope:** "
  line+="$desc"
  [[ $s =~ \(#[0-9]+\)$ ]] || line+=" (\`${sha:0:7}\`)"
  flat+=("$line")
  ((isbreaking)) && breaks+=("$line")
  case $type in
  feat | feature) feats+=("$line") ;;
  fix) fixes+=("$line") ;;
  *) others+=("$line") ;;
  esac
done < <(git log --first-parent --format='%H%x1f%s%x1f%b%x1e' "$range")

notes() {
  section() {
    local title=$1
    shift
    (($#)) || return 0
    printf '### %s\n\n' "$title"
    printf '%s\n' "$@"
    printf '\n'
  }
  if ((${#breaks[@]})); then section "Breaking changes" "${breaks[@]}"; fi
  if ((conventional)); then
    if ((${#feats[@]})); then section "Features" "${feats[@]}"; fi
    if ((${#fixes[@]})); then section "Fixes" "${fixes[@]}"; fi
    if ((${#others[@]})); then section "Other" "${others[@]}"; fi
  elif ((${#flat[@]})); then
    section "Changes" "${flat[@]}"
  fi
  if [[ -n $last ]]; then
    printf 'Full changes: %s/%s/compare/%s...%s\n' "$SERVER" "$REPO" "$last" "$version"
  fi
}
notes_text=$(notes)

if [[ $only == version ]]; then
  echo "$version"
elif [[ $only == notes ]]; then
  printf '%s\n' "$notes_text"
elif [[ $mode == dry ]]; then
  echo "version:  $version"
  echo "previous: ${last:-none}"
  echo "bump:     $bump"
  echo "commits:  $ncommits"
  echo
  printf '%s\n' "$notes_text"
fi
[[ $mode == dry ]] && exit 0

[[ -n $TOKEN ]] || die "no RELEASE_TOKEN or GITHUB_TOKEN in the environment"
[[ ${GITHUB_SHA:-$HEAD_SHA} == "$HEAD_SHA" ]] || die "HEAD ($HEAD_SHA) is not the commit being built (${GITHUB_SHA})"
if ((ncommits == 0)) && [[ -z $current ]]; then
  echo "nothing to release since $last"
  exit 0
fi

out() { [[ -z ${GITHUB_OUTPUT:-} ]] || echo "$1" >>"$GITHUB_OUTPUT"; }

api() {
  local method=$1 path=$2
  shift 2
  printf 'header = "Authorization: token %s"\n' "$TOKEN" |
    curl -sS -K - -X "$method" -H 'Accept: application/json' "$@" "$SERVER/api/v1/repos/$REPO$path"
}
body=$(mktemp)
trap 'rm -f "$body"' EXIT
api_code() {
  api "$@" -o "$body" -w '%{http_code}'
}

if [[ -z $current ]]; then
  if git rev-parse -q --verify "refs/tags/$version" >/dev/null; then
    other=$(git rev-parse "$version^{commit}")
    if [[ $other != "$HEAD_SHA" ]]; then
      echo "$version already exists at ${other:0:7}; a newer run released it. Skipping."
      out "released=false"
      exit 0
    fi
  fi
fi

code=$(api_code GET "/releases/tags/$version")
if [[ $code == 200 ]]; then
  rel_id=$(jq -r .id "$body")
  echo "release $version exists (id $rel_id)"
elif [[ $code == 404 ]]; then
  payload=$(jq -n --arg tag "$version" --arg sha "$HEAD_SHA" --arg body "$notes_text" \
    '{tag_name: $tag, target_commitish: $sha, name: $tag, body: $body, draft: false, prerelease: false}')
  code=$(api_code POST "/releases" -H 'Content-Type: application/json' -d "$payload")
  case $code in
  201) rel_id=$(jq -r .id "$body") ;;
  401 | 403)
    die "creating the release was refused ($code): the job token may not write releases. Give the repo a RELEASE_TOKEN secret (a token with write:repository)."
    ;;
  *) die "creating the release failed ($code): $(head -c 300 "$body")" ;;
  esac
  echo "created release $version (id $rel_id)"
else
  die "looking up release $version failed ($code): $(head -c 300 "$body")"
fi

make --no-print-directory cli VERSION="$version" >/dev/null
have=$(api GET "/releases/$rel_id/assets" | jq -r '.[].name' 2>/dev/null || true)
for f in bin/cli/*; do
  name=$(basename "$f")
  if grep -qxF "$name" <<<"$have"; then continue; fi
  code=$(api_code POST "/releases/$rel_id/assets?name=$name" -F "attachment=@$f")
  [[ $code == 201 ]] || die "uploading $name failed ($code): $(head -c 300 "$body")"
  echo "attached $name"
done

echo "released $version"
out "version=$version"
out "released=true"
