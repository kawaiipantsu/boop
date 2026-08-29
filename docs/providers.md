# Providers

Boop talks to model backends through one interface. Nothing outside
`internal/provider` knows which backend is running.

Seven adapters exist today. Three are local servers, three are cloud vendors,
and one is the generic OpenAI-compatible base the others are built on.

## Verification status

Read this before you trust a capability table.

| Adapter | Verified against a real server? |
|---|---|
| `openaicompat` | Yes — the dialect is standard and the package has a full test suite against the fake server; the live test runs against any OpenAI-compatible endpoint you point it at |
| `ollama` | **Yes.** Every endpoint and response shape was verified against a live Ollama 0.31.2 server |
| `lemonade` | **No.** The OpenAI-compatible path is standard; the native health/load/unload endpoints are **inferred** from documentation and were never exercised |
| `lmstudio` | **No.** The OpenAI-compatible path is standard; the `/api/v0` REST surface is **inferred** from documentation and was never exercised |
| `openai` | Wire format is the standard dialect; the adapter adds capability refinement and header handling |
| `anthropic` | Native Messages API implementation, covered by unit tests against recorded shapes |
| `xai` | Standard OpenAI dialect plus a Grok capability table |

The Lemonade and LM Studio packages say so in their own doc comments, and every
inferred constant is marked `INFERRED` at its declaration. Both adapters are
built so that an inferred endpoint failing is survivable: health falls back to
the model listing, and native model listing falls back to the OpenAI listing.
That is a mitigation, not a substitute for verification.

If you have one of these servers, running the live tests produces a correction
list. They report what they find rather than asserting what they expected:

```bash
BOOP_TEST_LEMONADE_URL=http://127.0.0.1:13305 go test ./internal/provider/lemonade/ -run Live -v
BOOP_TEST_LMSTUDIO_URL=http://127.0.0.1:1234  go test ./internal/provider/lmstudio/ -run Live -v
BOOP_TEST_OLLAMA_URL=http://127.0.0.1:11434   go test ./internal/provider/ollama/   -run Live -v
```

## Configuring a provider

Every provider is an entry in the `providers` map in `config.yaml`. The map key
is the name you refer to it by (in `provider:`, in `--provider`, in routing and
fallback); `type` selects the adapter.

```yaml
provider: ollama          # which entry is active
model: qwen2.5:7b         # empty means the provider default

providers:
  ollama:
    type: ollama
    base_url: http://127.0.0.1:11434
```

Fields, all from `config.ProviderConfig`:

| Field | Meaning |
|---|---|
| `type` | Adapter: `lemonade`, `lmstudio`, `ollama`, `openai`, `anthropic`, `xai`, or `openai-compatible` (aliases: `openaicompat`, `generic`, or omitted) |
| `base_url` | API root. Required for `openai-compatible`; optional elsewhere |
| `api_key_env` | **Name of an environment variable** holding the credential. Never the credential itself |
| `timeout` | Bounds non-streaming requests and the response-header wait of streaming ones. Generation time is never bounded by it |
| `headers` | Extra request headers, for installations behind an authenticating proxy |
| `disabled` | Skip this entry entirely |

The map key and `type` are independent, so you can register two of the same
backend:

```yaml
providers:
  ollama-laptop:
    type: ollama
    base_url: http://127.0.0.1:11434
  ollama-workstation:
    type: ollama
    base_url: http://192.168.1.50:11434
```

Errors and statistics carry the map key, so you can tell them apart.

## The adapters

### Lemonade (local)

```yaml
lemonade:
  type: lemonade
  base_url: http://127.0.0.1:13305
```

- Default base URL: `http://127.0.0.1:13305`
- Credential: none. `api_key_env` exists only for an authenticating proxy in
  front of it.
- OpenAI-compatible surface lives under `/api/v1`, so the effective OpenAI base
  is `<root>/api/v1`. The adapter normalizes a base URL you give with or
  without that suffix.
- Capabilities: streaming, tools, embeddings, plus load/unload
  (`ModelLifecycleProvider`).
