# BOOP — Project Implementation Specification

> **Purpose:** This document is the authoritative implementation plan and engineering contract for building **Boop**, a cross-platform AI CLI/TUI/WebUI/GUI client and local agent runtime.
>
> **Primary implementation environment:** Claude Code or another coding agent working directly in this repository.
>
> **Project name:** `boop`
>
> **Executable:** `boop`
>
> **Primary language:** Go
>
> **Repository model:** Local Git repository using Git Flow conventions from day one, with a clean path to publishing on GitHub later.

---

# 1. Executive Summary

Boop is a cross-platform local AI client and agent runtime inspired by modern coding-oriented CLI assistants, but designed to work across local and cloud LLM providers.

Its primary focus is first-class support for:

- Lemonade Server
- LM Studio
- Ollama

It must also support:

- OpenAI
- Anthropic / Claude
- xAI / Grok
- Generic OpenAI-compatible endpoints
- Future provider adapters without core redesign

Boop is not merely a chat frontend.

The core product is an **AI execution environment** containing:

- a provider-neutral model runtime,
- a local tool execution engine,
- a permission and safety policy engine,
- session/project memory,
- an agent scheduler,
- terminal UI,
- local LAN WebUI,
- optional desktop GUI,
- statistics and usage tracking,
- project inspection and initialization,
- reliable command execution and repair loops,
- testing and validation orchestration.

The user interface must be only a frontend to the Boop core. The TUI, WebUI, plain CLI mode, and later GUI must share the same runtime and behavior.

---

# 2. Core Product Principles

The following principles are architectural requirements.

## 2.1 Local-first

Boop must work fully with local LLM servers.

Cloud providers are optional extensions.

No telemetry is sent anywhere unless explicitly enabled in a future implementation.

## 2.2 Provider-neutral

The rest of Boop must not depend directly on Lemonade, LM Studio, Ollama, OpenAI, Anthropic, or xAI.

All model access must go through provider interfaces.

## 2.3 UI-independent core

Business logic must not live in the TUI or WebUI.

The frontends interact with the same application/core APIs.

## 2.4 Structured tool execution

Models do not receive direct unrestricted process handles.

Model-initiated actions go through registered Boop tools such as:

- `run`
- `read`
- `write`
- `edit`
- `search`
- `find`
- `list`
- `git`
- `test`
- `build`
- `lint`
- `format`
- `http`

## 2.5 Explicit permissions

Prompt instructions are not security controls.

Boop must enforce permissions independently of LLM output.

## 2.6 Recover from failure

A failed shell command is information.

Execution results must be returned to the model in structured form so it can:

1. diagnose the problem,
2. modify its plan,
3. repair files/configuration,
4. retry,
5. validate the result.

## 2.7 Preserve history

Boop must maintain:

- project memory,
- session history,
- local Git history,
- release tags,
- structured release notes.

## 2.8 Test by default

Unless the user explicitly says otherwise, Boop should validate its changes.

Relevant actions may include:

- unit tests,
- integration tests,
- static analysis,
- formatting,
- linting,
- builds,
- smoke tests.

## 2.9 Production requires deliberate intent

Boop must distinguish normal local/development work from production infrastructure.

Production changes must never happen merely because they appear useful.

Unless the user has explicitly authorized the relevant production work, Boop must:

1. describe the intended production change,
2. explain why it is needed,
3. identify affected systems/files,
4. identify risks,
5. request confirmation.

This policy belongs in both the system prompt and the permission engine.

---

# 3. Supported Platforms

Official first-class targets:

## macOS

- `darwin/amd64`
- `darwin/arm64`

## Linux

- `linux/amd64`
- `linux/arm64`

## Windows

- `windows/amd64`
- `windows/arm64`

Windows binaries must work from:

- PowerShell
- Command Prompt
- Windows Terminal

Experimental/community targets may include:

- `linux/386`
- `windows/386`

32-bit support must not block development of the primary 64-bit targets.

---

# 4. Primary Technology Stack

## 4.1 Language

Use **Go**.

Reasons:

- excellent cross-compilation,
- simple static deployment,
- strong concurrency model,
- good terminal ecosystem,
- strong networking support,
- easy embedded WebUI assets,
- native testing,
- straightforward process management,
- good suitability for a long-running agent runtime.

## 4.2 TUI

Use:

- Bubble Tea for application/update architecture,
- Tcell or compatible lower-level terminal capability where required,
- Bubbles/Lip Gloss where useful but avoid coupling core logic to Charm-specific types.

The UI should behave like a modern ncurses application, but direct ncurses usage is not required.

## 4.3 Web server

Use Go's standard HTTP stack unless a clear need for another router appears.

Recommended:

- `net/http`
- WebSocket implementation with a small, well-maintained dependency
- embedded static assets via `embed`

## 4.4 Web frontend

Keep the initial WebUI lightweight.

Preferred approach:

- TypeScript
- minimal framework or small modern framework
- local build output embedded into the Go binary

Avoid a frontend architecture that creates unnecessary operational complexity.

## 4.5 Native GUI

Do not implement the native GUI before the core, TUI, and WebUI are mature.

The `--gui` interface should be planned early, but native GUI implementation is a later milestone.

Potential future choices:

- Wails
- Fyne

Prefer Wails if reusing the WebUI is advantageous.

## 4.6 Configuration

Use YAML or TOML.

Prefer YAML unless another format gives a compelling implementation advantage.

## 4.7 Persistent structured data

Use SQLite for:

- sessions,
- aggregate statistics,
- tool execution metadata,
- model usage,
- agent metadata,
- searchable event history.

`Boop.md` remains human-readable project memory and must not be replaced by SQLite.

---

# 5. High-Level Architecture

