# Security Policy

Boop executes shell commands, reads and writes files, makes outbound network
requests, and holds references to API credentials. A defect in it is not a
cosmetic bug. Please treat this file as more than boilerplate — we do.

## Reporting a vulnerability

**Do not open a public issue for a security problem.**

Report it privately through GitHub's private vulnerability reporting:

1. Go to <https://github.com/kawaiipantsu/boop/security/advisories/new>
2. Describe the issue, the impact, and how to reproduce it.

If private reporting is unavailable, open a public issue that says only *"I
have a security report, please provide a private channel"* — no detail — and
wait for a maintainer to respond.

> **Maintainer:** a security contact email has not been set for this project.
> If you prefer email, add it here and to `CODE_OF_CONDUCT.md`.

### What to include

- Boop version (`boop version` output) and how it was built or installed.
- Operating system and architecture.
- The configuration that reproduces it, **with secrets removed** — replace
  values, do not truncate keys.
- Exact steps to reproduce, and the smallest input that triggers it.
- What an attacker gains, and what access they need to start.
- A proof of concept if you have one.

### What to expect

Boop is a young project maintained by volunteers, so we will not promise a
response time we cannot keep. We will acknowledge your report, tell you whether
we consider it in scope, and keep you informed while we work on it. Fixes ship
in a release with a `Security` section in `CHANGELOG.md`.

We will credit you in the advisory and the changelog unless you ask us not to.

Please give us reasonable time to ship a fix before disclosing publicly. If a
report goes unanswered for a month, disclose — an unmaintained security bug is
worse than a public one.

## Supported versions

Boop is pre-1.0 and under active development. Only the latest release and the
`develop` branch are supported. There are no backports to earlier tags.

## What is in scope

- Bypassing the permission engine: getting a tool to execute without a
  correctly classified `permissions.Action`, or getting an `allow` where the
  documented precedence says `confirm` or `deny`.
- Escaping the workspace boundary: reading or writing outside the project root
  through the filesystem tools, including via symlinks.
- Bypassing the production gate.
- Leaking credentials: an API key or WebUI token appearing in a log, an error
  message, a transcript, the WebUI, `Boop.md`, a crash report or a fixture.
- WebUI authentication or origin-validation bypass; unauthenticated access to
  an endpoint that should require a token; CSRF against a loopback-bound
  instance.
- SSRF through the `http`, `fetch` or `websearch` tools — in particular
  reaching loopback, private ranges or cloud metadata endpoints when
  `allow_private_networks` is false.
- Path traversal, argument injection into the `git` tool, or any way to make a
  tool run a command the classifier never saw.
- Denial of service that a remote input can trigger: unbounded memory from a
  hostile provider response, a fetched page, or a tool result.
- Dependency vulnerabilities that Boop actually reaches. `make security` runs
  `govulncheck`.

## What is out of scope

- **A model asking Boop to do something dangerous.** That is the permission
  engine's job, and a prompt that makes the model *request* `rm -rf /` is
  working as designed — the interesting question is whether it ran without
  approval.
- **A user approving a destructive action.** Boop asked; they said yes.
- **`--dangerously-unrestricted` doing what its name says**, except where it
  bypasses the production gate, which it must never do.
- **`BOOP_WEB_ALLOW_INSECURE=1` or `--allow-insecure-bind` exposing the
  WebUI.** Those exist to let an operator accept that risk on purpose.
- **A configuration that explicitly permits something.**
  `permissions.shell.execute: allow` means shell commands run.
- Vulnerabilities in Ollama, LM Studio, Lemonade, or a cloud provider's API —
  report those to their maintainers. Boop's handling of a *hostile response*
  from one of them is in scope.
- Attacks requiring an attacker who already has your user account, your config
  file, or your shell.
- Missing security headers on a loopback-bound development interface, absent a
  concrete exploit.

## The security model

### The permission engine is the boundary

