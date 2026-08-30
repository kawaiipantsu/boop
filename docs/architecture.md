# Architecture

This document describes how Boop is put together and how a prompt travels
through it. It is aimed at someone about to make their first change.

Boop is a local-first AI client and agent runtime. It is not a chat frontend
with tools bolted on: the core is an execution environment, and the interfaces
are views onto it.

`PROJECT.md` is the authoritative specification. Where this document cites a
section (`§7`, `§64`) it refers to that file.

## Status of this document

Boop is under active development and several subsystems are landing in
parallel. This document describes the parts that are complete and reachable,
and says plainly where something is still being built. If a statement here
disagrees with the code, the code is right — say so in an issue.

**Complete, tested, and reachable from the `boop` command:**

- `internal/config` — schema, defaults, validation, OS paths
- `internal/provider` — the neutral contract, capabilities, normalized errors,
  the registry and the router, plus seven adapters
- `internal/tools` — the schema-driven registry and thirteen tools
- `internal/execution` — the process executor and structured results
- `internal/permissions` — classifier, evaluator, approval broker
- `internal/session` + `internal/store` — SQLite-backed session state
- `internal/project` — root discovery, `Boop.md`, the `/prep` sequence
- `internal/stats`, `internal/webclient`, `internal/version`
- `internal/app` — the runtime assembly, the event bus and the tool loop
- `cmd/boop` — `boop version`, `boop status` and plain CLI mode (`boop --no-tui`)

**Under construction at the time of writing.** These packages exist in the tree
in varying states of completeness and are not yet wired into `dispatch` in
`cmd/boop/run.go`, so `boop`, `boop --web` and `boop --gui` still report which
milestone they belong to:

- **TUI** (`tui/`, §19, milestone 3)
- **WebUI** (`web/`, §22–§26, milestone 9) — the server, authenticator, origin
  policy and TypeScript frontend are substantially written; see
  [webui.md](webui.md)
- **Agent scheduler** (`internal/agent`, §10–§11, milestone 8) — the event bus
  already declares `agent.created` and `agent.status.changed`, and the store
  has agent tables
- **Documents / multimodal** (`internal/documents`, §27, milestone 11) —
  `provider.Message` already carries `Parts` with image and document kinds and
  the Anthropic adapter can send them
- **Structured logging** (`internal/logging`, §44) — `logging.level` is
  validated by config but not yet consumed

**Not started:**

- **Slash commands** (§20). `project.Prep` implements the `/prep` sequence as a
  library call, but no command dispatcher exists, so it is not reachable from
  the command line.
- **Native GUI** (milestone 13, deliberately last). `gui/` holds a README.

To see the real state of a subsystem, `go build ./...` is the honest answer.

## The shape of it

```
   cmd/boop            plain CLI  ·  TUI (wip)  ·  WebUI (wip)  ·  GUI (later)
        │
        ▼
   internal/app        App (assembly)  ·  Bus (events)  ·  Loop (one turn)
        │
        ├──────────────┬───────────────┬──────────────┬─────────────┐
        ▼              ▼               ▼              ▼             ▼
   provider.Router  tools.Registry  permissions   session/store   project
        │              │            .Evaluator         │             │
        ▼              ▼                │              ▼             ▼
   adapters       execution.Executor    │           SQLite       Boop.md
   (7 backends)   webclient             │
                                        ▼
                              permissions.Broker → frontend approver
```

`internal/app` is the only package that knows about all the others. If you are
adding a subsystem, wire it in there rather than reaching across packages.

## The UI-independent core

Everything that decides anything lives under `internal/`. The frontends decide
nothing; they render events and collect user input.

Two mechanisms make that hold:

1. **The event bus** (`internal/app/events.go`). A transport-neutral
   publish/subscribe hub. `Bus.Subscribe(handler, types...)` registers a
   handler; passing no types subscribes to everything. Events are plain
   structs with a JSON-serialisable payload, so the same event can be rendered
   in a terminal or pushed down a WebSocket without translation.

   ```go
   cancel := app.Bus.Subscribe(func(ev app.Event) {
       fmt.Println(ev.Type, ev.Payload)
   }, app.EventToolRequested, app.EventToolCompleted)
   defer cancel()
   ```

   `Publish` is synchronous and calls handlers in turn, so a handler must not
   block. The declared event types cover the session, the model stream, agents,
   tools, approvals, commands and tests.

