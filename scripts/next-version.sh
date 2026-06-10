#!/usr/bin/env bash
# Print the next release version X.Y.Z (used by .github/workflows/release.yml):
#   X — major, passed as $1 (default 0); bump manually for breaking changes.
#   Y — +1 on the first release of a calendar day; Z resets to 0.
#   Z — +1 for further releases on the same day.
# The first release of a major is X.1.0. "Day" is evaluated in RELEASE_TZ
# (default Asia/Shanghai); a previous release's day comes from its annotated
# tag's creatordate, so tags must be created by the release workflow (or
# `git tag -a`) at release time.
set -euo pipefail

major=${1:-0}
if ! [[ "$major" =~ ^[0-9]+$ ]]; then
  echo "error: major must be a non-negative integer, got '$major'" >&2
  exit 2
fi
export TZ=${RELEASE_TZ:-Asia/Shanghai}

today=$(date +%Y-%m-%d)
latest=$(git tag --list "v${major}.*" --sort=-v:refname | head -1)
if [[ -z "$latest" ]]; then
  echo "${major}.1.0"
  exit 0
fi

IFS=. read -r _ y z <<<"${latest#v}"
last_day=$(git for-each-ref --format='%(creatordate:format-local:%Y-%m-%d)' "refs/tags/$latest")
if [[ "$last_day" == "$today" ]]; then
  echo "${major}.${y}.$((z + 1))"
else
  echo "${major}.$((y + 1)).0"
fi