- **Inferred:** `POST /api/v1/load`, `POST /api/v1/unload`, `GET
  /api/v1/health`, and their request/response bodies. A 404 from any of them is
  treated as "this build does not have it". Load/unload try a second body shape
  before giving up with a normalized error naming the endpoint attempted.

### LM Studio (local)

```yaml
lmstudio:
  type: lmstudio
  base_url: http://127.0.0.1:1234
```

- Default base URL: `http://127.0.0.1:1234`
- Credential: none.
- OpenAI dialect under `/v1`. Native REST surface at `/api/v0/models`, used for
  per-model state (in memory or not), model type and context length — things
  the OpenAI listing cannot express.
- Capabilities: streaming, tools, embeddings, **load only**. `UnloadModel` is
  not available through the documented REST API, so the lifecycle support is
  partial.
- **Inferred:** the whole `/api/v0` surface, its state vocabulary and its field
  names. Fields are read leniently and a 404 falls back to the OpenAI listing.

### Ollama (local)

```yaml
ollama:
  type: ollama
  base_url: http://127.0.0.1:11434
```

- Default base URL: `http://127.0.0.1:11434` (loopback rather than `localhost`,
  so a broken IPv6 stack cannot stall the first request)
- Credential: none.
- OpenAI dialect under `/v1`, plus four native endpoints: `GET /api/tags` (the
  authoritative per-model capability list and real context window), `POST
  /api/show`, `GET /api/version` (health probe), and `POST /api/generate` with
  `keep_alive` for load/unload.
- Capabilities: streaming, tools, embeddings, load and unload.
- Because `/api/tags` reports capabilities authoritatively, it **replaces**
  name-based heuristics. Guessing that `qwen:7b` supports tools earns an HTTP
  400 from the server; asking first does not.

Two real Ollama quirks the adapter handles, and the tests pin: the final usage
SSE frame carries `"choices":[]` (indexing `Choices[0]` panics), and tool calls
arrive complete in a single delta rather than fragmented across deltas as
OpenAI sends them.

### OpenAI (cloud)

```yaml
openai:
  type: openai
  base_url: https://api.openai.com/v1
  api_key_env: OPENAI_API_KEY
```

- Default base URL: `https://api.openai.com/v1`
- Credential: **required**, via `api_key_env` (default `OPENAI_API_KEY`), sent
  as a bearer token.
- Thin layer over `openaicompat`: capability refinement for model families the
  generic name heuristics get wrong, the `OpenAI-Organization` and
  `OpenAI-Project` headers (set them via `headers`), and a correction to the
  normalized error category for the one OpenAI failure whose HTTP status
  misreports it.
- `base_url` is overridable so an Azure-style or gateway deployment can reuse
  the adapter.

### Anthropic (cloud)

```yaml
anthropic:
  type: anthropic
  api_key_env: ANTHROPIC_API_KEY
```

- Default base URL: `https://api.anthropic.com`. Note there is no `/v1` in the
  base: the version segment is part of the paths (`/v1/messages`,
  `/v1/models`).
- Credential: **required**, sent as the `x-api-key` header.
- `anthropic-version: 2023-06-01` is sent on every request.
- This is the one adapter that does **not** build on `openaicompat`. The
  Messages API is a different dialect: the system prompt is a top-level field,
  `max_tokens` is required, tools declare an `input_schema`, tool calls and
  results are content blocks, and streaming is a typed event stream rather than
  choice deltas. A translation layer would be larger and more fragile than a
  native implementation.
- Because `max_tokens` is required, the adapter sends `4096` when
  `ChatRequest.MaxTokens` is zero — a value every published Claude model
  accepts. Set `Options.DefaultMaxTokens` or `ChatRequest.MaxTokens` for the
  larger ceilings of current models.
- Capabilities: streaming, tools, vision. No embeddings endpoint.

### xAI / Grok (cloud)

```yaml
xai:
  type: xai
  api_key_env: XAI_API_KEY
```

- Default base URL: `https://api.x.ai/v1`
- Credential: **required**, bearer token.
- xAI serves the OpenAI dialect including the error envelope and SSE chunk
  format, so this adapter is a configuration of `openaicompat` plus one
  genuinely vendor-specific thing: which Grok model can do what.

### Generic OpenAI-compatible

