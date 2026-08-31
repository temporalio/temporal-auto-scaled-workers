# Releasing `temporal-auto-scaled-workers`

A release is a git tag `v<major>.<minor>.<patch>-<serverVersion>` — WCI's own semver
core plus the `go.temporal.io/server` build tag that `go.mod` pins. Server's build tag is flattened to
digits and dots (server `v1.32.0-158.3` results in WCI `v1.0.0-1.32.0.158.3`). Downstream
consumers `go get` that tag.

The tag *is* the release. No GitHub Release is published — WCI is consumed as a Go
library, so there are no artifacts to attach to one.

Tagging and the gates are automated and are triggered by pushing a release branch

## Cut a release (manual step)

Pick `MAJOR` and `MINOR` for WCI's own version and a base commit on `main`. **The branch name
`release/vMAJOR.MINOR` is the only place you declare MAJOR.MINOR** — there's no
version file or variable to edit. The automation parses them off the branch and
computes PATCH and the server suffix for you.

- **New feature:** cut a new MINOR release: `release/v1.1`
- **Breaking API change:** cut a new MAJOR release: `release/v2.0`
- **Bug fix on an existing version:** don't cut anything; merge it on the existing
  `release/vMAJOR.MINOR` branch and PATCH auto-increments (see [Patches](#patches)).

**Prerequisite: `go.mod` must pin `go.temporal.io/server` to a build-tag release**
(`vX.Y.Z-BUILD.REV`, e.g. `v1.32.0-158.3`), not a pseudo-version. This sets the
release dependency on a released server build *and* pins WCI's own CI to the exact build the tag
will claim. The release **fails** otherwise.

**That pin belongs on the release branch, not on `main`.** `main` tracks server tip as
a pseudo-version and keeps doing so; the repin is a release-time change. What matters
is that the build tag is in the commit at the tip of the branch when you push it —
that's the commit `check-pin` and the gates run against. Cutting the branch from
`origin/main` takes nothing from your working tree, so repin after the branch exists:

```shell
RELEASE_BRANCH="release/v1.0"          # MAJOR.MINOR live here
BASE="origin/main"                     # or a specific reviewed SHA

git fetch origin main
git switch -c "${RELEASE_BRANCH}" "${BASE}"

# The resolver prints the earliest build tag containing the commit go.mod points at.
# Pin that, or a later build you've validated, in both modules.
TAG=$(EMIT=tag bash .github/scripts/resolve-server-suffix.sh)   # e.g. v1.32.0-158.0
go get "go.temporal.io/server@${TAG}" && go mod tidy
( cd tests && go get "go.temporal.io/server@${TAG}" && go mod tidy )   # tests/ is a separate module
git commit -am "chore: pin go.temporal.io/server ${TAG}"

git push -u origin "${RELEASE_BRANCH}"  # this triggers .github/workflows/release-auto-tag.yml
```

If the branch already pins a build tag, `go get` changes nothing and `git commit` has
nothing to commit — skip both. If you skip the pin entirely, `check-pin` fails and
prints this same command.

## What happens automatically after the push

Pushing the branch runs **`release-auto-tag.yml`**, which validates, then tags:

- `check-pin` guards that `go.mod`'s server pin is a build-tag release. Anything else —
  a pseudo-version, or a plain `vX.Y.Z` — fails, and it prints the repin command,
  naming the exact build tag when `resolve-server-suffix.sh` can resolve one. It reads
  the root `go.mod`; `tests/go.mod` is covered by `ci.yaml`'s go.mod sync check below.
- `ci` and `lint` run the same gates a PR runs — `ci.yaml` (go.mod sync between the
  root and `tests/`, unit tests, integration tests) and `golangci-lint.yaml` — against
  the commit being released. Both are called, not merely triggered, so they block the
  tag push instead of racing it. Lint covers the whole tree here; on a PR it is
  limited to the diff.
- `auto-tag` runs `.github/scripts/compute-release-tag.sh` and pushes what it returns:
  the next WCI patch on the `vMAJOR.MINOR.*` version (seeding at `.0`), suffixed with the
  pinned build tag flattened by `resolve-server-suffix.sh` (`v1.32.0-158.3` flattens to
  `1.32.0.158.3`). Runs are serialized, so back-to-back pushes tag in order.

That push is the end of the process — the tag is the release.

## Patches

Land the fix on the same `release/vMAJOR.MINOR` branch (cherry-pick or PR into it).
Each push re-runs the automation and cuts the next patch (`v1.0.1-…`, `v1.0.2-…`).
The suffix follows whatever server build tag `go.mod` pins on that branch (bump it
with `go get go.temporal.io/server@<build-tag>` to move to a newer server).

## New minor / major 

Cut a fresh branch (`release/v1.1`, `release/v2.0`); it seeds its own `.0` patch.

## Break-glass

- Re-run a failed `release-auto-tag.yml` from the Actions UI. It re-runs on the same
  commit, and if that commit is already tagged it reuses the tag instead of cutting a
  second one, so a re-run after a partial failure is safe.
- Preview the tag a branch would get, without pushing anything:
  `BRANCH=release/v1.0 .github/scripts/compute-release-tag.sh`
- A tag pushed **by hand** bypasses all of this. Nothing validates it and nothing
  re-runs the gates, and the module proxy caches it the moment it resolves, so it
  cannot be retracted — only superseded by a higher patch. Let the automation cut
  tags.
