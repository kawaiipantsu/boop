#!/usr/bin/env bash
# Build the WebUI bundle embedded into the Go binary.
set -euo pipefail

FRONTEND="web/frontend"

if [ ! -d "$FRONTEND" ]; then
    echo "no $FRONTEND yet; WebUI lands in milestone 9 — skipping"
    mkdir -p web/static/dist
    exit 0
fi

command -v npm >/dev/null 2>&1 || { echo "npm is required to build the WebUI" >&2; exit 1; }
npm --prefix "$FRONTEND" ci
npm --prefix "$FRONTEND" run build
echo "WebUI bundle written to web/static/dist"