```yaml
myserver:
  type: openai-compatible
  base_url: http://192.168.1.20:8080/v1
  api_key_env: MYSERVER_TOKEN     # optional
```

- `base_url` is **required**; there is no sensible default.
- Credential optional. When set it is sent as a bearer token.
- Default paths: `/models`, `/chat/completions`, `/embeddings`, all relative to
  `base_url`.
- Use this for llama.cpp's server, vLLM, LocalAI, text-generation-webui, a
  corporate gateway, or anything else that speaks the dialect.

## Credentials

Credentials are named, never written. The config file holds the *name* of an
environment variable:

```yaml
providers:
  openai:
    api_key_env: OPENAI_API_KEY
```

```bash
export OPENAI_API_KEY=sk-…
boop --no-tui --provider openai "hello"
```

Config validation rejects a literal key pasted into `api_key_env` by shape: a
value with a known credential prefix (`sk-`, `xai-`, `ghp_`, `Bearer `, …), a
value that is not a valid environment-variable name, or a value longer than 64
characters. It also rejects a credential pasted into a custom `headers` value.
The offending string is never echoed in the error.

A missing cloud credential is a **warning**, not a startup failure — the
provider is skipped and the reason is recorded in `App.Warnings`. Run with
`--verbose` to see them. A local-only user should not be blocked by a key they
do not have.

Local providers (`lemonade`, `lmstudio`, `ollama`) never require a credential.

## Capabilities

Boop never assumes a capability. `Capabilities(ctx, model)` returns a set drawn
from: `streaming`, `tools`, `vision`, `reasoning`, `responses`, `embeddings`,
`structured_output`, `audio`.

