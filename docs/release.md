# Releasing `temporal-auto-scaled-workers`

A release is a git tag `v<major>.<minor>.<patch>-<serverVersion>` — WCI's own semver
core plus the `go.temporal.io/server` build tag that `go.mod` pins, flattened to
digits and dots (server `v1.32.0-158.3` → WCI `v1.0.0-1.32.0.158.3`). Downstream
consumers `go get` that tag.

Tagging, gates, and the GitHub Release are automated. **You do exactly one manual
step: cut the release branch.** Its push drives everything else.

## Cut a release (the only manual step)

Pick MAJOR.MINOR for WCI's own line and a base commit on `main`. **The branch name
`release/vMAJOR.MINOR` is the only place you declare MAJOR.MINOR** — there's no
version file or variable to edit. The automation parses them off the branch and
computes PATCH and the server suffix for you. So:

- **New feature** → cut a new MINOR line: `release/v1.1`
- **Breaking API change** → cut a new MAJOR line: `release/v2.0`
- **Bug fix on an existing line** → don't cut anything; land it on the existing
  `release/vMAJOR.MINOR` branch and PATCH auto-increments (see [Patches](#patches)).

**Prerequisite: `go.mod` must pin `go.temporal.io/server` to a build-tag release**
(`vX.Y.Z-BUILD.REV`, e.g. `v1.32.0-158.3`), not a pseudo-version. This keeps the
release on a released server build *and* pins WCI's own CI to the exact build the tag
will claim. The release **fails** otherwise.

Let the resolver tell you the tag — it prints the earliest build tag containing the
commit `go.mod` currently points at — then pin it (in both modules), or pin a later
build you've validated:

```shell
TAG=$(EMIT=tag bash scripts/resolve-server-suffix.sh)   # earliest build tag, e.g. v1.32.0-158.0
go get "go.temporal.io/server@${TAG}" && go mod tidy
( cd tests && go get "go.temporal.io/server@${TAG}" && go mod tidy )   # tests/ is a separate module
# commit the go.mod/go.sum changes (root + tests/) before cutting the branch
```

If `go.mod` already pins a build tag this is a no-op; if you skip it, the guard fails
and prints this same command.

Then cut the branch:

```shell
RELEASE_BRANCH="release/v1.0"          # MAJOR.MINOR live here — v1.0 → tag v1.0.0-<server>
BASE="origin/main"                     # or a specific reviewed SHA

git fetch origin main
git branch "${RELEASE_BRANCH}" "${BASE}"
git push origin "${RELEASE_BRANCH}"    # ← triggers .github/workflows/release-auto-tag.yml
```

## What happens automatically after the push

1. **`release-auto-tag.yml`** computes the next tag:
   - guards that `go.mod`'s server pin is a build-tag release; on a pseudo-version it
     fails and prints the exact repin command (via `resolve-server-suffix.sh`).
   - server suffix ← `scripts/resolve-server-suffix.sh`: the pinned build tag,
     flattened (e.g. `v1.32.0-158.3` → `1.32.0.158.3`). (If the pin is ever a
     pseudo-version, the script falls back to the earliest server build tag
     containing that commit — the safety net.)
   - next WCI patch on the `vMAJOR.MINOR.*` line (seeds at `.0`).
   - pushes the tag
2. **`release-tag.yml`** runs `golangci-lint` + `make unit-test`, then publishes the
   GitHub Release.

## Patches

Land the fix on the same `release/vMAJOR.MINOR` branch (cherry-pick or PR into it).
Each push re-runs the automation and cuts the next patch (`v1.0.1-…`, `v1.0.2-…`).
The suffix follows whatever server build tag `go.mod` pins on that branch (bump it
with `go get go.temporal.io/server@<build-tag>` to move to a newer server).

## New minor / major line

Cut a fresh branch (`release/v1.1`, `release/v2.0`); it seeds its own `.0` patch.

## Break-glass

- Re-publish an existing tag: run `release-tag.yml` via **workflow_dispatch** with the
  tag.
- A tag pushed **by a human** (not `GITHUB_TOKEN`) also triggers `release-tag.yml`
  directly.
