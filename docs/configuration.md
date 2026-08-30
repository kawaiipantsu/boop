# Configuration

Boop is configured by a single YAML file. Every key has a default, so the file
only ever needs to contain the values you actually want to change.

The schema lives in `internal/config/config.go`, the defaults in
`internal/config/defaults.go`, and the validation rules in
`internal/config/validate.go`. If this document and those files disagree, they
are right.

## Where the file lives

| OS | Config | Data (`boop.db`, `sessions/`) | Cache | Logs |
|---|---|---|---|---|
| Linux | `$XDG_CONFIG_HOME/boop` → `~/.config/boop` | `$XDG_DATA_HOME/boop` → `~/.local/share/boop` | `$XDG_CACHE_HOME/boop` → `~/.cache/boop` | `$XDG_STATE_HOME/boop/logs` → `~/.local/state/boop/logs` |
| macOS | `~/Library/Application Support/boop` | `~/Library/Application Support/boop` | `~/Library/Caches/boop` | `~/Library/Logs/boop` |
| Windows | `%AppData%\boop` | `%AppData%\boop` | `%LocalAppData%\boop` | `%LocalAppData%\boop\logs` |

The config file itself is `config.yaml` inside the config directory.

XDG variables are honoured, but only when absolute — the XDG specification
requires a relative value to be ignored. On Windows, roaming data goes under
`%AppData%` and machine-local data (cache, logs) under `%LocalAppData%`, since
caches should not roam.

Directories are created `0700` and `config.yaml` is written `0600`. Session
transcripts, the database and the logs can all contain the contents of private
source trees, so they are not readable by other users on a shared machine.

### `BOOP_CONFIG_DIR`

Setting `BOOP_CONFIG_DIR` relocates **everything** under one root and bypasses
the OS-native layout entirely:

```
$BOOP_CONFIG_DIR/config.yaml
$BOOP_CONFIG_DIR/boop.db
$BOOP_CONFIG_DIR/sessions/
$BOOP_CONFIG_DIR/cache/
$BOOP_CONFIG_DIR/logs/
```

It exists for two reasons: tests need a disposable tree that never touches your
real configuration, and portable installs (a USB stick, a checked-out dotfiles
repo, a container image) need everything in one place.

```bash
BOOP_CONFIG_DIR=/tmp/boop-test ./boop --no-tui "hello"
```

The value is resolved to an absolute path. A value that cannot be made absolute
is a startup error.

## Loading and layering

On first run, if no config file exists, Boop writes one containing the full
defaults. That gives you something to edit and makes the effective
configuration inspectable rather than implicit.

Keys present in the file win; absent keys keep their default. A config written
by an older version keeps working when new settings are added.

Provider entries layer field by field. Overriding only `openai.base_url` keeps
that entry's `type` and `api_key_env`.

Writes are atomic: a temporary file in the destination directory, synced and
renamed, so an interrupted write cannot leave a truncated config behind.

`Marshal` produces plain two-space-indented YAML with no comments — the file is
written by Boop as well as by hand, and `yaml.v3` cannot round-trip your
comments.

## Editing from the TUI

`/config` with no arguments prints the effective configuration; credential
values are never shown, only the environment-variable names.

`/config edit` opens a full-screen editor over the same fields: `↑`/`↓` move,
`←`/`→` or space toggle a boolean or cycle an enum, `Enter` edits a text value
(`Enter` commits, `Esc` cancels the edit), `Ctrl+S` saves, `Esc` closes without
saving. Each row is tagged `live` or `restart`; a `*` marks a field you changed.
Saving validates the whole draft, writes `config.yaml`, and applies the live
fields immediately (`App.ApplyConfig`), naming the groups that need a restart.
Credentials have no field in the editor.

`/config <field> <value>` changes one setting, writes it straight to
`config.yaml`, and — through `App.ApplyConfig` — applies whatever a running
process can honour immediately, naming the groups that need a restart. It
reloads the file before writing, so a per-invocation flag override (`--mode`,
`--provider`) is never frozen in, and a change made in another editor is not
clobbered. An invalid value is rejected and nothing is written.