`internal/permissions` decides what runs. Prompt instructions are not a
security control: a model may request anything, and this package decides.

Every tool reports a `permissions.Action` from `Permission(call)` *before*
`Execute(ctx, call)` runs. The evaluator applies precedence — explicit `deny` →
unauthorized production → critical risk → unrestricted → confirm mode → auto
rules — and a `confirm` outcome blocks until a human answers.

Session grants ("always for session") live in memory only. They are never
written to disk and never survive a restart, and they are refused entirely for
production-affecting and critical-risk actions. Full detail in
[docs/permissions.md](docs/permissions.md).

### Production requires deliberate intent

Production work confirms even in `auto` mode and even with
`--dangerously-unrestricted`. Only an explicit
`Evaluator.AuthorizeProduction(true)` lifts the gate, and that is deliberately
not a side effect of changing mode or of approving any single action.

Boop cannot know that a host is disposable, that a Kubernetes context is a test
cluster, or that a Terraform state describes a sandbox. The cautious assumption
is the only safe one.

### Workspace confinement

`tools.Workspace.Resolve` rejects any path outside the project root, and it
does so **after** symlink resolution. For a path that does not yet exist, the
nearest existing ancestor is resolved instead, so creating a file through an
escaping symlink is refused too.

The `git` tool passes arguments to `git` directly rather than through a shell,
and refuses any subcommand not on its allowlist.

### SSRF defence

Outbound web access (`network.enabled`) is off by default. When on,
`internal/webclient.Guard` enforces:

- **Scheme allowlist** — `http` and `https` only.
- **Domain policy** — `blocked_domains` always wins; a non-empty
  `allowed_domains` denies everything not listed.
- **Address policy** — loopback, link-local, private, unspecified, multicast
  and several reserved ranges (CGNAT, TEST-NET, benchmarking, NAT64, IPv6
  documentation) are refused unless `allow_private_networks` is set.
- **Cloud metadata endpoints are refused unconditionally**, even with private
  networks allowed: `169.254.169.254`, `169.254.170.2`, `100.100.100.200`,
  `192.0.0.192`, `fd00:ec2::254`. They hand out credentials to anything that
  can reach them.

The check runs three times: on the URL after resolution, in the dialer's
`Control` hook against the address actually being connected to, and again on
every redirect hop. `HTTP_PROXY` is ignored as a bypass vector. Response bodies
are capped (5 MiB by default) and redirect chains are bounded.

**Residual risk, stated honestly:** a DNS-rebinding race remains theoretically
possible if a resolver answer changes between the `Control`-hook check and the
kernel's use of that address. Fully closing it needs connection-level pinning
of the checked IP, which Go's `http.Transport` does not expose. The window is
small, but it is real.

### Secret handling

Credentials are **named, never stored**. The config file holds the name of an
environment variable (`api_key_env`, `web.auth.token_env`); the value is read
at use time.

Config validation rejects a literal key pasted where a variable name belongs,
detected by shape — a known credential prefix, an invalid environment-variable
name, or a value over 64 characters — and the same prefix check applies to
custom provider headers. The offending string is never echoed back in the
error: an over-eager heuristic that leaked the key into a message would be
worse than the mistake it detects.

The WebUI token is kept in memory only as a SHA-256 digest, and comparison is
over digests, which is constant time in both value and length.

Keys are redacted from logs, errors, transcripts and the WebUI. Redaction is
not left to pattern matching alone: when a credential is resolved from the
environment its exact value is registered with the log redactor
(`logging.RegisterSecret`), because a self-hosted gateway key can be an
ordinary-looking word that no shape heuristic would ever catch.

`config.yaml` is written `0600` and every Boop directory is created `0700`,
because session transcripts and the database can contain the contents of
private source trees.

Never put a secret in a test fixture, and never in `Boop.md`.

### WebUI posture

The WebUI can run shell commands, so the default is `127.0.0.1:8585`.