2. **The `Approver` interface** (`internal/permissions/policy.go`). One method:

   ```go
   type Approver interface {
       Approve(action Action) (bool, error)
   }
   ```

   The core blocks on it when the permission engine says "confirm". The plain
   CLI implements it with a terminal prompt (`cmd/boop/cli.go`); the TUI and
   WebUI will implement it with their own UI. `permissions.Broker` is a
   multi-frontend implementation: the core calls `Request`, frontends call
   `Pending`/`Resolve`, and every attached frontend sees the same queue.

**Why:** §64.1. A core that depends on a UI has to be reimplemented for every
new UI, and the second implementation is always subtly different. Approvals in
particular must be identical everywhere (§50) — an approval dialog that shows
less than the terminal does is a security regression.

## The provider abstraction

`internal/provider/provider.go` declares the contract:

```go
type Provider interface {
    Name() string
    Health(ctx context.Context) error
    ListModels(ctx context.Context) ([]Model, error)
    Chat(ctx context.Context, req ChatRequest) (<-chan ChatEvent, error)
    Capabilities(ctx context.Context, model string) (Capabilities, error)
}
```

`Chat` returns a channel, so streaming is the base case rather than an
extension. An adapter that cannot stream must still emit a well-formed event
sequence ending in `EventDone`. The adapter owns the channel and closes it;
callers must drain it or cancel the context.

Vendor features that only some backends have go in **optional** interfaces —
`ModelLifecycleProvider` (load/unload) and `EmbeddingProvider` (embed) — which
callers reach with a type assertion. The base interface is never widened for
one vendor.

Capabilities (`internal/provider/capabilities.go`) are runtime metadata, not
assumptions: `streaming`, `tools`, `vision`, `reasoning`, `responses`,
`embeddings`, `structured_output`, `audio`. Errors are normalized into nine
categories (`unavailable`, `timeout`, `authentication`, `rate_limited`,
`invalid_request`, `unsupported_capability`, `malformed_response`,
`server_error`, `cancelled`) so the rest of Boop can react without knowing
which backend failed.

`internal/provider/router.go` sits above the registry. It resolves a
`Selection` (explicit provider/model, or a routing class, or the default class)
to a `Target`, caches health verdicts, and falls down the configured fallback
list on retryable failures. Every resolution produces a `Decision` recording
the class, the attempts and the reason, because §9 requires routing decisions
to be visible.

`internal/app/providers.go` is the **only** place that maps a config `type`
string onto a concrete adapter. See [providers.md](providers.md).

**Why:** §64.3. Once vendor knowledge leaks into the tool runtime or the UI,
adding a backend stops being a new package and becomes a survey of the whole
tree.

## The tool runtime

`internal/tools/registry.go` declares the tool contract:

```go
type Tool interface {
    Name() string
    Description() string
    Schema() map[string]any
    Permission(call Call) (permissions.Action, error)
    Execute(ctx context.Context, call Call) (Result, error)
}
```

`Permission` is consulted **before** `Execute`, which is what makes gating
possible at all: the tool describes what it would do without doing it.

`Registry.Definitions(allowed)` renders the registry as
`[]provider.ToolDefinition` for the model, optionally restricted to a named
subset — the hook the agent runtime will use for scoped tool access.

`BuildTools` in `internal/app/tools.go` registers:

| Tool | Category it reports | Notes |
|---|---|---|
| `read` | `filesystem.read` | offset/limit, binary files reported not dumped |
| `write` | `filesystem.write` | atomic |
| `edit` | `filesystem.write` | exact string replacement |
| `list` | `filesystem.read` | skips `.git`, `node_modules`, `vendor`, … |
| `find` | `filesystem.read` | name/glob |
| `search` | `filesystem.read` | RE2 content search |
| `run` | from the classifier | the central tool; see below |
| `git` | from an allowlist | 27 subcommands, no shell |
| `test` | from the classifier | detects the project's test command |
| `build` | from the classifier | detects the project's build command |
| `http` | `network.http` | only when `network.enabled` |
| `fetch` | `network.fetch` | only when `network.enabled` |
| `websearch` | `network.search` | only when `network.enabled` |

