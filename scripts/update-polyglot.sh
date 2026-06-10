#!/usr/bin/env bash
# Bump the polyglot submodule (third_party/polyglot-src) to a release tag.
#
# Usage:
#   scripts/update-polyglot.sh                # upgrade to the latest vX.Y.Z release
#   scripts/update-polyglot.sh v0.5.1         # upgrade/downgrade to a specific tag
#   scripts/update-polyglot.sh --check        # only print current vs latest, change nothing
#   scripts/update-polyglot.sh --no-verify    # skip the FFI rebuild + test suite (CI bump
#                                             # PRs rely on the PR's own CI for validation)
#
# What it does: fetches tags, checks out the tag in the submodule, syncs the
# require line in go.mod (the replace directive makes the submodule the real
# source), rebuilds the FFI lib, and runs the test suite. It does NOT commit —
# review `git diff` and commit the gitlink + go.mod yourself.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
SUBMODULE=third_party/polyglot-src
MODULE=github.com/tobilg/polyglot/packages/go

check=0
verify=1
tag=""
for arg in "$@"; do
  case "$arg" in
    --check) check=1 ;;
    --no-verify) verify=0 ;;
    -*) echo "usage: $0 [--check] [--no-verify] [tag]" >&2; exit 2 ;;
    *) tag=$arg ;;
  esac
done

git submodule update --init "$SUBMODULE"
git -C "$SUBMODULE" fetch --tags --quiet origin

current=$(git -C "$SUBMODULE" describe --tags --always)
latest=$(git -C "$SUBMODULE" tag --list 'v[0-9]*' --sort=-v:refname | head -1)

if (( check )); then
  echo "current: $current"
  echo "latest:  $latest"
  exit 0
fi

tag=${tag:-$latest}
if ! git -C "$SUBMODULE" rev-parse --verify --quiet "refs/tags/$tag" >/dev/null; then
  echo "error: tag '$tag' not found in $SUBMODULE (try: git -C $SUBMODULE tag --list 'v*')" >&2
  exit 1
fi

if [[ "$(git -C "$SUBMODULE" rev-parse HEAD)" == "$(git -C "$SUBMODULE" rev-parse "refs/tags/$tag^{commit}")" ]]; then
  echo "already at $tag ($current) — nothing to do"
  exit 0
fi

echo "updating polyglot: $current -> $tag"
git -C "$SUBMODULE" checkout --quiet "refs/tags/$tag"
# Stage the new gitlink right away: `git submodule update` (run by `make ffi`
# below) resets the submodule to the SHA recorded in the parent index, which
# would silently undo the checkout above.
git add "$SUBMODULE"

# Keep the (inert, replace'd) require line in sync with the submodule pin so
# go.mod tells the truth about the version in use.
go mod edit -require="$MODULE@$tag"
go mod tidy

if (( verify )); then
  # Force an FFI rebuild: the Makefile target is satisfied by an existing lib.
  rm -f third_party/lib/libpolyglot_sql_ffi.*
  make test
fi

echo
echo "done. review and commit:"
git status --short -- "$SUBMODULE" go.mod go.sum
