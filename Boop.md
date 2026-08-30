# Boop Project Memory

## Project

<!-- boop:generated -->

Name: boop
Root: /var/www/projects/boop
Languages: Go (275), TypeScript (29), JavaScript (11), Shell (7), HTML (2), SQL (2)
Frameworks: Bubble Tea, GitHub Actions, SQLite (modernc)
Git: branch develop, remote origin, 51 uncommitted change(s)
Documentation: CHANGELOG.md, CLAUDE.md, CONTRIBUTING.md, LICENSE, PROJECT.md, README.md, docs/architecture.md, docs/configuration.md, docs/permissions.md, docs/providers.md, docs/releases.md, docs/webui.md

<!-- /boop:generated -->

## Goals

## Architecture

<!-- boop:generated -->

Primary language: Go (404 files scanned)

Top-level layout:

- `internal/` — 218 file(s)
- `web/` — 60 file(s)
- `assets/` — 39 file(s)
- `tui/` — 23 file(s)
- `tests/` — 22 file(s)
- `.github/` — 7 file(s)
- `cmd/` — 7 file(s)
- `docs/` — 6 file(s)
- `prompts/` — 4 file(s)
- `scripts/` — 3 file(s)
- `gui/` — 1 file(s)

Entry points:

- `cmd/boop/main.go`

<!-- /boop:generated -->

## Important Files

<!-- boop:generated -->

- `Makefile`
- `go.mod`
- `PROJECT.md`
- `cmd/boop/main.go`

### Production-sensitive

Changes to these files can affect production; treat them with deliberate intent.

- `scripts/release-check.sh` — deployment script (deployment, high)

<!-- /boop:generated -->

## Decisions

## Current Work

## Known Problems

## Tests

<!-- boop:generated -->

156 test file(s) detected.

Test commands:

- `make test` (from Makefile)
- `make test-unit` (from Makefile)
- `go test ./...` (inferred from go.mod)

<!-- /boop:generated -->

## Useful Commands

<!-- boop:generated -->

Build:

- `make build` (from Makefile)
- `go build ./...` (inferred from go.mod)

Test:

- `make test` (from Makefile)
- `make test-unit` (from Makefile)
- `go test ./...` (inferred from go.mod)

Lint:

- `make vet` (from Makefile)
- `make lint` (from Makefile)
- `go vet ./...` (inferred from go.mod)

Format:

- `make fmt` (from Makefile)
- `gofmt -w .` (inferred from go.mod)

<!-- /boop:generated -->

## Agent Notes

- 2026-08-30: `main` and `develop` are protected via GitHub rulesets (require PR,
  require the CI status checks — Test, Cross-build, Lint, Branch flow — to
  pass, block force-push/deletion). `main` only accepts merges from
  `release/<version>` or `hotfix/<...>`; the `.github/workflows/branch-flow.yml`
  job enforces this as a required check, because 24 open PRs had all been
  opened against `main` from `feature/*` branches before this was in place.
  Configure further via `gh api repos/kawaiipantsu/boop/rulesets`, not the
  classic branch-protection endpoint.

- 2026-08-30: `/config` (issue #16, §55) now takes direct set commands as well
  as printing the effective config: `/config mode auto|confirm`,
  `/config agents on|off`, `/config agents max <n>`, `/config web on|off`,
  `/config web port <n>`, `/config web listen <ip>`. Each reloads `config.yaml`
  (so per-invocation `--mode`/`--provider` flags are never persisted), applies
  one field, validates, and saves via `config.Save()`. `mode` and `agents`
  also move the running evaluator/fleet through the existing `/permissions`
  and `/agents` helpers; `web.*` only persists and says to restart
  `boop --web`. Lives in `tui/commands_config.go`. The full interactive editor
  (provider, model, timeouts, token limits, temperature, logging) is still
  outstanding and waits on the synchronised config holder from issue #6 for
  the settings a live process cannot safely adopt.

## Session Summaries

