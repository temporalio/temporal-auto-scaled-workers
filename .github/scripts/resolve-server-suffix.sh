#!/usr/bin/env bash
#
# Resolve the server-release suffix for a WCI module tag.
#
# The WCI tag is v<wciMajor>.<wciMinor>.<wciPatch>-<serverVersion>, where
# <serverVersion> is the pinned go.temporal.io/server build tag flattened to digits
# and dots (v1.32.0-158.3 -> 1.32.0.158.3). This prints that suffix.
#
# Resolution:
#   1. Read the go.temporal.io/server pin from go.mod (or arg $1).
#   2. If it's already a clean build tag (vX.Y.Z-BUILD.REV), reformat and use it.
#   3. Otherwise it's a pseudo-version (safety net): extract the commit hash, find
#      the earliest server build tag that *contains* that commit, and use that. (Go
#      bases a pseudo-version on the nearest ancestor tag, which can sit many builds
#      behind — and on a different minor line — from where the commit really shipped.)
#
# Failure modes (both exit non-zero):
#   - commit not reachable in temporal    -> can't build against it anyway
#   - commit ahead of every release tag   -> WCI must track a *released* server
#
# Env:
#   EMIT           "suffix" (default) prints the flattened suffix (1.32.0.158.3);
#                  "tag" prints the resolved server build tag (v1.32.0-158.3) — used
#                  by the release guard to suggest a repin target.
#   TEMPORAL_DIR   use an existing temporal checkout instead of cloning (tests)
#   GITHUB_TOKEN   auth for the clone (public repo; avoids the unauth rate limit)
#   SERVER_REPO    override clone URL (default github.com/temporalio/temporal)
set -euo pipefail

# A semver numeric identifier: no leading zeros. Server build tag v1.32.0-158.03 would
# flatten to the suffix 1.32.0.158.03, and `1.0.0-1.32.0.158.03` is not valid semver —
# Go and the module proxy reject it. Fail here instead of at tag-push time.
NUM='(0|[1-9][0-9]*)'
RELEASE_TAG_RE="^v${NUM}[.]${NUM}[.]${NUM}-${NUM}[.]${NUM}$"

# Flatten a server build tag to the WCI-tag suffix: v1.32.0-158.3 -> 1.32.0.158.3
# (drop the leading v, turn the base/build separator '-' into '.').
to_suffix() {
  local tag="${1#v}"
  printf '%s\n' "${tag/-/.}"
}

# Emit the resolved server build tag ($1): the flattened suffix by default, or the
# tag itself when EMIT=tag (the guard uses that to suggest a repin target).
emit() {
  if [[ "${EMIT:-suffix}" == "tag" ]]; then
    printf '%s\n' "$1"
  else
    to_suffix "$1"
  fi
}

# Set only when we create a throwaway clone; removed on exit.
CLONE_DIR=""
cleanup() { [[ -n "$CLONE_DIR" ]] && rm -rf "$CLONE_DIR"; return 0; }
trap cleanup EXIT

server_pin() {
  if [[ $# -ge 1 && -n "${1:-}" ]]; then
    printf '%s\n' "$1"
    return
  fi
  local ver
  ver=$(grep 'go\.temporal\.io/server ' "${GOMOD:-go.mod}" | awk '{print $2}')
  [[ -n "$ver" ]] || { echo "no go.temporal.io/server pin in ${GOMOD:-go.mod}" >&2; exit 1; }
  printf '%s\n' "$ver"
}

# Earliest server release tag (by globally-monotonic build number) that contains
# $1 in its history. Empty output if none.
earliest_containing() {
  local hash="$1"
  git -C "$TEMPORAL_DIR" tag --contains "$hash" --list 'v*' 2>/dev/null \
    | grep -E "$RELEASE_TAG_RE" \
    | sort -t- -k2 -V \
    | head -1
}

main() {
  local ver
  ver=$(server_pin "${1:-}")

  # Already a clean build tag — no lookup needed.
  if [[ "$ver" =~ $RELEASE_TAG_RE ]]; then
    emit "$ver"
    return
  fi

  # Build-tag shape but not canonical semver — say so, rather than reporting it as an
  # unrecognised pin below.
  if [[ "$ver" =~ ^v[0-9]+[.][0-9]+[.][0-9]+-[0-9]+[.][0-9]+$ ]]; then
    echo "server pin '$ver' has a leading-zero component; the flattened suffix would not be valid semver" >&2
    exit 1
  fi

  # Pseudo-version: the commit hash is the final dash-delimited segment.
  local hash="${ver##*-}"
  if [[ ! "$hash" =~ ^[0-9a-f]{12}$ ]]; then
    echo "server pin '$ver' is neither a release tag nor a pseudo-version" >&2
    exit 1
  fi

  # Get a commit graph (no trees/blobs) unless one was provided.
  if [[ -z "${TEMPORAL_DIR:-}" ]]; then
    TEMPORAL_DIR=$(mktemp -d)
    CLONE_DIR="$TEMPORAL_DIR"
    local url="${SERVER_REPO:-https://github.com/temporalio/temporal.git}"
    if [[ -n "${GITHUB_TOKEN:-}" && "$url" == https://github.com/* ]]; then
      url="https://x-access-token:${GITHUB_TOKEN}@${url#https://}"
    fi
    git clone --filter=tree:0 --no-checkout --quiet "$url" "$TEMPORAL_DIR"
  fi

  # Failure mode 1: commit not reachable in the repo.
  if ! git -C "$TEMPORAL_DIR" cat-file -e "${hash}^{commit}" 2>/dev/null; then
    echo "server commit ${hash} is not reachable in temporal (force-pushed, unmerged, or private)" >&2
    exit 1
  fi

  local earliest
  earliest=$(earliest_containing "$hash")

  # Failure mode 2: reachable but not yet in any release.
  if [[ -z "$earliest" ]]; then
    echo "server commit ${hash} is not in any server release yet; WCI must track a released server build" >&2
    exit 1
  fi

  emit "$earliest"
}

main "$@"