Binding beyond loopback with authentication disabled is **refused at startup**,
not warned about. Wildcard origins are rejected outright. `Origin: null` is
always refused. `X-Forwarded-*` headers are ignored unless
`trusted_proxy_headers` is explicitly set. Public exposure is expected to go
behind a reverse proxy that owns TLS and authentication.

Full detail in [docs/webui.md](docs/webui.md).

### Supply chain

`CGO_ENABLED=0` and `-trimpath` on every build. Dependencies are pinned through
Go modules and kept few and mature. `make security` runs `govulncheck`, and CI
runs it on every push and pull request. Release archives ship with SHA-256
checksums.

## Known limits

Stating these plainly is more useful than implying a guarantee that does not
exist.

### Command classification is defence in depth, not a sandbox

`permissions.ClassifyCommand` reads a command line as a string. It parses
chaining, pipes, quoting, environment assignments, privilege escalators, `sh
-c` nesting, redirection and the download-and-pipe-to-shell pattern — and it is
good at those. It cannot see through:

- shell functions and aliases defined earlier in the session,
- variable expansion (`$CMD`, `rm -rf $DIR`),
- base64 or hex decoding,
- arbitrary interpreter payloads (`python -c`, `perl -e`),
- programs that are themselves shells,
- a binary that simply does something other than its name suggests,
- a script in the workspace (`./deploy.sh` is `shell.execute` at medium risk;
  the classifier has no idea what is inside it).

Anything it cannot analyse is **escalated rather than trusted**: unknown
programs are medium, command substitution is high, deep nesting is high, an
unparseable line is medium.

The real containment guarantees come from the permission decision (a human
looking at the command), the workspace boundary, the git allowlist, and the
operating system. Do not treat `auto` mode with a permissive rule table as a
sandbox. It is not one and was never meant to be.

### Boop runs with your privileges

There is no process isolation, no container, no seccomp filter and no user
separation. An approved command runs as you, with your environment and your
credentials. If that is not acceptable for your threat model, run Boop inside a
container or a VM.

### Prompt injection is real and not solved

Content Boop reads — a file, a fetched page, a search result, a command's
output — reaches the model, and a model can be steered by it. Boop's answer is
structural rather than clever: the permission engine does not consult the
model's reasoning, so an injected instruction can make the model *ask* for
something dangerous, but not make it happen. In `confirm` mode you see the
request. In `auto` mode with permissive rules you may not.

This is the strongest argument for keeping `execution.mode: confirm` and for
leaving `network.enabled: false` unless you need it.

### `robots.txt` is honoured, not enforced

`respect_robots` defaults to true and can be turned off. That is a politeness
setting, not a security control.

### Under-construction subsystems

The TUI, WebUI, agent scheduler, document handling and structured logging are
being built at the time of writing. Their security properties are less settled
than the core's. Reports against them are welcome and in scope.

## Hardening checklist

If Boop is doing anything beyond your own laptop:

- [ ] Keep `execution.mode: confirm`.
- [ ] Set `permissions.git.push: deny` unless you want it pushing.
- [ ] Set `permissions.production.change: deny` on any machine that can reach
      production.
- [ ] Leave `network.enabled: false` unless you need web access; if you enable
      it, set `allowed_domains`.
- [ ] Never set `network.allow_private_networks: true` on a host that can reach
      an intranet or a cloud metadata service.
- [ ] Keep the WebUI on `127.0.0.1` and reach it through an SSH tunnel.
- [ ] If you must bind it wider, enable token authentication with real entropy
      and set `allowed_origins` explicitly.
- [ ] Never use `BOOP_WEB_ALLOW_INSECURE` or `--allow-insecure-bind` on an
      untrusted network.
- [ ] Run Boop as an unprivileged user with a workspace it does not need `sudo`
      to work in.
- [ ] Run `make security` (`govulncheck`) before you ship anything built from
      this tree.