```text
                         ┌─────────────────────┐
                         │      boop CLI        │
                         └──────────┬──────────┘
                                    │
                  ┌─────────────────┼──────────────────┐
                  │                 │                  │
                  ▼                 ▼                  ▼
             Terminal TUI       Local WebUI        Native GUI
              / Plain CLI       + WebSocket          later
                  │                 │                  │
                  └─────────────────┼──────────────────┘
                                    ▼
                           ┌──────────────────┐
                           │    BOOP CORE     │
                           ├──────────────────┤
                           │ Application      │
                           │ Session runtime  │
                           │ Context manager  │
                           │ Agent runtime    │
                           │ Tool registry    │
                           │ Permission engine│
                           │ Event bus        │
                           │ Stats            │
                           │ Project memory   │
                           └────────┬─────────┘
                                    │
                            ┌───────▼────────┐
                            │  Model Router   │
                            └───────┬────────┘
                                    │
             ┌──────────┬───────────┼──────────┬────────────┐
             ▼          ▼           ▼          ▼            ▼
         Lemonade   LM Studio     Ollama     OpenAI      Anthropic
                                                          / xAI
```

---

# 6. Repository Layout

Start with the following structure and evolve only when justified.

```text
boop/
├── cmd/
│   └── boop/
│       └── main.go
│
├── internal/
│   ├── app/
│   │   ├── app.go
│   │   ├── lifecycle.go
│   │   └── events.go
│   │
│   ├── agent/
│   │   ├── agent.go
│   │   ├── scheduler.go
│   │   ├── coordinator.go
│   │   ├── planner.go
│   │   └── status.go
│   │
│   ├── provider/
│   │   ├── provider.go
│   │   ├── capabilities.go
│   │   ├── router.go
│   │   ├── openaicompat/
│   │   ├── lemonade/
│   │   ├── lmstudio/
│   │   ├── ollama/
│   │   ├── openai/
│   │   ├── anthropic/
│   │   └── xai/
│   │
│   ├── tools/
│   │   ├── registry.go
│   │   ├── run.go
│   │   ├── read.go
│   │   ├── write.go
│   │   ├── edit.go
│   │   ├── search.go
│   │   ├── find.go
│   │   ├── list.go
│   │   ├── git.go
│   │   ├── test.go
│   │   ├── build.go
│   │   └── http.go
│   │
│   ├── execution/
│   │   ├── executor.go
│   │   ├── process.go
│   │   ├── pty.go
│   │   ├── result.go
│   │   └── platform_*.go
│   │
│   ├── permissions/
│   │   ├── policy.go
│   │   ├── evaluator.go
│   │   ├── classifications.go
│   │   └── approvals.go
│   │
│   ├── session/
│   │   ├── session.go
│   │   ├── context.go
│   │   ├── history.go
│   │   └── persistence.go
│   │
│   ├── project/
│   │   ├── discovery.go
│   │   ├── prep.go
│   │   ├── memory.go
│   │   └── boopmd.go
│   │
│   ├── documents/
│   │   ├── mime.go
│   │   ├── text.go
│   │   ├── image.go
│   │   ├── pdf.go
│   │   └── office.go
│   │
│   ├── stats/
│   │   ├── stats.go
│   │   ├── tokens.go
│   │   └── costs.go
│   │
│   ├── config/
│   │   ├── config.go
│   │   ├── defaults.go
│   │   └── paths.go
│   │
│   └── store/
│       ├── sqlite.go
│       └── migrations/
│
├── tui/
│   ├── app.go
│   ├── layout.go
│   ├── header.go
│   ├── transcript.go
│   ├── input.go
│   ├── footer.go
│   ├── menu.go
│   ├── agents.go
│   └── approvals.go
│
├── web/
│   ├── server.go
│   ├── api.go
│   ├── websocket.go
│   ├── auth.go
│   └── static/
│
├── gui/
│   └── README.md
│
├── prompts/
│   ├── system.md
│   ├── planner.md
│   ├── agent.md
│   └── reviewer.md
│
├── scripts/
│   ├── build-cross.sh
│   ├── release.sh
│   ├── verify-release.sh
│   └── install-local.sh
│
├── tests/
│   ├── integration/
│   ├── fixtures/
│   └── e2e/
│
├── docs/
│   ├── architecture.md
│   ├── providers.md
│   ├── permissions.md
│   ├── webui.md
│   └── releases.md
│
├── .github/
│   └── workflows/
│
├── Makefile
├── go.mod
├── go.sum
├── PROJECT.md
├── Boop.md
├── README.md
├── CHANGELOG.md
├── LICENSE
└── .gitignore
```

---

# 7. Provider Architecture

Provider support must begin with a generic OpenAI-compatible implementation.

```go
type Provider interface {
    Name() string
    Health(ctx context.Context) error
    ListModels(ctx context.Context) ([]Model, error)
    Chat(ctx context.Context, req ChatRequest) (<-chan ChatEvent, error)
    Capabilities(ctx context.Context, model string) (Capabilities, error)
}
```

Provider-specific extensions may be expressed through optional interfaces.

Examples:

```go
type ModelLifecycleProvider interface {
    LoadModel(ctx context.Context, model string) error
    UnloadModel(ctx context.Context, model string) error
}
```

```go
type EmbeddingProvider interface {
    Embed(ctx context.Context, req EmbeddingRequest) (EmbeddingResponse, error)
}
```

`EmbeddingProvider` is implemented by the OpenAI-compatible client and the local
adapters and is verified against a real server, but no subsystem calls it yet.
It is retained deliberately: the intended future use is semantic search over a
project as a complement to the regex `search` tool, deferred because it requires
an index to build and invalidate. Until something adopts it, it stays an
available capability rather than dead code to remove.

## Initial adapters

Implement:

1. Generic OpenAI-compatible
2. Lemonade
3. LM Studio
4. Ollama
5. OpenAI
6. Anthropic
7. xAI

The Lemonade, LM Studio, and Ollama implementations should reuse the OpenAI-compatible path wherever possible, adding native functionality only for features that require it.

---

# 8. Model Capabilities

Capabilities are first-class runtime metadata.

```go
type Capability string

const (
    CapabilityStreaming        Capability = "streaming"
    CapabilityTools            Capability = "tools"
    CapabilityVision           Capability = "vision"
    CapabilityReasoning        Capability = "reasoning"
    CapabilityResponses        Capability = "responses"
    CapabilityEmbeddings       Capability = "embeddings"
    CapabilityStructuredOutput Capability = "structured_output"
    CapabilityAudio            Capability = "audio"
)
```

Boop must never assume every provider/model supports all features.

When a requested task needs unsupported functionality, Boop should:

1. explain the missing capability,
2. list compatible configured models where possible,
3. allow model switching,
4. optionally route automatically when configured.

---

# 9. Model Routing

Support manual selection first.

Then implement configurable routing.

Example:

```yaml
routing:
  default:
    provider: lemonade
    model: qwen3

  vision:
    provider: lmstudio
    model: qwen-vl

  reasoning:
    provider: openai
    model: gpt-5

  fast:
    provider: ollama
    model: llama3
```

Provider fallback should be supported.

```yaml
fallback:
  - lemonade
  - lmstudio
  - ollama
  - openai
```

Routing decisions must be visible in debug/status output.

---

# 10. Agent Runtime

Agents are first-class runtime objects.

```go
type Agent struct {
    ID         string
    Name       string
    Task       string
    Provider   string
    Model      string
    Status     AgentStatus
    ParentID   string
    StartedAt  time.Time
    FinishedAt time.Time
}
```

Statuses:

- idle
- planning
- thinking
- working
- waiting
- running
- testing
- blocked
- error
- complete
- cancelled

Default maximum agents:

```text
5
```

Commands:

```text
/agents
/agents on
/agents off
/agents max <int>
/agents list
/agents stop <id>
```

## Agent context isolation

Do not copy the entire main conversation into every worker.

Each agent should receive:

- its task,
- necessary user requirements,
- relevant files/context,
- explicit allowed tools,
- relevant project memory.

Agents should not compete blindly for writes.

The scheduler must understand task dependencies and write conflicts.

---

# 11. Task Scheduler

Represent work as tasks.

```go
type Task struct {
    ID           string
    Description  string
    Dependencies []string
    AgentID      string
    Status       TaskStatus
}
```

Independent tasks may execute concurrently.

Example:

```text
Main task
├── inspect architecture
├── implement provider
├── write tests
└── review implementation
```

The scheduler must prevent uncontrolled recursive spawning.

Global concurrency is bounded by `/agents max`.

---

# 12. Tool Runtime

The tool system must be extensible and schema-driven.

Initial tools:

- `run`
- `read`
- `write`
- `edit`
- `search`
- `find`
- `list`
- `git`
- `test`
- `build`
- `http`

Tools return structured results.

---

# 13. The `run` Tool

This is one of Boop's most important features.

```go
type RunRequest struct {
    Command    string
    WorkingDir string
    Timeout    time.Duration
    Env        map[string]string
}
```

```go
type RunResult struct {
    Command   string
    ExitCode  int
    Stdout    string
    Stderr    string
    Duration  time.Duration
    TimedOut  bool
    Cancelled bool
}
```

The executor must capture:

- command,
- working directory,
- stdout,
- stderr,
- exit code,
- duration,
- timeout,
- cancellation,
- signal where available.

The result must be returned to the model.

## Error-repair loop

Typical loop:

```text
LLM
 ↓
tool call
 ↓
execute
 ↓
failure
 ↓
structured error result
 ↓
LLM diagnosis
 ↓
repair
 ↓
retry
 ↓
validation
```

Set configurable limits.

Example:

```yaml
agent:
  max_tool_iterations: 50
  max_retries_per_command: 3
  command_timeout: 300s
```

---

# 14. Permission Engine

Two user-facing execution modes are required:

- `confirm`
- `auto`

## Confirm mode

Any tool action requiring permission is presented to the user for approval.

Approvals must work from:

- TUI,
- WebUI,
- future GUI.

## Auto mode

Boop may perform approved categories of work without prompting.

However, internally maintain operation risk levels and policy classes.

Suggested risk levels:

- low
- medium
- high
- critical

Example policy:

```yaml
permissions:
  filesystem:
    read: allow
    write: confirm

  shell:
    execute: confirm

  git:
    read: allow
    commit: confirm
    push: confirm

  network:
    http: confirm

  production:
    change: confirm
```

An explicitly unrestricted mode may exist:

```bash
boop --mode auto --dangerously-unrestricted
```

This flag must be deliberately named and documented.

It should not be required for normal autonomous local development.

---

# 15. Production Safety

Production intent must be tracked separately from ordinary execution mode.

Operations that may affect production include:

- production server configuration,
- remote deployments,
- production databases,
- production secrets,
- firewall rules,
- destructive cloud operations,
- force pushes to protected release branches,
- destructive infrastructure changes.

Absent explicit prior authorization, Boop must provide an execution preview before doing such work.

---

# 16. Project Memory — `Boop.md`

Every project root used by Boop must contain:

```text
Boop.md
```

If missing, `/prep` creates it.

This is persistent project memory.

Suggested format:

```markdown
# Boop Project Memory

## Project

Name:
Root:
Languages:
Frameworks:

## Goals

## Architecture

## Important Files

## Decisions

## Current Work

## Known Problems

## Tests

## Useful Commands

## Agent Notes

## Session Summaries
```

Do not append complete raw transcripts forever.

Boop.md should contain compressed, useful project knowledge.

Raw/session-level data belongs in the session database.

---

# 17. `/prep`

`/prep` is Boop's project initialization command and is analogous in purpose to project initialization commands in other AI development tools.

It should:

1. detect repository root,
2. inspect languages and build systems,
3. inspect Git state,
4. inspect relevant project files,
5. identify test commands,
6. identify build commands,
7. identify lint/format commands,
8. inspect existing documentation,
9. create or update `Boop.md`,
10. summarize project architecture,
11. identify likely production-sensitive files,
12. present a concise ready-state summary.

Do not modify source code during `/prep` unless the user explicitly requests repair/setup work.

---

# 18. Configuration Paths

Use native OS conventions.

## Linux

```text
~/.config/boop/config.yaml
~/.local/share/boop/
~/.cache/boop/
```

Honor XDG environment variables.

## macOS

```text
~/Library/Application Support/boop/
~/Library/Caches/boop/
```

## Windows

Use appropriate roaming/local application data locations.

Never hardcode username-specific paths.

Suggested data:

```text
config.yaml
boop.db
sessions/
logs/
cache/
```

Secrets should preferentially come from environment variables or OS credential storage in a later milestone.

---

# 19. TUI Specification

The terminal UI is resizable and follows terminal dimensions.

Base structure:

```text
┌──────────────────────────────────────────────────────────────┐
│ BOOP │ Provider / Model / Status / Agents / Tokens           │
├──────────────────────────────────────────────────────────────┤
│                                                              │
│ Main conversation, command output, agent activity,           │
│ approvals, errors, and tool results                          │
│                                                              │
├──────────────────────────────────────────────────────────────┤
│ multiline prompt / command input                             │
├──────────────────────────────────────────────────────────────┤
│ command hints / mode / cwd / context                         │
└──────────────────────────────────────────────────────────────┘
```

## Header

Header layout:

- left: small Boop identity/version field,
- center/right: larger provider/model/status/menu area.

Display statuses such as:

- IDLE
- THINKING
- PLANNING
- WORKING
- RUNNING
- TESTING
- WAITING
- ERROR

Also show:

- active provider,
- model,
- current execution mode,
- agent count,
- session token usage.

## Main panel

Display:

- user prompts,
- model responses,
- tool calls,
- command output,
- stdout/stderr,
- approvals,
- agent updates,
- errors,
- testing output.

## Footer/input

Support:

- multiline input,
- command mode beginning with `/`,
- copy/paste,
- bracketed paste,
- input history,
- keyboard navigation.

Recommended behavior:

- Enter = newline in multiline mode
- Ctrl+Enter = submit
- Alt+Enter = submit alternative if terminal permits
- Esc = cancel current input/dialog
- Up/Down = history where appropriate
- Ctrl+R = search history

## Mouse

Support mouse interactions for:

- menus,
- status pane,
- agents,
- approvals,
- selectable links/items where practical.

---

# 20. Commands

Initial command set:

```text
/help

/prep
/init

/config
/provider
/model
/models

/agents
/agents on
/agents off
/agents max <int>
/agents list
/agents stop <id>

/run
/permissions

/status
/stats
/tokens

/session
/session new
/session list
/session save
/session load

/context
/context add
/context clear

/files
/tree
/search

/test
/build

/web
/web on
/web off

/gui

/clear
/reset

/quit
/exit

/boop
```

`/boop` may respond with a small harmless Easter egg.

---

# 21. CLI Startup Modes

Examples:

```bash
boop
```

Launch TUI.

```bash
boop "fix the failing tests"
```

Submit a prompt immediately and then continue interactively unless a one-shot flag is used.

```bash
boop --no-tui
```

Plain CLI mode.

```bash
boop --provider lemonade
```

```bash
boop --model qwen3
```

```bash
boop --mode confirm
```

```bash
boop --mode auto
```

```bash
boop --prompt "review this project"
```

```bash
boop --web
```

Start local WebUI.

```bash
boop --web --port 9000
```

Custom WebUI port.

```bash
boop --gui
```

Launch native GUI once implemented.

---

# 22. WebUI

The WebUI must run from the same Boop process.

Default port:

```text
8585
```

## Default network behavior

The WebUI is intended for local machine or trusted local LAN use.

Default bind behavior should be conservative.

Recommended modes:

```text
127.0.0.1:8585
```

for local-only usage.

Allow explicit LAN bind:

```text
0.0.0.0:8585
```

or a specific local interface.

Example:

```bash
boop --web --listen 0.0.0.0 --port 8585
```

Do not silently expose the WebUI publicly.

## External access / upstream proxy

The WebUI itself is not intended to be a public Internet service.

However, advanced users must be able to place an upstream reverse proxy in front of it.

Support deployments such as:

```text
Internet
   ↓
Reverse proxy / VPN / auth gateway / TLS
   ↓
Boop WebUI on LAN/private interface
```

Examples could later include:

- Nginx
- Caddy
- Traefik
- authenticated VPN/tunnel

Boop should document proxy headers and WebSocket requirements.

Do not implement public Internet exposure as a default convenience feature.

---

# 23. WebUI Security

When binding beyond loopback:

- clearly warn the user,
- support optional access token authentication,
- support configurable allowed origins,
- validate WebSocket origins,
- avoid unsafe wildcard CORS defaults.

Future options:

- password/token login,
- local TLS,
- proxy-aware trusted headers.

Never assume a LAN is automatically trustworthy.

---

# 24. Web API

Initial API surface may include:

```text
GET  /api/status
GET  /api/config
PUT  /api/config
GET  /api/models
GET  /api/providers
GET  /api/agents
GET  /api/sessions
GET  /api/stats
POST /api/message
POST /api/approval
POST /api/agents
POST /api/session
```

Use WebSockets or server-sent events for runtime events.

Prefer WebSocket because it can support interactive approvals and bidirectional runtime communication.

---

# 25. Event Bus

Core events should be transport-neutral.

Examples:

- session started,
- prompt received,
- model request started,
- model token received,
- model response completed,
- agent created,
- agent status changed,
- tool requested,
- approval requested,
- approval received,
- command started,
- command stdout,
- command stderr,
- command completed,
- test started,
- test completed,
- error,
- session completed.

The TUI and WebUI subscribe to the same event stream.

---

# 26. WebUI Layout

The WebUI should expose more than chat.

Recommended navigation:

```text
Chat
Project
Agents
Tools
Models
Statistics
Sessions
Settings
```

Agent monitor example:

```text
● Main       WORKING
● Research   COMPLETE
● Coder      RUNNING
● Tests      TESTING
○ Reviewer   WAITING
```

Selecting an agent should show:

- task,
- status,
- model,
- current operation,
- allowed tools,
- modified files,
- token use,
- runtime,
- recent output.

Approvals must be actionable directly from the WebUI.

---

# 27. Binary and Document Input

Boop should detect MIME type and choose a processing path.

Support targets:

- text/plain
- text/markdown
- JSON
- XML
- source code
- PNG
- JPEG
- WebP
- PDF
- DOCX
- common office/text container formats

Behavior:

```text
file
 ↓
MIME detection
 ↓
extract/transform if required
 ↓
capability check
 ↓
provider/model
```

Examples:

## Image

Send directly only if the model/provider supports vision.

## PDF

Try text extraction.

If page images or diagrams are important and the model supports vision, make page/image content available through the multimodal adapter.

## DOCX

Extract:

- text,
- tables where feasible,
- images where useful.