Where the server reports capabilities (Ollama's `/api/tags`), that is
authoritative and replaces heuristics. Where it does not, the adapter derives a
set from the model id and its own family table.

When a task needs something the routed model lacks, the router returns a
`CapabilityRoutingError` naming the missing capabilities *and* the configured
targets that do have them, rather than letting the request fail as an opaque
400.

## Routing and fallback

```yaml
routing:
  vision:
    provider: lmstudio
    model: qwen-vl
  reasoning:
    provider: openai
    model: gpt-5

fallback:
  - ollama
  - lmstudio
  - openai
```

The active `provider`/`model` pair becomes the `default` routing class
automatically. Routing classes and fallback entries are validated at load time
against the `providers` map, so a typo is a startup error rather than a runtime
surprise.

The router retries down the fallback list only on *retryable* normalized errors
(`unavailable`, `timeout`, `rate_limited`, `server_error`). An authentication
failure or an invalid request is not retried against a different backend, and
`Selection.NoFallback` pins a selection entirely.

Health verdicts are cached: 10 s for healthy, 30 s for unhealthy. The longer
unhealthy TTL exists because the common case is a local server that is simply
not running, and re-dialling it on every call is wasted latency.

## Adding a provider

1. **Create the package** under `internal/provider/<name>/`.

2. **Decide whether it speaks the OpenAI dialect.** If it does — and most local
   servers do — embed `*openaicompat.Client` and override only what is
   genuinely different:

   ```go
   package myvendor

   import (
       "github.com/kawaiipantsu/boop/internal/provider"
       "github.com/kawaiipantsu/boop/internal/provider/openaicompat"
   )

   const (
       ProviderName   = "myvendor"
       DefaultBaseURL = "http://127.0.0.1:9000"
   )

   type Client struct{ *openaicompat.Client }

   var _ provider.Provider = (*Client)(nil)

   func New(baseURL string, opts ...Option) *Client {
       if baseURL == "" {
           baseURL = DefaultBaseURL
       }
       return &Client{Client: openaicompat.New(openaicompat.Options{
           Name:               ProviderName,
           BaseURL:            baseURL,
           RefineCapabilities: refineCapabilities,
       })}
   }
   ```

   `openaicompat.Options` gives you `RefineCapabilities` and `RefineModels`
   hooks, and `GetJSON`/`PostJSON` escape hatches for native endpoints, so you
   should not need to reimplement HTTP, SSE parsing, error normalization or
   redaction.

   If it does not speak the dialect, implement `provider.Provider` directly and
   look at `internal/provider/anthropic` for the shape.

3. **Implement the five methods.** `Chat` must return a channel it owns and
   closes, terminating in exactly one `EventDone` or `EventError`, and must not
   block on an abandoned channel after the context is cancelled.

4. **Normalize errors** into `provider.ErrorCategory`. Do not leak transport
   detail into `Message`; put it in `Detail`, which only surfaces in debug mode.

5. **Add optional interfaces if they apply**, never by widening `Provider`:

   ```go
   type ModelLifecycleProvider interface {
       LoadModel(ctx context.Context, model string) error
       UnloadModel(ctx context.Context, model string) error
   }

   type EmbeddingProvider interface {
       Embed(ctx context.Context, req EmbeddingRequest) (EmbeddingResponse, error)
   }
   ```

   Assert them at compile time: `var _ provider.EmbeddingProvider = (*Client)(nil)`.

6. **Register the type string** in `BuildProvider` in
   `internal/app/providers.go`. That function is the only place a type string
   becomes a concrete adapter; add the case and, if the backend needs a
   credential, add it to `requiresKey`.

7. **Add a default entry** to `DefaultProviders()` in
   `internal/config/defaults.go` if the backend is common enough to ship
   pre-configured.

8. **Test against the fake server**, not a live one. `tests/fixtures` has a
   scriptable HTTP server covering the OpenAI, Anthropic and Ollama-native
   shapes, plus streaming, tool calls, malformed responses, HTTP failures and
   delays. Add a `live_test.go` guarded by an environment variable if a real
   server is useful, and make it skip when unset.

9. **Document the verification status** in the package doc comment, and mark
   every constant you inferred rather than confirmed. Then add the adapter to
   the table at the top of this file.

## Troubleshooting

### connection refused

By far the most common failure. The local server is not running, or it is not
running where Boop is looking.

```
boop: provider "ollama" unavailable: … connection refused
```

Check the server is up and on the port you configured:

```bash
curl http://127.0.0.1:11434/api/version      # Ollama
curl http://127.0.0.1:1234/v1/models         # LM Studio
curl http://127.0.0.1:13305/api/v1/models    # Lemonade
```

If the server listens on a different port, set `base_url` in the provider
entry. If it listens only on `localhost` as an IPv6 name and your stack is
broken, prefer `127.0.0.1`.

Note that a provider that cannot be reached at startup is skipped with a
warning, not a fatal error — until it is the *active* provider, in which case
Boop refuses to start with "active provider … is not available".

### `no usable providers configured`

Every entry in `providers` failed to build. Usually this means each one is
`disabled: true`, or every entry has an unknown `type`, or the cloud entries
are all missing credentials and there are no local entries.

### `active provider "x" is not available`

`provider: x` names an entry that could not be built. Run with `--verbose` to
see the specific reason for `x`.

### `api key environment variable is not set`

`api_key_env` names a variable that is unset or empty. Export it in the same
shell that runs `boop`; a variable exported in another terminal does not count.

### 401 / 403 from a cloud provider

The key is set but wrong, revoked, or belongs to a different organization. For
OpenAI, an account in more than one organization must send
`OpenAI-Organization` or requests are billed to and rate-limited by the default
organization — set it via `headers`.

### HTTP 400 asking for tools

The model does not support tool calling. Boop asks for capabilities first where
the server reports them, so this usually means an OpenAI-compatible server that
does not report capabilities and a model whose name did not match the
heuristics. Pick a tool-capable model, or file the model id as a heuristic gap.

### The stream ends immediately

`the provider produced no events` or `the stream ended without a completion`
means the adapter got a response the model runtime could not use. Check the
server log; a proxy that buffers SSE will do this.

### Timeouts on a slow local model

`timeout` bounds non-streaming requests and the wait for response *headers*.
Generation time is deliberately never bounded by it — a slow local model is not
a failure. If you are timing out, the server is not responding at all, not
responding slowly.

### Everything works over `curl` but not from Boop

Check `base_url` includes the path prefix the server expects. Boop normalizes
the well-known suffixes (`/v1` for LM Studio and Ollama, `/api/v1` for
Lemonade) but a custom gateway path must be spelled out in `base_url`.
