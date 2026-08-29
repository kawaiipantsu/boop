# Contributing to Boop

Thanks for looking. Boop is a local-first AI client and agent runtime written
in Go.

Before you start, read [`PROJECT.md`](PROJECT.md). It is the authoritative
specification and the engineering contract for this project — 70 sections
covering everything from the provider interface to the release procedure. When
the spec and the code disagree, the spec wins; changing the spec is a
deliberate act, not a side effect of an implementation.

[`docs/architecture.md`](docs/architecture.md) is the shorter orientation, and
the better place to start if you want to make a change today.

## Getting set up

You need Go 1.25 or newer and Git. That is the whole list — `CGO_ENABLED=0`
throughout, so there is no C toolchain, no system SQLite and no node
requirement unless you are working on the WebUI frontend.

```bash
git clone https://github.com/kawaiipantsu/boop.git
cd boop
make build
./boop version
```

`make help` lists every target.

## The pre-commit loop

```bash
make fmt vet test build
```

Run all four, every time, before you commit. They are fast, and CI runs the
same things plus the race detector and the linter.

Narrower runs while you work:

```bash
go test ./internal/permissions/ -run TestClassify -v
make test-unit                    # unit tests only
make test-integration             # -tags=integration
make test-e2e                     # -tags=e2e
make race                         # race detector
make coverage                     # writes coverage.html
make lint                         # golangci-lint if installed, else go vet
```

Tests never need the network or a paid API. `tests/fixtures` provides a
scriptable fake provider server covering the OpenAI, Anthropic and
Ollama-native shapes — including streaming, tool calls, malformed responses,
HTTP failures and timeouts — plus a deterministic fake `Provider` for driving
the repair loop.

Live-server tests are opt-in and skip when their environment variable is unset,
so `make test` stays green offline:

```bash
BOOP_TEST_OPENAI_COMPAT_URL=http://127.0.0.1:11434/v1 BOOP_TEST_MODEL=qwen2.5:7b \
  go test ./internal/provider/... -run Live -v

BOOP_NETWORK_TESTS=1 go test ./internal/webclient/... -run Live -v
```

To exercise the binary without touching your real configuration:

```bash
BOOP_CONFIG_DIR=/tmp/boop-test ./boop --no-tui -v "read notes.txt and count the lines"
```

