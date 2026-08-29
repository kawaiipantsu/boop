#!/usr/bin/env bash
# Capture a real screenshot of the Boop TUI into assets/screenshots/.
#
# Runs boop inside a fixed-size tmux pane, drives it with scripted keystrokes,
# captures the pane's rendered screen (ANSI intact), and rasterises it through
# assets/ansi2png.py and headless Chromium. The result is what the terminal
# actually drew, not a mock-up.
#
# Usage: ./assets/screenshot.sh <name> "<title>" [keystroke-script-file]
set -euo pipefail
cd "$(dirname "$0")/.."

NAME=${1:?usage: screenshot.sh <name> "<title>" [script]}
TITLE=${2:-boop}
SCRIPT=${3:-}
COLS=${COLS:-104}
ROWS=${ROWS:-32}
OUT=assets/screenshots
mkdir -p "$OUT"

for c in tmux python3; do command -v $c >/dev/null || { echo "$c required" >&2; exit 1; }; done
CHROME=$(command -v chromium || command -v google-chrome || true)
[ -n "$CHROME" ] || { echo "chromium or google-chrome required" >&2; exit 1; }

SESSION="boopshot-$$"
trap 'tmux kill-session -t "$SESSION" 2>/dev/null || true' EXIT

tmux new-session -d -s "$SESSION" -x "$COLS" -y "$ROWS" "${BOOP_CMD:-./boop}"
sleep "${SETTLE:-2}"

if [ -n "$SCRIPT" ] && [ -f "$SCRIPT" ]; then
    # Each line is either  send:<text>  key:<tmux-key>  or  wait:<seconds>
    while IFS= read -r line; do
        case "$line" in
            send:*) tmux send-keys -t "$SESSION" -l "${line#send:}" ;;
            key:*)  tmux send-keys -t "$SESSION" "${line#key:}" ;;
            wait:*) sleep "${line#wait:}" ;;
        esac
    done < "$SCRIPT"
fi
sleep "${SETTLE:-2}"

tmux capture-pane -t "$SESSION" -e -p > "/tmp/${SESSION}.ansi"
python3 assets/ansi2png.py "$TITLE" < "/tmp/${SESSION}.ansi" > "/tmp/${SESSION}.html"

"$CHROME" --headless --disable-gpu --no-sandbox --hide-scrollbars \
    --force-device-scale-factor=2 --default-background-color=00000000 \
    --window-size=$((COLS * 9 + 60)),$((ROWS * 19 + 90)) \
    --screenshot="/tmp/${SESSION}.png" "file:///tmp/${SESSION}.html" >/dev/null 2>&1

command -v convert >/dev/null && convert "/tmp/${SESSION}.png" -trim +repage "$OUT/${NAME}.png" \
    || cp "/tmp/${SESSION}.png" "$OUT/${NAME}.png"

echo "wrote $OUT/${NAME}.png ($(identify -format '%wx%h' "$OUT/${NAME}.png" 2>/dev/null || echo '?'))"