The three network tools are registered **only** when `network.enabled` is true,
so a model is never offered a tool that is configured to refuse. With the
default configuration a session has ten tools.

Two structural rules that will bite you if you miss them:

- **`Result` versus `error`.** Anything the model could plausibly repair — a
  missing file, a non-zero exit, an ambiguous edit, a denied action — returns
  `Result{IsError: true}` with an explanatory message. A Go `error` is reserved
  for input the tool cannot use at all. Returning an `error` where a `Result`
  belongs breaks the repair loop, and the repair loop is the product.
- **Filesystem confinement is a security boundary.** `tools.Workspace.Resolve`
  rejects any path outside the workspace root, and it does so *after* symlink
  resolution — for a path that does not exist yet it resolves the nearest
  existing ancestor, so creating a file through an escaping symlink is refused
  too.

The `run` tool (`internal/tools/run.go`) implements §13. Defaults: 300 s
timeout (from `execution.command_timeout`), 30 minute hard cap on a
model-requested timeout, 256 KiB captured per stream, 400 displayed lines per
stream with the middle elided. A non-zero exit is returned as data.

## The permission engine

`internal/permissions` is a security boundary. Prompt instructions are not a
security control; a model may request anything, and this package decides.

Three pieces:

- **Classifier** (`classifications.go`) — turns a command line or a path into a
  `Classification`: category, risk, production flag, human reason. It parses
  chaining, pipes, quoting, leading environment assignments, privilege
  escalators, `sh -c` nesting, `find -exec`, redirection, command substitution
  and the download-and-pipe-to-shell pattern.
- **Evaluator** (`evaluator.go`) — applies the `Policy` to an `Action` and
  returns `allow`, `confirm` or `deny`.
- **Broker** (`approvals.go`) — mediates a `confirm` outcome between the core
  and whichever frontends are attached, with optional "allow for session"
  grants that live in memory only.

Precedence, highest first: explicit `deny` → unauthorized production → critical
risk → unrestricted → confirm mode → auto rules. See
[permissions.md](permissions.md) for the full treatment.

## The two memories

They are deliberately separate and must not be collapsed into each other.

- **`Boop.md`** (`internal/project`) — human-readable, compressed durable
  project knowledge with a fixed section layout (§16). `/prep` writes only the
  blocks it owns, so hand-written prose survives a re-run. Raw transcripts
  never go here.
- **SQLite `boop.db`** (`internal/store`, `internal/session`) — machine state:
  session headers, messages, tool calls, executions, agent metadata, events,
  token usage. Everything is behind a `Store` interface; the driver is
  `modernc.org/sqlite`, which is pure Go so cross-compilation keeps working.

**Why:** §64.5 and §64.6. A human-readable memory that fills with transcript is
no longer readable, and a machine store that holds prose cannot be queried.

## How a prompt flows

Plain CLI mode (`cmd/boop/cli.go` → `internal/app/loop.go`) is the path that
works today. The TUI and WebUI will call the same `Loop`.

1. **Start up.** `run` parses flags. Remaining non-flag arguments become the
   prompt, so `boop fix the failing tests` works unquoted. `loadConfig` loads
   `config.yaml` (creating it with defaults on first run) and applies
   `--provider`, `--model`, `--mode`, `--log-level`.

2. **Assemble.** `app.New` validates the config, finds the project root
   (`project.FindRoot`, falling back to the process directory), builds the
   `Workspace`, builds every usable provider into a `Router`, builds the tool
   registry, opens the SQLite store, loads `Boop.md`, and constructs the
   `Evaluator` from `cfg.Policy()`. A provider that cannot be built is a
   *warning*, not a failure — a local-only user has no `OPENAI_API_KEY` and
   should not be told about it every run.

3. **Compose the system prompt.** `app.PromptContext.Render` appends the real
   environment to the embedded prompt: OS/arch, shell, working directory,
   provider and model, execution mode, the actual tool names registered, and
   whether outbound web access is on. `Boop.md` is appended whole if present.