| Command | Field | Takes effect |
|---|---|---|
| `/config mode auto\|confirm` | `execution.mode` | immediately — the evaluator re-reads it every decision |
| `/config provider <name>` | `provider` | immediately — must already be configured |
| `/config model <id>\|default` | `model` | immediately |
| `/config agents on\|off` | `agents.enabled` | immediately — the live fleet moves with it |
| `/config agents max <n>` | `agents.max` | immediately |
| `/config max-iterations <n>` | `execution.max_tool_iterations` | next message |
| `/config max-retries <n>` | `execution.max_retries_per_command` | next message |
| `/config base-url [provider] <url>` | `providers.<name>.base_url` | restart |
| `/config network on\|off` | `network.enabled` | restart — it gates which tools exist |
| `/config timeout <duration>` | `execution.command_timeout` | restart |
| `/config log level <lvl>` / `/config log format text\|json` | `logging.*` | restart |
| `/config web on\|off` | `web.enabled` | next `boop --web` |
| `/config web port <1-65535>` | `web.port` | next `boop --web` |
| `/config web listen <ip>` | `web.listen` | next `boop --web` |

Adding a provider, editing routing or fallback, and setting a credential
environment variable are still `config.yaml` edits (`api_key_env` names a
variable; a literal key is rejected by shape). `temperature` has no config field
yet.

## Validation

`Validate()` returns **errors** and **warnings** separately, and the split
matters. An error means Boop cannot safely act on this configuration and
startup stops. A warning means you have chosen something legal but risky and
deserve to be told, once, loudly.

Errors are joined, so a single run reports every problem instead of making you
fix them one at a time.

Notably, an unrecognised permission rule is an error rather than a fallback to
`confirm`: silently reinterpreting a permission setting is exactly the kind of
surprise the permission engine exists to prevent.

## The default file

This is what Boop writes on first run, with the values from
`config.Default()`.

```yaml
version: 1

provider: lemonade
model: ""

execution:
  mode: confirm
  command_timeout: 5m0s
  max_retries_per_command: 3
  max_tool_iterations: 50

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

network:
  enabled: false
  timeout: 30s
  max_response_bytes: 5242880
  max_redirects: 5
  allow_private_networks: false
  respect_robots: true
  search:
    provider: duckduckgo
    max_results: 10
    safe_search: moderate
    region: wt-wt

logging:
  level: info
  format: text
  max_size_mb: 10
  max_backups: 5
```

## Key reference

### Top level

| Key | Default | Valid values | Meaning |
|---|---|---|---|
| `version` | `1` | integer | Schema version. Bumped only alongside a migration |
| `provider` | `lemonade` | a key in `providers` | The active provider. Must exist and not be disabled |
| `model` | `""` | any model id | The active model. Empty means the provider's default |

### `execution`

| Key | Default | Valid values | Meaning |
|---|---|---|---|
| `execution.mode` | `confirm` | `confirm`, `auto` | Whether privileged actions prompt. See [permissions.md](permissions.md) |
| `execution.command_timeout` | `300s` | positive duration | Bounds a single tool command |
| `execution.max_retries_per_command` | `3` | integer | Bounds the error-repair loop for one command |
| `execution.max_tool_iterations` | `50` | integer | Bounds tool calls within a single turn, so a looping model cannot run indefinitely |

Durations accept a Go duration string (`300s`, `5m`, `1h30m`) or a bare integer
read as seconds. They are written back as Go duration strings.

> `max_retries_per_command` is carried in the config and validated but is not
> yet consumed by `app.Loop`; the loop is currently bounded by
> `max_tool_iterations` alone.

### `agents`

| Key | Default | Valid values | Meaning |
|---|---|---|---|
| `agents.enabled` | `true` | bool | Allow the scheduler to spawn workers |
| `agents.max` | `5` | integer | Maximum concurrent agents |

The agent scheduler is under construction; these keys are read but the runtime
is not yet wired into the CLI.

### `web`

Full treatment in [webui.md](webui.md).

| Key | Default | Valid values | Meaning |
|---|---|---|---|
| `web.enabled` | `false` | bool | Start the WebUI with the process |
| `web.listen` | `127.0.0.1` | an IP address | Bind address. Must parse as an IP |
| `web.port` | `8585` | 1–65535 | Listen port |
| `web.auth.enabled` | `false` | bool | Require an access token |
| `web.auth.token_env` | *(unset)* | env var name | **Name** of the variable holding the token |
| `web.allowed_origins` | `[]` | list of `scheme://host[:port]` | Origin allowlist. `*` is rejected |
| `web.trusted_proxy_headers` | `false` | bool | Honour `X-Forwarded-Proto`/`-Host`/`-For` |

Warnings you will see: a non-loopback bind with authentication off, a
non-loopback bind with no allowed origins, and `auth.enabled` with an empty
`token_env`. A non-loopback bind with authentication off is refused outright by
the server unless you override it deliberately.

### `providers`

A map. The key is the name you refer to the provider by; `type` selects the
adapter. Full treatment in [providers.md](providers.md).