Cross-compilation is a hard requirement. If you touch a dependency, verify it
still holds:

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build ./...
make build-all
```

## Branches

Git Flow, from day one.

| Branch | From | Merges to |
|---|---|---|
| `feature/<short-name>` | `develop` | `develop` |
| `release/<version>` | `develop` | `main` **and** `develop` |
| `hotfix/<version-or-desc>` | `main` | `main` **and** `develop` |

- `main` is release history only. Never commit normal feature work to it.
- `develop` is the integration branch and the base for feature branches.
- Merges use `--no-ff`, so branch structure survives in the history.
- Releases are annotated tags on `main`. See [docs/releases.md](docs/releases.md).

```bash
git switch develop
git switch -c feature/provider-interface
```

Local interactive rebase to tidy a feature branch before merging is fine while
nothing is shared. Do not rewrite history that has been pushed.

## Commit messages

Conventional-commit prefixes:

```
feat:     a new capability
fix:      a bug fix
test:     tests only
docs:     documentation only
refactor: behaviour-preserving change
build:    build system, Makefile, cross-compilation
chore:    dependencies, housekeeping
```

Examples from this project's own history style:

```
feat: add provider capability interface
fix: handle command timeout on windows
test: add ollama provider integration test
docs: document web proxy setup
refactor: separate execution policy from tool registry
build: add linux arm64 cross-build target
```

Keep commits small and cohesive. One logical change per commit. No giant
"everything" commits — they cannot be reviewed and they cannot be reverted.

Write the body when the *why* is not obvious from the diff. The diff already
says what changed.

## Definition of done (§62)

A change is done only when all of these are true:

- [ ] The implementation is complete — no stubs left behind that a caller could
      reach.
- [ ] Tests exist where appropriate.
- [ ] The relevant tests pass.
- [ ] The build passes, including cross-compilation if you touched
      dependencies.
- [ ] User-facing behaviour is documented — in `docs/`, and in `CHANGELOG.md`
      under `[Unreleased]`.
- [ ] Errors are handled. Not swallowed, not `panic`, not a bare `return err`
      that loses the context the user needs.
- [ ] Platform assumptions are explicit. Windows and Unix differences live
      behind the `execution` platform abstraction.
- [ ] No secrets are introduced. Not in code, not in fixtures, not in
      `Boop.md`.
- [ ] Code is formatted (`make fmt`).
- [ ] Git history is meaningful.
- [ ] `Boop.md` is updated if the change embeds a durable project decision.

## House rules

These come out of the architecture and will save you a review round.

**`Result` versus `error` in tools.** Anything the model can plausibly repair —
a missing file, a non-zero exit, an ambiguous edit, a denied action — returns
`tools.Result{IsError: true}` with an explanatory message. A Go `error` is
reserved for input that is structurally unusable. Returning an `error` where a
`Result` belongs breaks the repair loop, and the repair loop is the product.

**Everything goes through the permission layer.** A tool that touches the OS
without reporting a `permissions.Action` from `Permission()` first is a hole in
the only thing standing between a model and the machine. Do not bypass it to
make an implementation easier.

**The permission seam carries three things.** `RiskClassifier` returns a full
`permissions.Classification`: category, risk *and* the production flag. It once
returned only the risk, which quietly filed `terraform apply` as
`shell.execute` with `Production: false` and skipped the production gate. If
you touch that seam, keep all three.

**Providers stay behind the interface.** Vendor-specific behaviour belongs in
`internal/provider/<vendor>/`, never in shared UI or tool code. New vendor
features go in optional interfaces, never by widening `Provider`. See
[docs/providers.md](docs/providers.md) for how to add one.

**Capabilities are discovered, never assumed.** Where a server reports them
(Ollama's `/api/tags`), that is authoritative and replaces heuristics.

**Two memories, kept apart.** `Boop.md` is human-readable durable knowledge;
SQLite holds machine state. Never collapse one into the other, and never put a
raw transcript in `Boop.md`.

**Dependency minimalism.** Standard library first, then small mature
dependencies with clear maintenance and licensing. Do not add a library for
something the stdlib does. Anything that pulls in cgo breaks five of six
release targets and is a spec change, not an implementation detail.

**Document what you inferred.** If you write an adapter against documentation
rather than a running server, say so in the package doc comment and mark each
inferred constant at its declaration, as `internal/provider/lemonade` and
`internal/provider/lmstudio` do.

**Do not claim it works without running it.** This applies to humans and to
agents working in this repository.

## What a good pull request looks like

- **One thing.** A PR that fixes a bug and refactors three packages is two PRs.
- **A title in conventional-commit form**, matching the change.
- **A description that says why**, not just what. What was broken, what you
  expected, what the alternatives were and why you did not take them.
- **Tests that would have caught the bug.** For a fix, the ideal is one commit
  adding a failing test and one making it pass.
- **A changelog entry** under `[Unreleased]`, in the right section
  (`Added`/`Changed`/`Deprecated`/`Removed`/`Fixed`/`Security`).
- **Docs updated** if a user can see the change.
- **Green CI** — test, cross-build, lint, vulnerability scan.
- **No unrelated churn.** Reformatting a file you happened to open makes the
  real change invisible.

If you are unsure whether an approach fits the spec, open an issue and ask
before writing it. A design conversation is cheaper than a rewrite.

## Reporting bugs

Use the issue templates. The bug report asks for `boop version` output, your
OS, the provider and model, and your config with secrets removed — those four
things resolve most reports without a round trip.

## Security

Do not open a public issue for a vulnerability. See [SECURITY.md](SECURITY.md).

## Code of conduct

By participating you agree to abide by the
[Code of Conduct](CODE_OF_CONDUCT.md).

## Licence

Contributions are accepted under the MIT Licence, the same terms as the project
([LICENSE](LICENSE)).
