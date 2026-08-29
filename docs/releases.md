# Releases

Boop uses Git Flow, semantic versioning and a `Makefile`-driven build. Nothing
in the release process needs GitHub: CI is additive, and a full set of release
artifacts can be produced from a clean checkout on a Linux box with Go
installed.

## Branching model

Two long-lived branches:

| Branch | Contents |
|---|---|
| `main` | Release history only. Every release state is tagged. No normal feature development |
| `develop` | Integration branch. The basis for new feature branches |

Three kinds of short-lived branch:

```
feature/<short-name>     from develop  →  merges to develop
release/<version>        from develop  →  merges to main AND develop
hotfix/<version-or-desc> from main     →  merges to main AND develop
```

Examples: `feature/provider-interface`, `feature/tui-layout`,
`release/0.1.0`, `hotfix/0.1.1`.

Rules that matter:

- Never commit normal feature work directly to `main`.
- A release branch merges into **both** `main` and `develop`. Forgetting
  `develop` means the next release silently reverts the version bump and the
  changelog.
- Tag `main`, not the release branch.
- Do not rewrite published history once a remote exists. Local interactive
  rebase before merging a feature branch is fine while nothing is shared.

Merges use `--no-ff` so the branch structure survives in the history.

## Versioning

Semantic versioning, tags prefixed with `v`: `v0.1.0`, `v0.2.1`, `v1.0.0`.

Tags are annotated:

```bash
git tag -a v0.1.0 -m "Boop v0.1.0"
```

Development happens at `0.1.0-dev`. The first tagged release is `v0.1.0`, cut
once milestones 0–7 are stable enough for Boop to be genuinely useful as a
local coding client.

Because the project is pre-1.0, the minor number carries breaking changes.
After 1.0, ordinary semver rules apply.

## Where the version comes from

The `Makefile` derives `VERSION` from the first `## [x.y.z]` heading in
`CHANGELOG.md`, falling back to `0.1.0-dev`:

```make
VERSION ?= $(shell sed -n 's/^## \[\([0-9][^]]*\)\].*/\1/p' CHANGELOG.md | head -1)
```

So the changelog is the single source of truth for the version, and preparing a
release means writing the changelog entry. You can override it for a one-off
build:

```bash
make build VERSION=0.2.0-rc1
```

## Build metadata

Injected at link time into `internal/version`:

| Variable | Source |
|---|---|
| `Version` | `VERSION`, from the changelog |
| `Commit` | `git rev-parse --short HEAD`, or `unknown` |
| `Dirty` | `true` when `git status --porcelain` is non-empty |
| `Date` | UTC timestamp — **omitted** when `SOURCE_DATE_EPOCH` is set |

```
$ ./boop version
boop v0.1.0-dev (dirty)
commit: b7829b9
built:  2026-08-29T16:42:47Z
go:     go1.27.0
os/arch: linux/amd64
```

Setting `SOURCE_DATE_EPOCH` drops the date from the binary, which is how you
get a byte-reproducible build:

```bash
SOURCE_DATE_EPOCH=1 make build
```

`boop version` and `boop --version` print the same thing. It is what a bug
report should lead with.

## `make release-check`

```make
release-check: fmt-check vet lint test build-all
	@scripts/release-check.sh $(VERSION)
```

The prerequisites do the real work: formatting is verified (not applied), `go
vet` runs, the linter runs (`golangci-lint` if installed, otherwise `go vet`
again), the full test suite runs, and all six targets cross-build.

Then `scripts/release-check.sh` verifies four release-specific facts:

| Check | Fails when |
|---|---|
| Working tree is clean | `git status --porcelain` is non-empty |
| `CHANGELOG.md` has an entry for the version | No `[VERSION]` heading found |
| Tag is available | `v<VERSION>` already exists |
| No release-blocking TODOs | Any `.go` file contains `TODO: before release` |

It reports every failure rather than stopping at the first, and exits non-zero
if any failed.

```
$ make release-check
release-check for v0.1.0
  ok     working tree is clean
  ok     CHANGELOG.md has an entry for 0.1.0
  ok     tag v0.1.0 is available
  ok     no release-blocking TODOs
release-check passed
```

The "release-blocking TODOs" convention is worth using: write `// TODO: before
release — …` for anything that must not ship, and it will stop the release
rather than being remembered or not.

## `make dist`

```make
dist: build-all
	@scripts/package.sh $(DIST) $(BINARY) $(VERSION)
```

`build-all` cross-compiles all six targets into
`dist/boop_<version>_<os>_<arch>/`, then `scripts/package.sh` archives each
directory and writes checksums:

- `*_windows_*` → `.zip` (requires `zip`)
- everything else → `.tar.gz`
- `sha256sum` of every archive → `dist/checksums.txt`