| Key | Default | Valid values | Meaning |
|---|---|---|---|
| `type` | `openai-compatible` when empty | `lemonade`, `lmstudio`, `ollama`, `openai`, `anthropic`, `xai`, `openai-compatible` (aliases `openaicompat`, `generic`) | Which adapter to build |
| `base_url` | per adapter | URL | API root. Required for `openai-compatible` |
| `api_key_env` | per adapter | env var **name** | Names the variable holding the credential |
| `timeout` | adapter default (120s) | duration | Bounds non-streaming requests and the response-header wait |
| `headers` | *(none)* | map of string → string | Extra request headers |
| `disabled` | `false` | bool | Skip this entry entirely |

Default base URLs: Lemonade `http://127.0.0.1:13305`, LM Studio
`http://127.0.0.1:1234`, Ollama `http://127.0.0.1:11434`, OpenAI
`https://api.openai.com/v1`. Anthropic (`https://api.anthropic.com`) and xAI
(`https://api.x.ai/v1`) know their own endpoints and ship without one.

### `permissions`

Each key takes `allow`, `confirm` or `deny`. Anything else is a startup error.

| Key | Default |
|---|---|
| `permissions.filesystem.read` | `allow` |
| `permissions.filesystem.write` | `confirm` |
| `permissions.shell.execute` | `confirm` |
| `permissions.git.read` | `allow` |
| `permissions.git.commit` | `confirm` |
| `permissions.git.push` | `confirm` |
| `permissions.network.http` | `confirm` |
| `permissions.production.change` | `confirm` |

The `network.fetch` and `network.search` categories exist in the engine but
have no YAML key yet, so they always resolve to `confirm`. See
[permissions.md](permissions.md).

### `network`

Boop's **outbound** access to the public internet — fetching URLs and running
web searches. This is a different thing from `web`, which serves Boop's own
local interface.

| Key | Default | Valid values | Meaning |
|---|---|---|---|
| `network.enabled` | `false` | bool | Master switch. With it off, the `http`, `fetch` and `websearch` tools are not registered at all |
| `network.user_agent` | `""` | RFC 9110 User-Agent containing `boop` | Overrides the default. Validated; an override that drops "boop" is rejected |
| `network.timeout` | `30s` | positive duration | Bounds a single outbound request |
| `network.max_response_bytes` | `5242880` (5 MiB) | positive integer | Caps a fetched body |
| `network.max_redirects` | `5` | non-negative integer | Bounds a redirect chain |
| `network.allow_private_networks` | `false` | bool | Permit loopback, link-local and private ranges |
| `network.allowed_domains` | `[]` | list of domains | When non-empty, an allowlist: nothing else is fetched |
| `network.blocked_domains` | `[]` | list of domains | Always denied; takes precedence over the allowlist |
| `network.respect_robots` | `true` | bool | Honour `robots.txt` |
| `network.search.provider` | `duckduckgo` | `duckduckgo` | The only implemented backend; anything else is an error |
| `network.search.max_results` | `10` | non-negative integer | Result cap |
| `network.search.safe_search` | `moderate` | `off`, `moderate`, `strict` | |
| `network.search.region` | `wt-wt` | DuckDuckGo region code | `wt-wt` means no region |

Outbound access is off by default because reaching a third-party site sends
your text to servers you did not choose.

The default User-Agent identifies Boop so a site operator can attribute the
traffic and block it if they want:

```
Boop/0.1.0-dev (+https://github.com/kawaiipantsu/boop)
```

Two settings produce warnings when `network.enabled` is true:
`allow_private_networks` (a model-chosen URL can then reach loopback,
link-local and private addresses) and `respect_robots: false` (Boop will scrape
sites that asked not to be). Cloud metadata endpoints stay blocked even with
private networks allowed.

### `routing` and `fallback`

Optional. Both are validated against the `providers` map at load time, so a
typo is a startup error.

```yaml
routing:
  vision:
    provider: lmstudio
    model: qwen-vl
  reasoning:
    provider: openai
    model: gpt-5
  fast:
    provider: ollama
    model: llama3

fallback:
  - ollama
  - lmstudio
  - openai
```

Built-in classes are `default`, `vision`, `reasoning` and `fast`; you may
define others and select them by name. The active `provider`/`model` pair
becomes the `default` class automatically.

`fallback` is an ordered list of provider names tried when the selected
provider fails with a *retryable* error (`unavailable`, `timeout`,
`rate_limited`, `server_error`). Authentication failures and invalid requests
are not retried elsewhere.

### `logging`

| Key | Default | Valid values | Meaning |
|---|---|---|---|
| `logging.level` | `info` | `trace`, `debug`, `info`, `warn`, `error` | Verbosity |
| `logging.file` | *(unset)* | path | Overrides the platform default log location |
| `logging.format` | `text` | `text`, `json` | Text is for a person reading a log file; JSON is for anything that parses them |
| `logging.max_size_mb` | `10` | non-negative integer | Size at which the log file rotates, so a long-running runtime cannot fill a disk |
| `logging.max_backups` | `5` | non-negative integer | How many rotated files are kept |

