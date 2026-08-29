// Package e2e holds Boop's end-to-end tests.
//
// Every test file here carries the `e2e` build tag and therefore only compiles
// under `go test -tags=e2e ./tests/e2e/...`, which is what `make test-e2e`
// runs.
//
// These tests drive representative whole flows — the prompt → tool call →
// failure → repair → success loop of PROJECT.md §13 and §41 — against the
// deterministic fake provider in tests/fixtures. They never contact a live or
// paid model API, and they must stay reproducible: no wall-clock dependence,
// no network, no ordering luck.
//
// This file is deliberately untagged so the package always has at least one
// buildable Go file.
package e2e