```
dist/
├── boop_0.1.0_linux_amd64/boop
├── boop_0.1.0_linux_amd64.tar.gz
├── boop_0.1.0_linux_arm64/boop
├── boop_0.1.0_linux_arm64.tar.gz
├── boop_0.1.0_darwin_amd64/boop
├── boop_0.1.0_darwin_amd64.tar.gz
├── boop_0.1.0_darwin_arm64/boop
├── boop_0.1.0_darwin_arm64.tar.gz
├── boop_0.1.0_windows_amd64/boop.exe
├── boop_0.1.0_windows_amd64.zip
├── boop_0.1.0_windows_arm64/boop.exe
├── boop_0.1.0_windows_arm64.zip
└── checksums.txt
```

`make snapshot` is `dist` with a version of `<VERSION>-snapshot.<commit>`, for
handing someone a build without cutting a release.

## Cross-compilation targets

Six first-class targets, all built from a Linux host with no toolchain beyond
Go:

| Platform | amd64 | arm64 |
|---|:--:|:--:|
| Linux | `make build-linux-amd64` | `make build-linux-arm64` |
| macOS | `make build-darwin-amd64` | `make build-darwin-arm64` |
| Windows | `make build-windows-amd64` | `make build-windows-arm64` |

Group targets: `build-linux`, `build-darwin`, `build-windows`, `build-all`.

Every build sets `CGO_ENABLED=0` and `-trimpath`. Pure Go is a hard requirement
(§37), which is specifically why the SQLite driver is `modernc.org/sqlite`
rather than a cgo binding. Verify a change has not broken it:

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build ./...
```

Anything that introduces cgo breaks five of the six targets and is a spec
change, not an implementation detail. If a native GUI later needs
platform-specific tooling, it stays out of the standard core/TUI cross-build
path.

`-ldflags "-s -w"` strips the symbol table and DWARF data. Windows binaries get
a `.exe` suffix automatically.

## Changelog convention

`CHANGELOG.md` follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

Sections, in this order, omitting the empty ones:

```
Added
Changed
Deprecated
Removed
Fixed
Security
```

Unreleased work accumulates under `## [Unreleased]`. Cutting a release turns
that heading into `## [0.1.0] - 2026-08-29` and starts a fresh `[Unreleased]`
above it.

Two rules:

- **Release notes are generated from actual merged work, not invented.** If it
  is not in the diff, it does not go in the changelog.
- **Write for the reader, not the committer.** "Fixed a race in the approval
  broker that could drop an event" is useful; "fix bug" is not.

The changelog also drives `VERSION`, so a malformed heading breaks the build
metadata. Keep the `## [x.y.z]` shape exact.

## Cutting a release

```bash
# 1. Start from an up-to-date develop.
git switch develop
git pull --ff-only

# 2. Branch.
git switch -c release/0.1.0

# 3. Turn [Unreleased] into [0.1.0] - <date> in CHANGELOG.md,
#    and add a fresh empty [Unreleased] above it.
$EDITOR CHANGELOG.md

# 4. Verify. This is the gate; do not skip it.
make release-check

# 5. Build the artifacts.
make dist

# 6. Commit the release preparation.
git commit -am "chore: prepare v0.1.0"

# 7. Merge to main and tag there.
git switch main
git merge --no-ff release/0.1.0
git tag -a v0.1.0 -m "Boop v0.1.0"

# 8. Merge back to develop — do not skip this.
git switch develop
git merge --no-ff release/0.1.0

# 9. Clean up.
git branch -d release/0.1.0

# 10. Publish.
git push origin main develop
git push origin v0.1.0
```

Every release must, per §33:

1. pass tests,
2. pass lint and static checks,
3. build all supported targets,
4. produce versioned artifacts,
5. update `CHANGELOG.md`,
6. merge to `main`,
7. carry an annotated tag,
8. merge back to `develop`.

Steps 1–4 are `make release-check && make dist`.

## Hotfixes

A hotfix branches from `main`, not `develop`, because `develop` may contain
unreleased work you do not want to ship.

```bash
git switch main
git switch -c hotfix/0.1.1
# fix, then bump CHANGELOG.md to [0.1.1]
make release-check
git commit -am "fix: …"

git switch main
git merge --no-ff hotfix/0.1.1
git tag -a v0.1.1 -m "Boop v0.1.1"

git switch develop
git merge --no-ff hotfix/0.1.1

git branch -d hotfix/0.1.1
```

## CI

`.github/workflows/ci.yml` runs on pushes and pull requests against `main` and
`develop`, in four jobs:

| Job | Runs |
|---|---|
| Test | `make deps`, `make fmt-check`, `make vet`, `make test`, `make race` |
| Cross-build | `make build-all` |
| Lint | `golangci-lint` |
| Vulnerability scan | `govulncheck ./...` |

CI is additive. Nothing in the local build depends on it, and a contributor
with no network can still run every check locally — that is invariant §64.7.

Release artifact publication is not automated yet. `make dist` produces
everything an upload would need, including `checksums.txt`.

## Packaging beyond archives

Current: `tar.gz` for Linux and macOS, `zip` for Windows, SHA-256 checksums.

Planned, in no committed order: Homebrew, Debian package, RPM, Arch package, a
Windows package-manager entry, a macOS package. None of these blocks core
development.
