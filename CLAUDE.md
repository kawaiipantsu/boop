# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What Boop is

A cross-platform Go binary (`boop`) that is an AI execution environment, not a chat frontend:
a provider-neutral model runtime, a local tool execution engine, a permission engine, session
and project memory, and an agent scheduler — fronted by a plain CLI, a TUI, a local WebUI, and
later a native GUI.

`PROJECT.md` is the authoritative specification (70 sections). When spec and code disagree, the
spec wins; changing it is a deliberate act, not a side effect of implementation. Section numbers
below refer to it.

**Repo:** `github.com/kawaiipantsu/boop` · **Go 1.25+** · `CGO_ENABLED=0` everywhere.

## Commands

```bash
make help                    # discover every target
make fmt vet test build      # the pre-commit loop — run all four
make test-unit               # unit only
make test-integration        # -tags=integration
make test-e2e                # -tags=e2e
make race coverage lint
make build-all dist          # six targets; archives + checksums
make release-check
```

Narrower runs: `go test ./internal/permissions/ -run TestClassify -v`.

Live-provider tests are opt-in and skip when unset, so `make test` stays green offline:

```bash
BOOP_TEST_OPENAI_COMPAT_URL=http://127.0.0.1:11434/v1 BOOP_TEST_MODEL=qwen2.5:7b \
  go test ./internal/provider/... -run Live -v
BOOP_NETWORK_TESTS=1 go test ./internal/webclient/... -run Live -v
```

Running it against a real server:

```bash
BOOP_CONFIG_DIR=/tmp/boop-test ./boop --no-tui -v "read notes.txt and count the lines"
```

## Architecture

Everything hangs off a UI-independent core. Frontends are thin and subscribe to one event stream.

```
CLI / TUI / WebUI  →  internal/app  →  provider.Router  →  adapters
                           ↓
              tools · permissions · session · store · project · stats
```

- **`internal/app`** is the only package that knows about all the others. `New()` assembles a
  runtime from config; `Loop.Run()` drives one user turn: model → tool calls → permission
  evaluation → execution → structured results → model again, until the model answers or
  `MaxToolIterations` is hit. Adding a subsystem means wiring it here, not spreading it around.
- **`internal/provider`** holds the frozen `Provider` interface (`Chat` returns a
  `<-chan ChatEvent`, so streaming is the base case), capabilities as *runtime metadata*, and
  normalized error categories. Vendor-specific features go in optional interfaces
  (`ModelLifecycleProvider`, `EmbeddingProvider`), never by widening `Provider`.
  `openaicompat` is the shared base; the local and cloud adapters build on it, and `anthropic`
  is native because the Messages API is a different dialect.
- **`internal/tools`** is a schema-driven registry. Each tool reports a
  `permissions.Action` from `Permission()` *before* `Execute()` runs, which is what makes
  gating possible.
- **`internal/permissions`** is a security boundary. It decides; prompts do not.
- **`internal/session` + `internal/store`** hold machine state in SQLite.
  **`internal/project`** holds human-readable memory in `Boop.md`. Never collapse one into
  the other (invariant §64.5, §64.6).

### Things that will bite you

- **`Result` vs `error` in tools.** Anything the model can repair — a missing file, a non-zero
  exit, an ambiguous edit, a denied action — returns `Result{IsError: true}` with an
  explanatory message. Go `error` is reserved for structurally unusable input. Returning an
  error where a `Result` belongs breaks the repair loop, which is the product.
- **The permission seam carries three things, not one.** `RiskClassifier` returns a full
  `permissions.Classification` — category, risk *and* the production flag. It used to return
  only `Risk`, which silently filed `terraform apply` as `shell.execute` with
  `Production: false` and skipped the production gate. If you touch that seam, keep all three.
- **Production sits above `--dangerously-unrestricted`.** Precedence is: explicit `deny` →
  unauthorized production → critical risk → unrestricted → confirm mode → auto rules. Only
  explicit authorization unlocks production (§2.9, §15, §64.14).
- **A stream that yields nothing is an error**, not an empty answer. `Loop.collect` checks
  cancellation after the loop as well as inside it, because a stream with no events never
  enters the body.
- **Capabilities are discovered, never assumed** (§8). Ollama's `/api/tags` reports them
  authoritatively and *replaces* name heuristics — guessing tool support for `qwen:7b` earns
  an HTTP 400.
- **Two real Ollama quirks** the adapters handle and tests pin: the final usage SSE frame
  carries `"choices":[]` (indexing `Choices[0]` panics), and tool calls arrive **complete in
  one delta**, unlike OpenAI's fragmented arguments. Handle both.
- **Timestamps in the store** are fixed-width UTC padded to nine fractional digits, because
  RFC3339Nano trims trailing zeros and makes `…05Z` sort after `…05.5Z`.

## Invariants (§64)

Violating these is a spec change, not an implementation detail:

1. Core does not depend on a UI; provider specifics never leak into shared UI code.
2. All tool actions pass through the permission layer.
3. Local development works without GitHub; CI is additive.
4. WebUI is loopback by default; public exposure needs an explicit proxy and auth layer.
5. Production changes require deliberate intent.
6. `main` is releases only; work flows through `develop`; releases are tagged.

## Git workflow

Git Flow. `feature/<name>` branches from `develop` and merges back; `release/<version>` merges
into both `main` and `develop` with an annotated tag on `main`; `hotfix/<...>` branches from
`main`. Conventional commit prefixes. Never commit normal feature work directly to `main`.

Remote is `git@github.com:kawaiipantsu/boop.git`. Both branches are pushed.

## Constraints

- **Pure Go, no CGO.** Six cross-compile targets are a hard requirement (§37) — this is why
  the SQLite driver is `modernc.org/sqlite`. Verify with
  `CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build ./...`.
- **Dependency minimalism** (§40): stdlib first. `net/http` for the server, `log/slog` for
  logging, no HTML parser (the webclient does tolerant regex extraction and says so).
- **Secrets** (§45): config names an env var via `api_key_env`; a literal key is rejected by
  shape. Keys are redacted from logs, errors and transcripts. Never put one in a fixture.
- **Tests never need the network or a paid API** (§41). `tests/fixtures` has a scriptable fake
  provider server covering OpenAI, Anthropic and Ollama-native shapes, plus a deterministic
  fake `Provider` for driving the repair loop.
- **Outbound web access is off by default** (`network.enabled`). Every request carries an
  RFC 9110 User-Agent containing "boop", validated so a custom override cannot drop it.

## Current state

Built and tested: config, execution, permissions, tools (13), all seven providers plus the
router, session, store, project memory, stats, webclient, the app runtime and the tool loop,
and plain CLI mode (`boop --no-tui`).

In progress: TUI (§19), agent scheduler (§10–11), WebUI (§22–26), documents (§27), logging
(§44). Not started: native GUI (§13, deliberately last).

Two provider caveats worth repeating: only the **Ollama** adapter has been verified against a
live server. **Lemonade and LM Studio** were written against inferred endpoints and mark them
`INFERRED` at each declaration — their live tests report and skip rather than fail, so one run
against real servers produces a correction list.

## Task discipline (§61–62)

Do not claim code works without running the tests. Do not invent API behaviour or ignore failed
commands. Avoid unrelated refactors. A feature is done when: implemented, tested, tests pass,
build passes, user-facing behaviour documented, errors handled, platform assumptions explicit,
formatted, and `Boop.md` updated with any durable decision.
