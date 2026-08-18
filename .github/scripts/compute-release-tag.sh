#!/usr/bin/env bash
#
# Compute the next WCI release tag for a release branch.
#
# Prints v<wciMajor>.<wciMinor>.<wciPatch>-<serverVersion>: major/minor come from the
# release/vMAJOR.MINOR branch name, patch is one past the highest already tagged on that
# line, and the suffix is the pinned server build tag flattened to digits and dots
# (resolve-server-suffix.sh). Nothing is created or pushed — the caller does that.
#
# Reads tags from the current repo, so run it in a checkout with tags fetched
# (fetch-depth: 0), on the commit being released.
#
# Args:
#   $1  release branch (default: $BRANCH)
#
# Env:
#   BRANCH         release branch, when not passed as $1
#   GITHUB_TOKEN   inherited by resolve-server-suffix.sh for its clone
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# Semver numeric identifier: no leading zeros anywhere in the tag we mint, or Go and the
# module proxy reject it as non-canonical.
NUM='(0|[1-9][0-9]*)'
WCI_TAG_RE="^v${NUM}[.]${NUM}[.]${NUM}(-${NUM}([.]${NUM})*)?$"
# What the caller may push: the server suffix is mandatory, unlike WCI_TAG_RE above,
# which also has to match older or hand-made tags when scanning the line.
MODULE_TAG_RE="^v${NUM}[.]${NUM}[.]${NUM}-${NUM}([.]${NUM})+$"

# Next WCI patch on the $1.$2 line, across any server suffix: one past the highest
# existing tag by version sort — not git describe's nearest ancestor, which an
# out-of-order tag on an older commit would reset. 0 when the line has no tags yet.
next_patch() {
  local major="$1" minor="$2" tag
  tag=$(git tag --list "v${major}.${minor}.*" | grep -E "$WCI_TAG_RE" | sort -V | tail -1) || true
  if [[ -z "$tag" ]]; then
    printf '0\n'
    return
  fi
  # Anchored on this line's major.minor, so BASH_REMATCH[1] is the patch.
  if [[ ! "$tag" =~ ^v${major}[.]${minor}[.]${NUM}(-${NUM}([.]${NUM})*)?$ ]]; then
    echo "tag '$tag' does not match v${major}.${minor}.PATCH[-build.rev]" >&2
    exit 1
  fi
  printf '%s\n' "$((BASH_REMATCH[1] + 1))"
}

main() {
  local branch="${1:-${BRANCH:-}}"
  if [[ -z "$branch" ]]; then
    echo "usage: $(basename "$0") <release/vMAJOR.MINOR>  (or set BRANCH)" >&2
    exit 1
  fi

  # The branch encodes WCI's own major.minor.
  if [[ ! "$branch" =~ ^release/v${NUM}[.]${NUM}$ ]]; then
    echo "branch '$branch' does not match release/vMAJOR.MINOR" >&2
    exit 1
  fi
  local major="${BASH_REMATCH[1]}" minor="${BASH_REMATCH[2]}"

  # Re-run at an already-tagged commit (job re-run, or a push that changed nothing on
  # this line): reuse that tag. Computing a successor would leave one commit carrying
  # two release tags.
  local tag
  tag=$(git tag --points-at HEAD --list "v${major}.${minor}.*" | grep -E "$WCI_TAG_RE" | sort -V | tail -1) || true
  if [[ -n "$tag" ]]; then
    echo "HEAD is already tagged $tag" >&2
  else
    local suffix
    suffix=$(bash "$SCRIPT_DIR/resolve-server-suffix.sh")
    echo "server suffix: $suffix" >&2

    local patch
    patch=$(next_patch "$major" "$minor")
    tag="v${major}.${minor}.${patch}-${suffix}"
  fi

  # The module proxy caches a pushed tag immutably, so check the assembled result before
  # handing it to the caller — both paths build it from separately-validated parts.
  if [[ ! "$tag" =~ $MODULE_TAG_RE ]]; then
    echo "tag '$tag' is not a vMAJOR.MINOR.PATCH-<serverVersion> module version" >&2
    exit 1
  fi

  printf '%s\n' "$tag"
}

main "$@"
