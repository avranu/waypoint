#!/usr/bin/env bash
set -euo pipefail

file=.waypoint-queue
if [ ! -f "$file" ]; then
  echo "::error title=Merge queue stack::$file is missing on this branch"
  exit 1
fi
heads=()
while read -r n sha _; do
  case "$n" in '' | '#'*) continue ;; esac
  if ! [[ "$n" =~ ^[0-9]+$ ]] || ! [[ "$sha" =~ ^[0-9a-f]{40}([0-9a-f]{24})?$ ]]; then
    echo "::error title=Merge queue stack::bad line in $file: $n $sha"
    exit 1
  fi
  heads+=("$n $sha")
done <"$file"
if [ "${#heads[@]}" = 0 ]; then
  echo "::error title=Merge queue stack::$file lists no heads"
  exit 1
fi

if [ "$(git rev-parse --is-shallow-repository)" = true ]; then
  git fetch -q --no-tags --unshallow origin
fi
git checkout -q --detach HEAD^
base=$(git rev-parse HEAD)
for h in "${heads[@]}"; do
  n=${h%% *} sha=${h#* }
  git fetch -q --no-tags origin "+refs/pull/$n/head:refs/remotes/pull/$n"
  if ! git cat-file -e "$sha^{commit}" 2>/dev/null; then
    echo "::error title=Merge queue stack::#$n's head $sha isn't on the forge any more"
    exit 1
  fi
  if ! git -c user.name=Waypoint -c user.email=waypoint@localhost merge -q --no-ff --no-edit "$sha"; then
    echo "::error title=Merge queue stack::#$n ($sha) doesn't merge cleanly onto the heads before it"
    git merge --abort || true
    git checkout -q --detach "$base"
    if ! git -c user.name=Waypoint -c user.email=waypoint@localhost merge -q --no-ff --no-commit "$sha" >/dev/null 2>&1; then
      echo "::error title=Merge queue stack::#$n ($sha) doesn't merge cleanly onto the base ($base) itself"
    fi
    git merge --abort 2>/dev/null || true
    exit 1
  fi
  echo "waypoint-stack #$n $sha"
done
echo "waypoint-stack-tree $(git rev-parse 'HEAD^{tree}')"