A text-only model should still be able to process textual content extracted from binary documents.

---

# 28. Statistics

Track per-session and all-time statistics.

Session examples:

- messages,
- input tokens,
- output tokens,
- total tokens,
- tool calls,
- command count,
- command failures,
- retries,
- agents spawned,
- agents completed,
- elapsed time.

Cloud provider usage may include estimated cost when pricing metadata is configured and known.

Local providers should normally show zero API cost while still tracking token usage.

---

# 29. System Prompt

Store prompts in versioned repository files.

Main prompt responsibilities:

- identify itself as Boop,
- understand user objectives,
- inspect projects,
- make plans,
- use tools rather than pretending actions occurred,
- interpret command results,
- repair failures,
- validate work,
- use agents when beneficial,
- maintain project memory,
- respect execution permissions,
- handle production cautiously,
- be concise when possible,
- report uncertainty.

Important policy text should include:

```text
You are Boop, an AI development and automation assistant running inside
the user's environment through controlled Boop tools.

You do not have direct shell or filesystem access outside registered tools.

Use tools to inspect facts instead of inventing results.

When changing code:
- inspect before editing,
- make focused changes,
- validate relevant behavior,
- run appropriate tests where practical,
- fix failures caused by your changes,
- summarize what changed.

When a command fails:
- inspect exit code, stdout, and stderr,
- diagnose the cause,
- correct the problem when appropriate,
- retry only when justified,
- stop repetitive loops.

Production systems require special care.
Do not alter production merely because it appears useful.
Unless explicitly authorized, describe the intended production change,
why it is needed, affected systems, and meaningful risks before proceeding.
```

Agent-specific prompts belong in separate files.

---

# 30. Git Requirements

Git is mandatory from project inception.

Claude must initialize and use Git before substantive implementation if the repository is not already initialized.

The repository is initially local.

The codebase must remain ready to push to GitHub without repository restructuring.

---

# 31. Git Flow Policy

Use Git Flow-style long-lived branches.

Primary branches:

```text
main
develop
```

Rules:

## `main`

- production/release history only,
- every release state must be tagged,
- no normal feature development directly on `main`.

## `develop`

- integration branch for completed features,
- normal basis for new feature branches.

## Feature branches

```text
feature/<short-name>
```

Examples:

```text
feature/provider-interface
feature/tui-layout
feature/run-tool
feature/webui
```

Branch from:

```text
develop
```

Merge back into:

```text
develop
```

## Release branches

```text
release/<version>
```

Example:

```text
release/0.1.0
```

Use for:

- final release stabilization,
- release notes,
- version updates,
- final bug fixes.

Merge release branch into both:

```text
main
develop
```

Tag `main`.

## Hotfix branches

```text
hotfix/<version-or-description>
```

Branch from `main`.

Merge back into:

- `main`
- `develop`

Tag the corrected release where appropriate.

---

# 32. Git Commit Rules

Commits should be small and meaningful.

Preferred conventional commit style:

```text
feat: add provider capability interface
fix: handle command timeout on windows
test: add ollama provider integration test
docs: document web proxy setup
refactor: separate execution policy from tool registry
build: add linux arm64 cross-build target
chore: update dependencies
```

Do not create giant "everything" commits.

Do not rewrite public/shared release history once a remote repository exists.

Local development may use interactive rebase before merging a feature branch if no shared remote history is affected.

---

# 33. Tags and Releases

Use semantic versioning.

Examples:

```text
v0.1.0
v0.2.0
v0.2.1
v1.0.0
```

Prefer annotated tags:

```bash
git tag -a v0.1.0 -m "Boop v0.1.0"
```

Every release must:

1. pass tests,
2. pass lint/static checks,
3. build all supported targets,
4. produce versioned artifacts,
5. update `CHANGELOG.md`,
6. merge to `main`,
7. create annotated tag,
8. merge release changes back to `develop`.

---

# 34. Changelog

Maintain:

```text
CHANGELOG.md
```

Use a Keep-a-Changelog-like format.

Sections:

```text
Added
Changed
Deprecated
Removed
Fixed
Security
```

Release notes must be generated from actual merged work, not invented.

---

# 35. GitHub Readiness

Even before a remote exists, maintain:

```text
.github/workflows/
```

Suggested workflows:

- tests,
- lint,
- cross-build,
- release artifacts,
- dependency/security scan.

Do not require GitHub-specific infrastructure to build locally.

GitHub automation must be additive, never a prerequisite for local development.

---

# 36. Build System

A root `Makefile` is required.

A fresh developer should be able to discover major tasks with:

```bash
make help
```

Required targets:

```text
help
deps
fmt
lint
vet
test
test-unit
test-integration
test-e2e
coverage
build
build-linux
build-linux-amd64
build-linux-arm64
build-darwin
build-darwin-amd64
build-darwin-arm64
build-windows
build-windows-amd64
build-windows-arm64
build-all
web
web-build
web-clean
clean
install
run
release-check
dist
snapshot
```

Optional targets:

```text
race
bench
fuzz
security
licenses
generate
```

---

# 37. Cross-Compilation

The Linux development host should be able to build as many release targets as reasonably possible.

Pure-Go code should be preferred in the core to preserve cross-compilation.

Avoid unnecessary CGO dependencies.

If SQLite is used, prefer a Go implementation or dependency strategy that does not make cross-compilation painful.

If a native GUI later requires platform-specific tooling, keep it separate from the standard core/TUI cross-build path.

Example release output:

```text
dist/
├── boop_0.1.0_linux_amd64/
│   └── boop
├── boop_0.1.0_linux_arm64/
│   └── boop
├── boop_0.1.0_darwin_amd64/
│   └── boop
├── boop_0.1.0_darwin_arm64/
│   └── boop
├── boop_0.1.0_windows_amd64/
│   └── boop.exe
└── boop_0.1.0_windows_arm64/
    └── boop.exe
```

---

# 38. Build Metadata

Embed into the executable:

- semantic version,
- Git commit,
- build date where reproducibility policy allows,
- dirty state if relevant.

Example:

```bash
boop version
```

Output:

```text
boop v0.3.0
commit: abc1234
```

Do not make build timestamps undermine reproducible builds unnecessarily.

---

