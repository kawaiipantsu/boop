# Boop

A cross-platform local AI client and agent runtime.

Boop is not a chat frontend. It is an AI execution environment: a
provider-neutral model runtime, a local tool execution engine, a permission and
safety policy engine, session and project memory, and an agent scheduler —
fronted by a terminal UI, a plain CLI, a local WebUI, and later a native GUI.

**Status:** early development. See [PROJECT.md](PROJECT.md) for the full
specification and [CHANGELOG.md](CHANGELOG.md) for what currently exists.

## Principles

- **Local-first.** Lemonade, LM Studio and Ollama are first-class. Cloud
  providers are optional extensions, never a prerequisite.
- **Provider-neutral.** Nothing outside `internal/provider` knows which backend
  is in use.
- **UI-independent core.** The TUI, WebUI and CLI are frontends over one runtime.
- **Permissions enforced in code.** Prompt instructions are not a security
  control.
- **Failure is information.** Command results come back structured so the model
  can diagnose, repair, retry and validate.

## Requirements

- Go 1.25 or newer
- Git

## Quick start

```bash
make build
./boop version
```

Run `make help` to see every available target.

## Development

```bash
make fmt vet test build
```

Boop follows Git Flow: `main` holds tagged releases, `develop` is the
integration branch, and work happens on `feature/<name>` branches. See
PROJECT.md §31 for the full policy.

## License

See [LICENSE](LICENSE).
