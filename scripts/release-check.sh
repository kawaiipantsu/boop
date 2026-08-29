#!/usr/bin/env bash
# Verify the working tree is ready to cut a release.
set -euo pipefail

VERSION="${1:?usage: release-check.sh <version>}"
fail=0
note() { printf '  %-6s %s\n' "$1" "$2"; }

echo "release-check for v${VERSION}"

if [ -n "$(git status --porcelain 2>/dev/null)" ]; then
    note "FAIL" "working tree is dirty"; fail=1
else
    note "ok" "working tree is clean"
fi

if grep -q "\[${VERSION}\]" CHANGELOG.md 2>/dev/null; then
    note "ok" "CHANGELOG.md has an entry for ${VERSION}"
else
    note "FAIL" "CHANGELOG.md has no entry for ${VERSION}"; fail=1
fi

if git rev-parse "v${VERSION}" >/dev/null 2>&1; then
    note "FAIL" "tag v${VERSION} already exists"; fail=1
else
    note "ok" "tag v${VERSION} is available"
fi

if grep -rn "TODO: before release" --include='*.go' . >/dev/null 2>&1; then
    note "FAIL" "release-blocking TODOs remain"; fail=1
else
    note "ok" "no release-blocking TODOs"
fi

[ "$fail" -eq 0 ] && echo "release-check passed" || { echo "release-check failed" >&2; exit 1; }