# 39. Packaging

Initial packaging priorities:

- tar.gz for Linux/macOS
- zip for Windows
- checksums

Later:

- Homebrew
- Debian package
- RPM
- Arch package
- Windows installer/package manager
- macOS package

Packaging must not block core development.

---

# 40. Dependency Philosophy

Prefer:

1. Go standard library,
2. small, mature dependencies,
3. dependencies with clear maintenance and licenses.

Avoid adding libraries for trivial functionality.

Pin dependency versions through normal Go module mechanisms.

Review dependency updates deliberately.

---

# 41. Testing Strategy

## Unit tests

Required for:

- provider parsing,
- config,
- model routing,
- permission classification,
- command result parsing,
- project memory transforms,
- agent scheduling,
- state machines.

## Integration tests

Cover:

- fake OpenAI-compatible server,
- fake Lemonade/LM Studio/Ollama behaviors,
- streaming,
- tool calls,
- failures,
- retries,
- WebSocket events,
- config persistence.

Do not require actual paid APIs in default CI.

## End-to-end tests

Exercise representative flows:

```text
prompt
→ model response
→ run tool
→ failure
→ repair
→ test
→ success
```

Use deterministic fake providers for E2E tests.

---

# 42. Provider Test Harness

Build a reusable test server capable of emulating:

- `/v1/models`
- chat completions,
- streaming,
- tool calls,
- malformed responses,
- HTTP failures,
- delays/timeouts,
- model capability metadata.

This prevents provider code from depending entirely on live server testing.

---

# 43. Command Execution Tests

Test:

- stdout,
- stderr,
- non-zero exit,
- timeout,
- cancellation,
- working directory,
- environment variables,
- quoting,
- platform shell behavior,
- large output handling.

Windows and Unix execution differences should be encapsulated behind a platform abstraction.

---

# 44. Logs

Support structured logs.

Levels:

- trace
- debug
- info
- warn
- error

Logs should be written to the platform-appropriate data/log directory.

Do not display debug noise in the normal TUI transcript.

Allow:

```bash
boop --log-level debug
```

Sensitive values must be redacted.

---

# 45. Secrets

Never store cloud API keys directly in `Boop.md`.

Supported sources:

- environment variables,
- config references to environment variables,
- future OS credential store.

Example:

```yaml
providers:
  openai:
    api_key_env: OPENAI_API_KEY
```

Boop must redact API keys from:

- logs,
- WebUI,
- crash reports,
- transcripts.

---

# 46. Session Persistence

Each session should preserve:

- ID,
- project path,
- provider/model history,
- user prompts,
- assistant messages,
- tool calls,
- execution results,
- agent activity,
- token stats,
- timestamps.

Session loading must allow continued work.

Boop.md stores the summarized durable project memory.

SQLite stores machine-oriented session state.

---

# 47. Context Management

The context manager is responsible for:

- current conversation,
- Boop.md,
- relevant project files,
- selected command output,
- agent results,
- token budgeting,
- summarization.

Avoid blindly sending the whole repository or whole session to the model.

Implement explicit context selection.

---

# 48. Project File Awareness

Provide utilities to:

- detect Git root,
- list tree,
- honor `.gitignore`,
- avoid binary blobs unless requested,
- identify likely build/test files,
- identify language ecosystems.

Avoid automatically reading secrets.

Patterns that should be treated carefully include:

```text
.env
*.pem
*.key
credentials*
secrets*
```

The user can explicitly authorize reading them.

---

# 49. TUI Approval UX

Example:

```text
Boop wants to run:

  go test ./...

cwd:
  /home/user/project

Risk:
  LOW

[Approve] [Always for session] [Reject]
```

High-risk example:

```text
Boop wants to run:

  git push origin main

Risk:
  HIGH

This changes a remote repository.

[Approve once] [Reject]
```

---

# 50. WebUI Approval UX

Approvals must contain the same information as the TUI.

Never make WebUI approvals less explicit than TUI approvals.

Approval state must be synchronized across all connected interfaces.

---

# 51. User Interrupts and Cancellation

The user must be able to stop:

- model generation,
- commands,
- agents,
- current task.

Recommended:

```text
Ctrl+C
```

Behavior should be context-aware:

1. first Ctrl+C cancels active operation,
2. second can exit or show confirmation depending on state.

WebUI needs equivalent cancel buttons.

---

# 52. Plain CLI / Automation Mode

Support non-TUI use for scripting.

Example:

```bash
boop --no-tui --prompt "run tests and explain failures"
```

Future structured output:

```bash
boop --json --prompt "..."
```

Machine mode must not mix progress decorations with JSON stdout.

Use stderr for human progress if required.

---

# 53. WebUI Reverse Proxy Support

Document expected reverse-proxy configuration.

Requirements:

- WebSocket upgrade support,
- forwarded scheme/host handling only when trusted,
- configurable base path if later needed,
- no automatic trust of arbitrary `X-Forwarded-*` headers.

Recommended deployment:

```text
public client
   ↓ HTTPS
reverse proxy/auth layer
   ↓ trusted LAN/private connection
boop:8585
```

The reverse proxy is responsible for:

- TLS,
- public authentication,
- rate limiting if needed,
- Internet-facing hardening.

Boop remains primarily a local/LAN service.

---

# 54. Health and Status

Expose:

```text
/api/status
```

Status should include safe operational metadata:

- Boop version,
- uptime,
- session state,
- provider health,
- current model,
- agent counts.

Do not expose secrets.

The same information is available from the command line without starting a
session:

```text
boop status          # human-readable
boop status --json    # machine-readable
```

`boop status` probes the active provider and exits non-zero when it is
unreachable, so it composes in a script or a health check. It builds no session
store and writes no logs.

---

# 55. Configuration UX

`/config` with no arguments prints the effective configuration (credential
values never shown). Direct set commands cover every adjustable field, each
written to `config.yaml` and applied live where a running process can honour it
(`App.ApplyConfig`), naming the groups that need a restart otherwise:

```text
/config mode auto|confirm
/config provider <name>
/config model <id>|default
/config base-url [provider] <url>
/config agents on|off | /config agents max <n>
/config network on|off
/config max-iterations <n> | /config max-retries <n>
/config timeout <duration>
/config log level <lvl> | /config log format text|json
/config web on|off | /config web port <n> | /config web listen <ip>
```

