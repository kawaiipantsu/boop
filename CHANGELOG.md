# Changelog

All notable changes to Boop are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
