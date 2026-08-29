#!/usr/bin/env bash
# Regenerate the README header from shieldcn.dev with the Boop mark embedded.
#
# The result is committed as a JPEG rather than hotlinked: the README front page
# should not break when a third-party renderer is down, and at 2400px a photo
# header is 160 KB as JPEG against 1.7 MB as PNG.
#
# Requires network access, rsvg-convert and ImageMagick.
set -euo pipefail
cd "$(dirname "$0")/.."

MARK=assets/png/mark-64.png
OUT=.github/assets/header.jpg
[ -f "$MARK" ] || { echo "run ./assets/build.sh first to generate $MARK" >&2; exit 1; }

LOGO=$(python3 -c "
import base64, urllib.parse, sys
data = base64.b64encode(open(sys.argv[1],'rb').read()).decode()
print(urllib.parse.quote('data:image/png;base64,'+data, safe=''))
" "$MARK")

PHOTO='https%3A%2F%2Fimages.unsplash.com%2Fphoto-1550745165-9bc0b252726f%3Fw%3D1600%26q%3D70%26fit%3Dcrop%26fm%3Djpg'
URL="https://shieldcn.dev/header/glow.svg?title=Boop&subtitle=Local-first+AI+client+and+agent+runtime&logo=${LOGO}&mode=dark&theme=emerald&font=jetbrains-mono&image=${PHOTO}"

tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
curl -fsSL --max-time 60 -o "$tmp/header.svg" "$URL"
python3 -c "import xml.etree.ElementTree as ET; ET.parse('$tmp/header.svg')" \
  || { echo "shieldcn returned malformed SVG; not overwriting $OUT" >&2; exit 1; }

rsvg-convert -w 2400 "$tmp/header.svg" -o "$tmp/header.png"
convert "$tmp/header.png" -strip -quality 82 "$OUT"
echo "wrote $OUT ($(du -h "$OUT" | cut -f1), $(identify -format '%wx%h' "$OUT"))"