Fields exposed: provider, model, base URL, execution mode, agent enablement,
max agents, WebUI enablement, listen address, port, tool-loop limits, timeouts,
logging. `temperature` has no config field yet; adding a provider, routing and
credentials stay `config.yaml` edits.

A full-screen visual editor over the same fields is a possible future addition;
the direct commands already deliver the adjust-without-editing-YAML goal.

---

# 56. Default Config Example

```yaml
version: 1

provider: lemonade
model: ""

execution:
  mode: confirm
  command_timeout: 300s
  max_retries_per_command: 3

agents:
  enabled: true
  max: 5

web:
  enabled: false
  listen: 127.0.0.1
  port: 8585
  auth:
    enabled: false

providers:
  lemonade:
    type: lemonade
    base_url: http://127.0.0.1:13305

  lmstudio:
    type: lmstudio
    base_url: http://127.0.0.1:1234

  ollama:
    type: ollama
    base_url: http://127.0.0.1:11434

  openai:
    type: openai
    base_url: https://api.openai.com/v1
    api_key_env: OPENAI_API_KEY

  anthropic:
    type: anthropic
    api_key_env: ANTHROPIC_API_KEY

  xai:
    type: xai
    api_key_env: XAI_API_KEY
```

Provider defaults should remain configurable because server defaults may differ.

---

# 57. Failure Handling

All external/provider errors should be normalized.

Categories:

- unavailable,
- timeout,
- authentication,
- rate limited,
- invalid request,
- unsupported capability,
- malformed response,
- server error,
- cancelled.

The UI should show useful errors without dumping implementation internals unless debug mode is enabled.

---

# 58. Graceful Shutdown

Shutdown must:

1. cancel active model calls,
2. cancel or stop running commands where possible,
3. notify agents,
4. flush persistent events/session state,
5. save relevant project memory,
6. stop WebUI cleanly,
7. close database.

---

# 59. Development Milestones

Implement in this order unless a dependency requires adjustment.

---

## Milestone 0 — Repository Foundation

Deliver:

- Git initialized,
- `main` and `develop`,
- base Git Flow rules,
- Go module,
- Makefile,
- PROJECT.md,
- README.md,
- CHANGELOG.md,
- license placeholder/decision,
- basic CI skeleton,
- version package,
- `make help`,
- `make test`,
- `make build`.

Acceptance:

```bash
make test
make build
./boop version
```

work.

---

## Milestone 1 — Provider Core

Deliver:

- provider interface,
- OpenAI-compatible transport,
- capabilities,
- provider registry,
- provider health,
- list models,
- streaming abstraction,
- fake provider test server.

Then add:

- Lemonade,
- LM Studio,
- Ollama.

Acceptance:

Boop can connect to each configured local server, list models, and stream a basic chat response.

---

## Milestone 2 — Session Core and Plain CLI

Deliver:

- session representation,
- prompt handling,
- streaming output,
- config file,
- platform config paths,
- basic SQLite persistence,
- one-shot/plain CLI mode.

Acceptance:

```bash
boop --no-tui --provider ollama --prompt "hello"
```

works.

---

## Milestone 3 — TUI

Deliver:

- responsive layout,
- header,
- transcript,
- multiline input,
- footer,
- slash command parser,
- status,
- resize support,
- mouse support,
- clipboard/paste handling.

Acceptance:

Interactive local chat is stable across Linux/macOS/Windows terminals.

---

## Milestone 4 — Project Memory and `/prep`

Deliver:

- root detection,
- project discovery,
- Boop.md creation,
- test/build detection,
- context manager,
- `/prep`,
- `/tree`,
- `/context`.

Acceptance:

Running `/prep` in a repository creates useful project memory without modifying application source code.

---

## Milestone 5 — Tool Runtime

Deliver:

- registry,
- read,
- write,
- edit,
- list,
- search,
- find,
- run,
- execution results,
- cancellation,
- timeouts.

Acceptance:

A fake/tool-capable model can request a command and receive structured command results.

---

## Milestone 6 — Permission Engine

Deliver:

- confirm mode,
- auto mode,
- risk classification,
- approval events,
- TUI approvals,
- production-sensitive classification,
- deliberately unrestricted override.

Acceptance:

No protected operation bypasses the permission layer.

---

## Milestone 7 — Autonomous Repair Loop

Deliver:

- tool-call iteration,
- error feedback,
- retries,
- validation workflow,
- loop limits,
- `/test`,
- `/build`.

Acceptance:

Deterministic E2E test demonstrates:

```text
failing implementation
→ test fails
→ model receives error
→ model fixes issue
→ test passes
```

---

## Milestone 8 — Agents

Deliver:

- Agent type,
- scheduler,
- scoped context,
- dependencies,
- concurrency limit,
- agent statuses,
- `/agents` commands,
- TUI agent monitor.

Acceptance:

Two independent read/research tasks can run in parallel and return results to the parent agent.

---

## Milestone 9 — WebUI

Deliver:

- embedded WebUI,
- local HTTP server,
- port 8585,
- configurable bind address,
- WebSocket event stream,
- chat,
- agent status,
- approvals,
- config,
- session stats.

Acceptance:

A user on the permitted LAN can operate Boop through the browser when Boop is explicitly bound to a LAN interface.

---

## Milestone 10 — Cloud Providers

Deliver:

- OpenAI,
- Anthropic,
- xAI,
- provider-specific auth,
- token accounting,
- cost estimates where configured.

Acceptance:

Provider selection can switch between local and cloud providers without changes to the agent/tool runtime.

---

## Milestone 11 — Binary Documents and Multimodal

Deliver:

- MIME detection,
- image attachments,
- PDF extraction,
- DOCX extraction,
- capability routing.

Acceptance:

Text-only models receive extracted document text; multimodal models can receive supported image content.

---

## Milestone 12 — Routing and Fallback

Deliver:

- capability-based routing,
- task class routing,
- fallback providers,
- visible routing decisions.

Acceptance:

A task requiring vision automatically routes to a configured vision-capable model when enabled.

---

## Milestone 13 — Native GUI

