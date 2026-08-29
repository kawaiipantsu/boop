<div align="center">
<img src="png/lockup-dark.png#gh-dark-mode-only" width="360" alt="Boop"/>
<img src="png/lockup-light.png#gh-light-mode-only" width="360" alt="Boop"/>
</div>

# Boop brand assets

Everything here is generated from the SVG masters in [`logo/`](logo) by
[`build.sh`](build.sh). Edit a master, re-run the script, commit the output —
do not hand-edit anything in `png/`, `social/` or `favicon/`.

```bash
./assets/build.sh      # needs rsvg-convert (librsvg2-bin) and ImageMagick
```

## The mark

A terminal prompt: a chevron and a cursor. The cursor is amber — that accent
is "the boop", and it is the one element that stays constant across every
variant.

Both the mark and the wordmark are **pure geometry**: circles, rounded
rectangles and stroked paths, with no text elements. Nothing depends on a font
being installed, so the SVGs render identically everywhere and the wordmark
cannot reflow into a different typeface on someone else's machine.

The wordmark is built the way the letters actually work — `b` and `p` are the
same bowl with the stem running up or down — which is why four circles and two
lines spell "boop".

## Palette

| Token | Hex | Use |
|:--|:--|:--|
| Violet | `#7C3AED` | Gradient start |
| Blue | `#2563EB` | Gradient middle |
| Cyan | `#06B6D4` | Gradient end |
| **Amber** | `#FBBF24` | **Accent — the cursor. Never substitute.** |
| Ink | `#0B1020` | Dark backgrounds, light-mode wordmark |
| Paper | `#F8FAFC` | Dark-mode wordmark |

The gradient runs top-left to bottom-right at 0% / 55% / 100%.

## What is here

### `logo/` — masters, edit these

| File | Use |
|:--|:--|
| `boop-mark.svg` | Primary mark, gradient |
| `boop-mark-mono.svg` | Single colour via `currentColor`, for stamps, embroidery, one-colour print |
| `boop-wordmark.svg` / `-dark.svg` | Wordmark for light / dark backgrounds |
| `boop-lockup.svg` / `-dark.svg` | Mark and wordmark, horizontal |

### `png/` — raster exports

`mark-{16,24,32,48,64,128,256,512,1024}.png`, plus `wordmark-` and `lockup-`
in light and dark.

### `social/` — sized for each platform

| File | Size | Where |
|:--|:--|:--|
| `github-social-preview.png` | 1280×640 | GitHub → Settings → General → Social preview |
| `og-image.png` | 1200×630 | `og:image` for link unfurls |
| `twitter-card.png` | 1200×675 | `twitter:image`, summary_large_image |
| `avatar-512.png` | 512×512 | Profile / organisation picture |
| `avatar-400.png` | 400×400 | Smaller profile slots |

### `favicon/`

`favicon.ico` (16+32+48 multi-resolution), `favicon-{16,32,48}.png`,
`apple-touch-icon.png` (180×180).

```html
<link rel="icon" href="/favicon.ico" sizes="any">
<link rel="icon" href="/favicon-32.png" type="image/png">
<link rel="apple-touch-icon" href="/apple-touch-icon.png">
```

## Using it

**Do**
- Keep clear space around the mark of at least the corner radius.
- Use the mono mark when the gradient will not survive — fax, laser etching,
  single-colour print.
- Use `avatar-*.png` unpadded; platforms crop to a circle and the rounded
  square is designed to survive that.

**Don't**
- Recolour the amber cursor.
- Stretch the lockup, or rebuild it by placing the mark and wordmark by hand —
  the spacing in `boop-lockup.svg` is deliberate.
- Put the gradient mark on a mid-blue or mid-violet background; use the mono
  mark instead.
- Render the mark below 16px. Use the chevron alone if you need smaller.

## A note on generated banners

The README banner is committed as a PNG rather than hotlinked to
capsule-render. Two reasons: the README should not break when a third-party
service is down, and capsule-render writes `desc` text into its SVG
unescaped — an `&` in the description produces malformed XML that GitHub
silently refuses to render.
