# Changelog

All notable changes to Boop are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `boop status` — a command-line equivalent of `GET /api/status` (§54). Reports
  build metadata, the active provider's health and latency, the model's
  discovered capabilities and context window, execution mode, agent bounds and
  the network toggle. Exits non-zero when the active provider is unreachable, so
  it composes in a script or a health check. `boop status --json` for a
  machine-readable form; `boop --status` is the flag equivalent. It builds no
  session store and writes no logs.

- `make fuzz` — the last of the §36 optional targets. Runs Go's native fuzzing
  over two property targets: `FuzzParseRenderRoundTrip` (an untouched `Boop.md`
  survives `Render(Parse(b)) == b`) and `FuzzClassifyCommand` (the shell risk
  classifier never panics and always returns a gated category and risk).
  `FUZZTIME` overrides the per-target budget.

- `lint` and `format` tools, mirroring `test` and `build` (§2.8). `lint` detects
  golangci-lint or `go vet`, eslint, ruff or flake8, cargo clippy, phpstan, or a
  Makefile `lint` target. `format` detects gofmt, prettier, black or ruff, cargo
  fmt, php-cs-fixer, and takes `check` for a read-only "is this formatted"
  (gated as `filesystem.read`; a rewrite is `filesystem.write`). TUI slash
  commands `/lint` and `/format [--check]` come with them.

### Fixed

- Project memory (`Boop.md`) now actually reaches the model, and reaches it
  again after a `/prep` without a restart (#7). `app.New` was passing
  `LoadOrCreate` a file path where it wanted a directory, so the TUI and CLI
  always ran on an empty in-memory document. `App.Memory()` is now an
  atomically-swapped snapshot with `App.ReloadMemory()`; the TUI rebuilds its
  system message every turn, and `/prep` (terminal and `POST /api/project/prep`)
  reloads the file into the running runtime.

### Changed

- `provider.EmbeddingProvider` documents that it is implemented and verified but
  deliberately unused for now, with semantic project search as the intended
  future adopter (PROJECT.md §7).

## [0.1.0-rc.1] - 2026-08-29

First release candidate. The core runtime, tool layer, permission engine,
provider adapters, terminal UI and local WebUI are implemented and tested.
See "Known limitations" for what is not yet finished.

### Added

**Core runtime**

- Provider-neutral model runtime. `Provider` returns a channel of events, so
  streaming is the base case rather than an add-on.
- Adapters for Lemonade, LM Studio, Ollama, OpenAI, Anthropic and xAI, plus a
  generic OpenAI-compatible adapter the others build on. Anthropic is native
  because the Messages API is a different dialect.
- Model router with routing classes, capability-aware selection and fallback
  that retries only genuinely retryable failures.
- Tool loop that returns structured results to the model — including failures —
  so it can diagnose, repair, retry and validate.
- Session persistence in SQLite, with a context manager that bounds each
  request rather than resending the whole conversation.
- Project memory in `Boop.md`, round-trip safe so hand edits survive.

**Tools** — 14 registered: `read`, `write`, `edit`, `list`, `find`, `search`,
`run`, `git`, `test`, `build`, `http`, `fetch`, `websearch`, `attach`.

**Permissions**

- Two modes, four risk levels, ten categories, evaluated in code rather than
  requested by prompt.
- Production actions require explicit authorization and are not unlocked by
  `--dangerously-unrestricted`.
- Command classification covering destructive filesystem operations, storage
  and mount operations, privilege escalation, deployment tooling, and
  fetch-and-execute pipelines.

**Interfaces**

- Terminal UI with streaming output, inline approvals and slash commands.
- Local WebUI on loopback with a WebSocket event stream, refusing to start when
  bound publicly without authentication.
- Plain CLI mode for scripts and CI, reading a prompt from arguments or stdin.
- `boop prep` implementing the project inspection sequence.

**Other**

- Outbound web access — URL fetching and DuckDuckGo search — off by default,
  behind an SSRF guard, identifying itself with an RFC 9110 User-Agent.
- Document input: text, images, PDF and OOXML.
- Agent scheduler with bounded concurrency, dependency ordering, write-conflict
  detection and per-agent tool allowlists.
- Structured logging to a file with credential redaction.
- Usage, token and cost tracking that never presents an estimate as a
  measurement.

### Known limitations

- Only the **Ollama** adapter is verified against a live server. The
  **Lemonade** and **LM Studio** adapters were written against inferred
  endpoints, are marked `INFERRED` at each declaration, and want verification.
- The native GUI is not implemented; it is deliberately the last milestone.
- Small local models are unreliable at tool calling — measured at roughly 60–80%
  on a trivial task for 7–8B general-purpose models. See `MODELS.md`.
- PDF extraction handles unencrypted PDF 1.0–1.7 with a text layer. Scans,
  encrypted files and unusual filters return a named error rather than empty
  text, but are not extracted.
- Windows process-tree termination is weaker than on Unix: there is no job
  object, so grandchildren may orphan.

[Unreleased]: https://github.com/kawaiipantsu/boop/compare/v0.1.0-rc.1...HEAD
[0.1.0-rc.1]: https://github.com/kawaiipantsu/boop/releases/tag/v0.1.0-rc.1