`internal/logging` is being wired up at the time of writing; these keys are
validated but the CLI path does not yet emit through the logger.

## Command-line overrides

Applied on top of the loaded file, in `loadConfig`:

| Flag | Overrides |
|---|---|
| `--provider <name>` | `provider` |
| `--model <id>` | `model` |
| `--mode confirm\|auto` | `execution.mode` |
| `--log-level <level>` | `logging.level` |
| `--listen <ip>` | `web.listen` (passed to the WebUI server) |
| `--port <n>` | `web.port` (passed to the WebUI server) |

Other flags select behaviour rather than overriding config: `--no-tui`,
`--web`, `--gui`, `--prompt`, `--verbose`/`-v`, `--version`,
`--allow-insecure-bind`, `--dangerously-unrestricted`.

> `--dangerously-unrestricted` is accepted but does not currently set
> `Policy.Unrestricted`, so today it has no effect. The evaluator honours the
> field correctly; the CLI does not yet populate it.

## Credentials are never literals

This is a hard rule (§45). The config file names an environment variable; the
value is read at use time.

```yaml
providers:
  openai:
    api_key_env: OPENAI_API_KEY
```

```bash
export OPENAI_API_KEY=sk-…
```

Validation rejects a literal key pasted into `api_key_env`, detected by shape:

- a known credential prefix (`sk-`, `sk_`, `pk-`, `xai-`, `ghp_`, `gho_`,
  `ghs_`, `github_pat_`, `Bearer `),
- a value that is not a valid environment-variable name
  (`^[A-Za-z_][A-Za-z0-9_]*$`),
- or a value longer than 64 characters.

The same prefix check applies to custom `headers` values. The offending string
is never echoed back in the error message — an over-eager heuristic that leaked
the key into an error would be worse than the mistake it detects.

The same rule applies to the WebUI token: `web.auth.token_env` names a
variable, and the token is kept in memory only as a SHA-256 digest.

Keys are redacted from logs, errors, transcripts and the WebUI.

## Environment variables

| Variable | Purpose |
|---|---|
| `BOOP_CONFIG_DIR` | Relocate every Boop directory under one root |
| `BOOP_WEB_ALLOW_INSECURE` | Skip the refusal to bind the WebUI beyond loopback without authentication |
| *(any name you choose)* | Provider credentials, named by `api_key_env` |
| *(any name you choose)* | WebUI access token, named by `web.auth.token_env` |

For development and testing only, all opt-in and skipping when unset:

| Variable | Purpose |
|---|---|
| `BOOP_TEST_OPENAI_COMPAT_URL` | Run the generic OpenAI-compatible live tests |
| `BOOP_TEST_OLLAMA_URL` | Run the Ollama live tests |
| `BOOP_TEST_LMSTUDIO_URL` | Run the LM Studio live tests |
| `BOOP_TEST_LEMONADE_URL` | Run the Lemonade live tests |
| `BOOP_TEST_MODEL` | Model id for live provider tests |
| `BOOP_TEST_NOTOOLS_MODEL` | A completion-only model, for the capability tests |
| `BOOP_TEST_API_KEY` | Credential for a live endpoint that needs one |
| `BOOP_NETWORK_TESTS` | Run the webclient tests that touch the real internet |
| `SOURCE_DATE_EPOCH` | Omit the build date, for reproducible builds |

## A worked example

A local-first setup: Ollama as the default, Anthropic available for reasoning,
git commits allowed unattended but pushes never, and web search on.

```yaml
version: 1

provider: ollama
model: qwen2.5:7b

execution:
  mode: auto
  command_timeout: 10m
  max_tool_iterations: 80

providers:
  ollama:
    type: ollama
    base_url: http://127.0.0.1:11434
  anthropic:
    type: anthropic
    api_key_env: ANTHROPIC_API_KEY

routing:
  reasoning:
    provider: anthropic
    model: claude-sonnet-4-5

fallback:
  - ollama

permissions:
  filesystem:
    read: allow
    write: confirm
  shell:
    execute: confirm
  git:
    read: allow
    commit: allow
    push: deny
  network:
    http: confirm
  production:
    change: confirm

network:
  enabled: true
  respect_robots: true
  allowed_domains:
    - pkg.go.dev
    - go.dev

logging:
  level: debug
```

`git.push: deny` outranks every mode and every flag, so this machine will not
push no matter what the model asks for.