4. **Run the turn.** `Loop.Run(ctx, history)` iterates up to
   `execution.max_tool_iterations` times:

   ```
   Router.Chat  →  stream of ChatEvent  →  collect()
        │                                     │
        │                              text? → done, return
        │                              tool calls? ↓
        │                            for each call: invoke()
        │                                     │
        └───────── tool messages appended ────┘
   ```

5. **`collect`** drains the stream, building the assistant message from
   `EventDelta`, emitting `EventReasoning` for thinking text, assembling tool
   calls from `EventToolCall`, and recording `EventUsage`. A stream that yields
   no events at all is a malformed-response error, not an empty answer — and
   cancellation is checked after the loop as well as inside it, because a
   stream with no events never enters the body.

6. **`invoke`** is the permission seam:

   ```
   registry.Get(name)      →  unknown tool? Result{IsError} listing what exists
   tool.Permission(call)   →  permissions.Action (category, risk, production)
   emit EventToolRequested
   Evaluator.Evaluate      →  deny?    Result{IsError: "denied: <reason>"}
                              confirm? Approver.Approve(action) → refused?
                                       Result{IsError: "the user declined…"}
                              allow?   fall through
   tool.Execute(ctx, call) →  Result
   emit EventToolCompleted
   ```

   Every outcome — including a denial — is a `Result` fed back to the model,
   not an abort. That is the error-repair loop: the model sees what happened
   and chooses another approach.

7. **Persist and print.** Every message in `turn.Messages` is appended to the
   session in SQLite. The final assistant text goes to stdout; progress,
   approvals and warnings go to stderr, so `boop --no-tui "…" > out.txt` stays
   useful.

If the loop hits `MaxIterations` with the model still asking for tools it sets
`StoppedAtLimit` and the CLI says so on stderr.

## Architectural invariants (§64)

These hold unless `PROJECT.md` is deliberately revised. Violating one is a spec
change, not an implementation detail.

1. **Boop core does not depend on a specific UI.** Otherwise every new frontend
   forks the logic, and the copies diverge.
2. **Tools go through a permission layer.** A tool that calls the OS directly
   is a hole in the only thing standing between a model and the machine.
3. **Providers are abstracted.** Vendor detail outside `internal/provider` turns
   "add a backend" into a whole-tree survey.
4. **Agents use scoped context.** Copying the main conversation into every
   worker wastes the context window and lets one agent act on another's
   half-finished reasoning.
5. **Project memory is human-readable in `Boop.md`.** A memory the user cannot
   read or correct is a memory they cannot trust.
6. **Structured session data lives outside `Boop.md`.** Transcript in a prose
   file destroys both.
7. **Local development works without GitHub.** CI is additive; a contributor
   with no network can still build and test.
8. **Releases are tagged in Git.** An untagged release cannot be reproduced or
   bisected.
9. **`main` represents releases.** It is the branch people build from.
10. **Normal development flows through `develop`.** It keeps `main` releasable
    at all times.
11. **Builds remain reproducible and scriptable.** `make` from a clean checkout
    must produce the artifacts; anything requiring a person's laptop is not a
    build.
12. **WebUI is local/LAN-oriented by default.** This interface can run shell
    commands. A default that listens on every interface would be a remote code
    execution service.
13. **Public exposure requires an explicit upstream proxy/security layer.**
    TLS, authentication and rate limiting belong to software built for it, not
    to a local agent runtime.
14. **Production changes require deliberate intent.** A model cannot know that
    a host is disposable; the cautious assumption is the only safe one.
15. **Tests and validation are default behaviour.** Unverified changes are
    guesses, and an agent that guesses confidently is worse than none.

## Making your first change

- Read the package doc comment first. Every package under `internal/` has one
  and they carry the reasoning, not just the summary.
- Run `make fmt vet test build` before you commit. That is the whole loop.
- Tests never need the network or a paid API. `tests/fixtures` has a scriptable
  fake provider server (OpenAI, Anthropic and Ollama-native shapes) and a
  deterministic fake `Provider` for driving the repair loop. Live-server tests
  are opt-in behind `BOOP_TEST_*` environment variables and skip when unset.
- New behaviour that a user can see needs documentation in this directory.

See [CONTRIBUTING.md](../CONTRIBUTING.md) for the full workflow.
