#!/usr/bin/env bash
# Capture screenshots of the running Boop WebUI into assets/screenshots/.
#
# Builds the frontend if needed, starts the server on a scratch port bound to
# loopback, waits for it to answer, and photographs it with headless Chromium
# at several viewport sizes. These are shots of the real interface served by
# the real binary, not mock-ups.
#
# Usage: ./assets/screenshot-web.sh [name-prefix]
set -euo pipefail
cd "$(dirname "$0")/.."

PREFIX=${1:-webui}
PORT=${PORT:-8599}
OUT=assets/screenshots
CONFIG_DIR=${CONFIG_DIR:-/tmp/boop-webshot}
mkdir -p "$OUT"

CHROME=$(command -v chromium || command -v google-chrome || true)
[ -n "$CHROME" ] || { echo "chromium or google-chrome required" >&2; exit 1; }

# Frontend bundle. Skipped when it is already built, since npm is slow.
if [ -d web/frontend ] && [ ! -d web/static/dist ]; then
    echo "building the frontend bundle..."
    if [ -f web/frontend/package.json ] && command -v npm >/dev/null; then
        (cd web/frontend && npm install --silent && npm run build --silent) || {
            echo "frontend build failed; the server will serve its placeholder" >&2; }
        [ -d web/frontend/dist ] && { mkdir -p web/static/dist; cp -r web/frontend/dist/. web/static/dist/; }
    fi
fi

echo "building boop..."
make build >/dev/null

rm -rf "$CONFIG_DIR"; mkdir -p "$CONFIG_DIR"
cat > "$CONFIG_DIR/config.yaml" <<EOF
version: 1
provider: ollama
model: llama3.1:8b
execution:
  mode: confirm
web:
  enabled: true
  listen: 127.0.0.1
  port: $PORT
  auth:
    enabled: false
providers:
  ollama:
    type: ollama
    base_url: ${OLLAMA_URL:-http://127.0.0.1:11434}
EOF

echo "starting the WebUI on 127.0.0.1:$PORT ..."
BOOP_CONFIG_DIR="$CONFIG_DIR" ./boop --web --listen 127.0.0.1 --port "$PORT" \
    > /tmp/boop-web.log 2>&1 &
SERVER=$!
trap 'kill $SERVER 2>/dev/null || true' EXIT

for _ in $(seq 1 40); do
    if curl -fsS -m 2 "http://127.0.0.1:$PORT/" -o /dev/null 2>/dev/null; then ready=1; break; fi
    kill -0 $SERVER 2>/dev/null || { echo "server exited early:"; tail -20 /tmp/boop-web.log; exit 1; }
    sleep 0.5
done
[ "${ready:-0}" = 1 ] || { echo "server never became ready:"; tail -20 /tmp/boop-web.log; exit 1; }
echo "server is up"

shoot () { # $1 = suffix, $2 = WxH
    local w=${2%x*} h=${2#*x}
    "$CHROME" --headless --disable-gpu --no-sandbox --hide-scrollbars \
        --force-device-scale-factor=2 --virtual-time-budget=4000 \
        --window-size="$w,$h" --screenshot="/tmp/web-$1.png" \
        "http://127.0.0.1:$PORT/" >/dev/null 2>&1
    if [ -s "/tmp/web-$1.png" ]; then
        cp "/tmp/web-$1.png" "$OUT/${PREFIX}-$1.png"
        echo "  wrote $OUT/${PREFIX}-$1.png ($(identify -format '%wx%h' "$OUT/${PREFIX}-$1.png" 2>/dev/null))"
    else
        echo "  FAILED to capture $1" >&2
    fi
}

shoot desktop 1440x900
shoot wide    1920x1080
shoot mobile  414x896

echo "done. server log: /tmp/boop-web.log"
