# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Current state

This repository currently contains **only `PROJECT.md`** — a 2990-line implementation specification. There is no Go module, no source tree, and **no Git repository yet**.

`PROJECT.md` is the authoritative engineering contract. When spec and code disagree, the spec wins; changing the spec is a deliberate act, not a side effect of implementation.

Per `PROJECT.md` §67, the bootstrap sequence is: `git init` → create `main` and `develop` → commit `PROJECT.md` → `go mod init` → repo skeleton → minimal `cmd/boop/main.go` → `boop version` → Makefile → base unit test → README/CHANGELOG/.gitignore → CI skeleton → `make test` && `make build` → commit → branch `feature/provider-interface`.

Do not jump ahead to agents, WebUI, or GUI. Milestones 0–14 are listed in §59 and are ordered by dependency.

## What Boop is

A cross-platform Go binary (`boop`) that is an AI execution environment, not a chat frontend: provider-neutral model runtime + local tool execution + permission engine + session/project memory + agent scheduler, fronted by a TUI, plain CLI, local WebUI, and (much later) a native GUI.

Primary providers are **local**: Lemonade, LM Studio, Ollama. Cloud (OpenAI, Anthropic, xAI) is an optional extension, never a prerequisite.

## Commands

None of these exist yet — creating the Makefile that provides them is a Milestone 0 task. The required target list is in §36; `make help` must be the discovery entry point.

```bash
make run                  # run boop
make fmt vet lint         # format + static checks
make test                 # full suite
make test-unit            # unit only
make test-integration     # integration only
make test-e2e             # end-to-end only
make coverage
make build                # host binary
make build-all            # cross-build all targets
make web-build            # build embedded WebUI assets
make dist                 # distributable archives into dist/
make release-check        # verify release readiness
```

Standard Go idioms apply for narrower runs, e.g. `go test ./internal/provider/... -run TestName -v`.

Before every commit: `make fmt && make vet && make test && make build`.

## Architecture

Everything hangs off a UI-independent core. Frontends are thin.

```
TUI / plain CLI / WebUI / GUI   →   BOOP CORE   →   Model Router   →   Provider adapters
```

The core (`internal/`) holds: `app` (lifecycle, events), `session` (context, history, persistence), `agent` (scheduler, coordinator, planner), `tools` (registry + the tool implementations), `execution` (process/pty, platform-specific), `permissions` (policy, evaluator, classifications), `project` (discovery, `/prep`, `Boop.md`), `provider` (interface, capabilities, router, per-vendor adapters), `config`, `store` (SQLite), `stats`, `documents`. UI lives outside core in `tui/` and `web/`. Full layout in §6.

Key structural decisions that span multiple files:

- **Provider interface** (§7): `Name/Health/ListModels/Chat/Capabilities`. `Chat` returns a `<-chan ChatEvent` — streaming is the base case. Vendor-specific features go in *optional* interfaces (e.g. `ModelLifecycleProvider`, `EmbeddingProvider`), never widening the base interface. Lemonade/LM Studio/Ollama reuse the generic `openaicompat` path and add native code only where genuinely required.
- **Capabilities are runtime metadata** (§8), not assumptions. Never assume tools/vision/reasoning support; when a task needs a missing capability, explain it and offer compatible configured models.
- **Event bus** (§25) is transport-neutral. TUI and WebUI subscribe to the same stream — that is what keeps frontends from accumulating logic.
- **Tools return structured results.** The `run` tool (§13) is central: `RunResult` carries exit code, stdout, stderr, duration, timed-out, cancelled. Failure is data fed back to the model, driving the error-repair loop (execute → structured error → diagnose → repair → retry → validate) under configured limits (`max_tool_iterations`, `max_retries_per_command`, `command_timeout`).
- **Permissions are enforced in code, not prompts** (§14). Modes `confirm` and `auto`; internal risk levels low/medium/high/critical; policy is per-category YAML (filesystem, shell, git, network, production). Approvals must work identically from TUI, WebUI, and future GUI.
- **Two memories, deliberately separate.** `Boop.md` is human-readable, compressed durable project knowledge (§16 defines its section layout). SQLite (`boop.db`) holds machine-oriented session state, stats, tool metadata, event history. Never collapse one into the other, and never put raw transcripts into `Boop.md`.
- **Agent context is scoped** (§10). Agents get their task, needed requirements, relevant files, explicitly allowed tools, and relevant memory — not a copy of the main conversation. The scheduler bounds concurrency (default max 5), understands dependencies and write conflicts, and prevents recursive spawning.
- **Context manager selects explicitly** (§47). Never blind-send the repo or full session.

## Architectural invariants (§64)

Violating any of these is a spec change, not an implementation detail:

1. Core does not depend on a specific UI; provider-specific behavior never leaks into shared UI code.
2. All tool actions pass through the permission layer — no bypassing it to make implementation easier.
3. Local development works fully without GitHub; CI is additive.
4. WebUI is local/LAN-oriented by default (`127.0.0.1:8585`); public exposure requires an explicit proxy/security layer, and binding beyond loopback must warn and support token auth + origin validation.
5. Production changes require deliberate intent — describe change, rationale, affected systems, risks, then ask (§2.9, §15).
6. `main` is releases only; normal work flows through `develop`; releases are tagged.

## Git workflow

Git Flow from day one, local-only but always push-ready.

- `feature/<short-name>` branches from `develop`, merges back to `develop`.
- `release/<version>` merges into **both** `main` and `develop`; annotated tag on `main` (`git tag -a v0.1.0 -m "Boop v0.1.0"`).
- `hotfix/<...>` branches from `main`, merges to both.
- Conventional commit prefixes (`feat:`, `fix:`, `test:`, `docs:`, `refactor:`, `build:`, `chore:`). Small cohesive commits; no "everything" commits.
- Never commit normal feature work directly to `main`.

Version starts at `v0.1.0-dev`; first real tag `v0.1.0` once Milestones 0–7 are solid.

## Constraints worth remembering

- **Pure Go / avoid CGO.** Cross-compilation to linux/darwin/windows × amd64/arm64 is a hard requirement, which specifically constrains the SQLite driver choice (§37).
- **Dependency minimalism** (§40): stdlib first, then small mature deps. `net/http` for the server. Bubble Tea for the TUI, but do not couple core logic to Charm types.
- **Secrets** (§45): config references env vars (`api_key_env: OPENAI_API_KEY`), never literal keys — and never in `Boop.md` or fixtures. Redact keys from logs, WebUI, transcripts, crash reports.
- **Config paths** are XDG/OS-native (§18); never hardcode user-specific paths.
- **Tests use fakes, not paid APIs.** Build the reusable fake provider server (§42) covering streaming, tool calls, malformed responses, HTTP failures, timeouts — E2E flows run against deterministic fake providers.
- Platform execution differences (Windows vs Unix shells) stay encapsulated behind the `execution` platform abstraction.

## Task discipline (§61–62)

Do not claim code works without running the tests. Do not invent API behavior or ignore failed commands. Avoid unrelated large refactors. A feature is done when: implemented, tested, tests pass, build passes, user-facing behavior documented, errors handled, platform assumptions explicit, formatted, and `Boop.md` updated with any durable decision.
