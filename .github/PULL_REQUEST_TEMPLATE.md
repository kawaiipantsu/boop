<!--
Thanks for contributing to Boop.

Before opening: read CONTRIBUTING.md. PROJECT.md is the authoritative spec —
if this change contradicts it, say so explicitly below. Changing the spec is
allowed; doing it by accident is not.
-->

## What this changes

<!-- One or two sentences. What is different after this is merged? -->

## Why

<!--
The reasoning, not the diff. What was broken or missing, what you expected,
and what alternatives you rejected. Link the issue if there is one.
-->

Closes #

## How to verify

<!--
The commands a reviewer runs to see it work, and what they should see.
-->

```bash

```

## Definition of done (§62)

- [ ] Implementation is complete — no reachable stubs left behind
- [ ] Tests exist where appropriate
- [ ] `make fmt vet test build` passes
- [ ] User-facing behaviour is documented in `docs/`
- [ ] `CHANGELOG.md` updated under `[Unreleased]`, in the right section
- [ ] Errors are handled, not swallowed
- [ ] Platform assumptions are explicit
- [ ] No secrets in code, fixtures, tests or `Boop.md`
- [ ] Commits are small, cohesive and conventionally prefixed
- [ ] `Boop.md` updated if this embeds a durable project decision

## Checks that apply

<!-- Tick what is relevant; delete the rest. -->

- [ ] **Cross-compilation** still works — `make build-all`, or at least
      `CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build ./...`
- [ ] **No new cgo dependency** (it would break five of six release targets)
- [ ] **New dependency** justified here, and small, mature and clearly licensed
- [ ] **Permission layer** — any new tool reports a `permissions.Action` from
      `Permission()` before `Execute()`, carrying category, risk *and* the
      production flag
- [ ] **Repair loop** — recoverable failures return `Result{IsError: true}`,
      not a Go `error`
- [ ] **Core stays UI-independent** — no business logic added to `tui/` or
      `web/`
- [ ] **Provider neutrality** — no vendor specifics outside
      `internal/provider/<vendor>/`; new vendor features go in optional
      interfaces
- [ ] **Tests need no network or paid API** — live tests are opt-in behind
      `BOOP_TEST_*` and skip when unset
- [ ] **Branch** is `feature/…` off `develop` (or `hotfix/…` off `main`), not
      a commit to `main`

## Anything a reviewer should know

<!--
Known gaps, follow-up work, parts you are unsure about, decisions you would
like a second opinion on. Uncertainty stated up front is cheaper than
uncertainty discovered in review.
-->
