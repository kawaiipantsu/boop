// Package integration holds Boop's integration tests.
//
// Every test file here carries the `integration` build tag and therefore only
// compiles under `go test -tags=integration ./tests/integration/...`, which is
// what `make test-integration` runs. Keeping them behind a tag means the
// default `go test ./...` stays fast and hermetic.
//
// These tests exercise components across a real boundary — today the fake
// provider server of PROJECT.md §42 over real HTTP — but never a live or paid
// model API (§41).
//
// This file is deliberately untagged so the package always has at least one
// buildable Go file.
package integration
