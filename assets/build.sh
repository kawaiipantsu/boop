#!/usr/bin/env bash
# Regenerate every Boop graphic from the SVG masters in assets/logo/.
#
# Requires: rsvg-convert (librsvg) and ImageMagick.
# Run from the repository root:  ./assets/build.sh
set -euo pipefail

cd "$(dirname "$0")/.."
OUT_PNG=assets/png
OUT_SOCIAL=assets/social
OUT_FAV=assets/favicon
mkdir -p "$OUT_PNG" "$OUT_SOCIAL" "$OUT_FAV"

command -v rsvg-convert >/dev/null || { echo "rsvg-convert required (apt install librsvg2-bin)" >&2; exit 1; }
command -v convert      >/dev/null || { echo "ImageMagick required (apt install imagemagick)" >&2; exit 1; }

# ---------------------------------------------------------------- brand
VIOLET='#7C3AED'; BLUE='#2563EB'; CYAN='#06B6D4'
AMBER='#FBBF24';  INK='#0B1020';  PAPER='#F8FAFC'
TAGLINE='Local-first AI client and agent runtime'

# Shared geometry, emitted into composed artwork so nothing depends on a font.
mark_group () { # $1 = translate/scale transform
cat <<EOF
  <g transform="$1">
    <rect x="0" y="0" width="512" height="512" rx="116" ry="116" fill="url(#boopGrad)"/>
    <path d="M 168 168 L 250 256 L 168 344" fill="none" stroke="#FFFFFF" stroke-width="44"
          stroke-linecap="round" stroke-linejoin="round"/>
    <rect x="286" y="318" width="110" height="30" rx="15" fill="$AMBER"/>
  </g>
EOF
}
word_group () { # $1 = transform, $2 = stroke colour
cat <<EOF
  <g transform="$1">
    <g fill="none" stroke="$2" stroke-width="26" stroke-linecap="round">
      <path d="M 15 -55 L 15 95"/>
      <circle cx="60" cy="50" r="45"/><circle cx="196" cy="50" r="45"/>
      <circle cx="332" cy="50" r="45"/><circle cx="468" cy="50" r="45"/>
      <path d="M 423 5 L 423 155"/>
    </g>
    <circle cx="556" cy="95" r="16" fill="$AMBER"/>
  </g>
EOF
}
grad_def () {
cat <<EOF
  <defs>
    <linearGradient id="boopGrad" x1="0" y1="0" x2="1" y2="1">
      <stop offset="0%" stop-color="$VIOLET"/><stop offset="55%" stop-color="$BLUE"/>
      <stop offset="100%" stop-color="$CYAN"/>
    </linearGradient>
    <linearGradient id="bgGrad" x1="0" y1="0" x2="1" y2="1">
      <stop offset="0%" stop-color="#131A33"/><stop offset="100%" stop-color="$INK"/>
    </linearGradient>
  </defs>
EOF
}

# ---------------------------------------------------------------- lockups
build_lockup () { # $1 = out file, $2 = wordmark colour
  {
    echo '<?xml version="1.0" encoding="UTF-8"?>'
    echo '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 572 220" width="572" height="220" role="img" aria-label="Boop">'
    echo '  <title>Boop</title>'
    grad_def
    mark_group "translate(20,26) scale(0.328)"
    word_group "translate(248,84) scale(0.52)" "$2"
    echo '</svg>'
  } > "$1"
}
build_lockup assets/logo/boop-lockup.svg      "$INK"
build_lockup assets/logo/boop-lockup-dark.svg "$PAPER"

# ---------------------------------------------------------------- png sizes
for s in 16 24 32 48 64 128 256 512 1024; do
  rsvg-convert -w $s -h $s assets/logo/boop-mark.svg -o "$OUT_PNG/mark-${s}.png"
done
rsvg-convert -w 1240 assets/logo/boop-wordmark.svg      -o "$OUT_PNG/wordmark-light.png"
rsvg-convert -w 1240 assets/logo/boop-wordmark-dark.svg -o "$OUT_PNG/wordmark-dark.png"
rsvg-convert -w 1800 assets/logo/boop-lockup.svg        -o "$OUT_PNG/lockup-light.png"
rsvg-convert -w 1800 assets/logo/boop-lockup-dark.svg   -o "$OUT_PNG/lockup-dark.png"