Deliver:

- GUI launcher,
- shared core,
- shared event bus,
- agent monitor,
- approvals,
- settings.

Do not duplicate core business logic.

---

## Milestone 14 — Release Hardening

Deliver:

- cross-platform build matrix,
- release scripts,
- signed/checksummed artifacts where feasible,
- release branch procedure,
- changelog enforcement,
- package preparation,
- GitHub-ready release automation.

---

# 60. Development Procedure for Claude

When Claude is implementing this project, use the following workflow.

## Before coding

1. Read `PROJECT.md`.
2. Read `Boop.md` if present.
3. Inspect Git status.
4. Confirm current branch.
5. Inspect relevant existing code.
6. Identify the next incomplete milestone or requested task.
7. Create a feature branch when appropriate.

Example:

```bash
git switch develop
git switch -c feature/provider-interface
```

## During implementation

- make small cohesive changes,
- add tests with implementation,
- run formatting,
- run focused tests frequently,
- avoid unrelated refactors,
- keep public APIs small,
- update documentation when behavior changes.

## Before committing

Run relevant validation.

Usually:

```bash
make fmt
make vet
make test
make build
```

Use lint when configured.

## Commit

Use meaningful conventional-style commit messages.

## Feature completion

Before merging into `develop`:

- all tests pass,
- no debug leftovers,
- documentation updated,
- `Boop.md` updated with durable project decisions if relevant.

---

# 61. Claude Task Discipline

Claude should not:

- claim code works without running appropriate tests,
- invent API behavior,
- ignore failed commands,
- bypass the permission architecture to make implementation easier,
- put provider-specific behavior into shared UI code,
- store secrets in fixtures,
- make unrelated large rewrites,
- silently change release policy,
- commit directly to `main` for normal feature work.

Claude should:

- inspect,
- plan,
- implement,
- validate,
- record important decisions,
- preserve repository history.

---

# 62. Definition of Done

A feature is done only when:

- implementation is complete,
- tests exist where appropriate,
- relevant tests pass,
- build passes,
- behavior is documented if user-facing,
- errors are handled,
- platform assumptions are explicit,
- no secrets are introduced,
- code is formatted,
- Git history is meaningful.

---

# 63. Non-Goals for Early Versions

Do not prioritize the following ahead of the core runtime:

- plugin marketplace,
- public hosted Boop service,
- cloud account system,
- multi-user Internet-facing WebUI,
- complex IDE integrations,
- mobile app,
- Kubernetes operator,
- native GUI before WebUI/TUI maturity.

---

# 64. Architectural Invariants

These should remain true unless PROJECT.md is deliberately revised.

1. Boop core does not depend on a specific UI.
2. Tools go through a permission layer.
3. Providers are abstracted.
4. Agents use scoped context.
5. Project memory is human-readable in `Boop.md`.
6. Structured session data lives outside `Boop.md`.
7. Local development works without GitHub.
8. Releases are tagged in Git.
9. `main` represents releases.
10. normal development flows through `develop`.
11. builds remain reproducible and scriptable.
12. WebUI is local/LAN-oriented by default.
13. public exposure requires an explicit upstream proxy/security layer.
14. production changes require deliberate intent.
15. tests and validation are default behavior.

---

# 65. Initial Makefile Behavior

The initial Makefile should provide discoverability.

Example conceptual output:

```text
$ make help

Development:
  run                 Run Boop
  fmt                 Format code
  vet                 Run go vet
  lint                Run linter
  test                Run test suite
  coverage            Generate coverage report

Build:
  build               Build host binary
  build-all           Cross-build supported CLI/TUI binaries
  web-build           Build embedded WebUI
  dist                Create distributable archives
  clean               Remove generated files

Release:
  release-check       Verify release readiness
  snapshot            Create local snapshot artifacts
```

The Makefile may delegate complex logic to scripts.

Do not turn the Makefile into an unreadable shell program.

---

# 66. Release Workflow

Example v0.1.0 flow:

```bash
git switch develop
git pull --ff-only  # only once a remote exists

git switch -c release/0.1.0

# version updates
# changelog updates
# final fixes

make release-check
make dist

git commit -am "chore: prepare v0.1.0"

git switch main
git merge --no-ff release/0.1.0

git tag -a v0.1.0 -m "Boop v0.1.0"

git switch develop
git merge --no-ff release/0.1.0

git branch -d release/0.1.0
```

When GitHub is later configured:

```bash
git push origin main develop
git push origin v0.1.0
```

Release artifact publication may then be automated.

---

# 67. First Implementation Tasks

Claude should begin with this sequence:

1. initialize Git if required,
2. create `main`,
3. create `develop`,
4. ensure this `PROJECT.md` is committed,
5. create the Go module,
6. create the initial repository structure,
7. create a minimal `cmd/boop/main.go`,
8. implement `boop version`,
9. create Makefile,
10. add base unit test,
11. add `README.md`,
12. add `CHANGELOG.md`,
13. add `.gitignore`,
14. add CI skeleton,
15. run `make test`,
16. run `make build`,
17. commit foundation,
18. start `feature/provider-interface`.

Do not jump directly into agents or the GUI.

---

# 68. Suggested Initial Version

Start at:

```text
v0.1.0-dev
```

The first tagged public-style milestone should be:

```text
v0.1.0
```

when Milestones 0–7 are stable enough to make Boop genuinely useful as a local coding client.

---

# 69. Product Identity

Name:

```text
Boop
```

Executable:

```text
boop
```

Configuration namespace:

```text
boop
```

Project memory:

```text
Boop.md
```

Default WebUI port:

```text
8585
```

Suggested short description:

> Boop is a local-first, provider-agnostic AI terminal client and agent runtime for working with local and cloud language models.

---

# 70. Final Engineering Direction

The core investment should be:

1. reliable model/provider abstraction,
2. robust execution and tool semantics,
3. permission enforcement,
4. recoverable autonomous workflows,
5. context and project memory,
6. agent coordination,
7. testing,
8. observability.

The interface is important, but the core runtime is the product.

A polished chat UI with an unreliable agent engine is not sufficient.

A strong agent/tool core can support many interfaces later.

Build the engine first, then make every frontend a clean view into that engine.