# ---------------------------------------------------------------- favicons
rsvg-convert -w 180 -h 180 assets/logo/boop-mark.svg -o "$OUT_FAV/apple-touch-icon.png"
for s in 16 32 48; do cp "$OUT_PNG/mark-${s}.png" "$OUT_FAV/favicon-${s}.png"; done
convert "$OUT_FAV/favicon-16.png" "$OUT_FAV/favicon-32.png" "$OUT_FAV/favicon-48.png" "$OUT_FAV/favicon.ico"

# ---------------------------------------------------------------- social
# $1=file $2=width $3=height $4=markScale $5=markX $6=markY $7=wordScale $8=wordX $9=wordY ${10}=tagY ${11}=tagSize
social () {
  local f=$1 w=$2 h=$3 ms=$4 mx=$5 my=$6 ws=$7 wx=$8 wy=$9 ty=${10} tsz=${11}
  {
    echo '<?xml version="1.0" encoding="UTF-8"?>'
    echo "<svg xmlns=\"http://www.w3.org/2000/svg\" viewBox=\"0 0 $w $h\" width=\"$w\" height=\"$h\">"
    grad_def
    echo "  <rect width=\"$w\" height=\"$h\" fill=\"url(#bgGrad)\"/>"
    # accent hairline along the top edge
    echo "  <rect width=\"$w\" height=\"6\" fill=\"url(#boopGrad)\"/>"
    mark_group "translate($mx,$my) scale($ms)"
    word_group "translate($wx,$wy) scale($ws)" "$PAPER"
    echo "  <text x=\"$((w/2))\" y=\"$ty\" text-anchor=\"middle\" fill=\"#93A3C0\" \
font-family=\"DejaVu Sans, Helvetica, Arial, sans-serif\" font-size=\"$tsz\">$TAGLINE</text>"
    echo "</svg>"
  } > /tmp/social.svg
  rsvg-convert -w "$w" -h "$h" /tmp/social.svg -o "$f"
}

# Group width = mark(215) + gap(40) + wordmark(355) = 610, centred per canvas.
# Wordmark y is set so its optical centre matches the mark's, not its box top.
social "$OUT_SOCIAL/github-social-preview.png" 1280 640 0.42 335 143 0.62 590 219 470 30
social "$OUT_SOCIAL/og-image.png"              1200 630 0.42 295 138 0.62 550 214 462 30
social "$OUT_SOCIAL/twitter-card.png"          1200 675 0.42 295 158 0.62 550 234 500 30

# Avatar: the mark alone, square, for a profile picture.
rsvg-convert -w 512 -h 512 assets/logo/boop-mark.svg -o "$OUT_SOCIAL/avatar-512.png"
rsvg-convert -w 400 -h 400 assets/logo/boop-mark.svg -o "$OUT_SOCIAL/avatar-400.png"

# README banner, regenerated locally rather than hotlinked.
{
  echo '<?xml version="1.0" encoding="UTF-8"?>'
  echo '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1280 300" width="1280" height="300">'
  grad_def
  echo '  <rect width="1280" height="300" fill="url(#bgGrad)"/>'
  echo '  <rect width="1280" height="6" fill="url(#boopGrad)"/>'
  mark_group "translate(419,43) scale(0.30)"
  word_group "translate(603,98) scale(0.45)" "$PAPER"
  echo "  <text x=\"640\" y=\"258\" text-anchor=\"middle\" fill=\"#93A3C0\" \
font-family=\"DejaVu Sans, Helvetica, Arial, sans-serif\" font-size=\"26\">$TAGLINE</text>"
  echo '</svg>'
} > /tmp/banner.svg
rsvg-convert -w 2560 -h 600 /tmp/banner.svg -o .github/assets/header.png

echo "generated:"
find assets .github/assets -name '*.png' -o -name '*.ico' -o -name '*.svg' | sort | sed 's/^/  /'
