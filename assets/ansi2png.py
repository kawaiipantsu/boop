#!/usr/bin/env python3
"""Render captured terminal output (with ANSI SGR codes) to a PNG.

Used to produce the TUI screenshots in assets/screenshots. Takes a tmux
capture-pane dump on stdin and writes standalone HTML on stdout, which
assets/screenshot.sh then rasterises with headless Chromium.

Handles the SGR subset a Lip Gloss TUI actually emits: 16-colour, 256-colour
and 24-bit truecolour foreground/background, bold, dim, italic, underline,
reverse, and reset.
"""
import html
import re
import sys

# A terminal palette close to what most users see, so screenshots look familiar.
BASE16 = [
    "#1c1f26", "#e06c75", "#98c379", "#e5c07b", "#61afef", "#c678dd", "#56b6c2", "#abb2bf",
    "#5c6370", "#ef7b85", "#a8d389", "#f0cb8b", "#71bfff", "#d68aed", "#66c5d2", "#ffffff",
]
BG = "#0b1020"
FG = "#c8d3e6"

def xterm256(n: int) -> str:
    if n < 16:
        return BASE16[n]
    if n < 232:
        n -= 16
        r, g, b = n // 36, (n % 36) // 6, n % 6
        conv = lambda v: 0 if v == 0 else 55 + 40 * v
        return "#%02x%02x%02x" % (conv(r), conv(g), conv(b))
    v = 8 + (n - 232) * 10
    return "#%02x%02x%02x" % (v, v, v)

class Style:
    __slots__ = ("fg", "bg", "bold", "dim", "italic", "underline", "reverse")

    def __init__(self):
        self.reset()

    def reset(self):
        self.fg = self.bg = None
        self.bold = self.dim = self.italic = self.underline = self.reverse = False

    def copy(self):
        s = Style()
        for f in self.__slots__:
            setattr(s, f, getattr(self, f))
        return s

    def css(self) -> str:
        fg, bg = self.fg or FG, self.bg
        if self.reverse:
            fg, bg = bg or BG, fg
        parts = [f"color:{fg}"]
        if bg:
            parts.append(f"background:{bg}")
        if self.bold:
            parts.append("font-weight:700")
        if self.dim:
            parts.append("opacity:.65")
        if self.italic:
            parts.append("font-style:italic")
        if self.underline:
            parts.append("text-decoration:underline")
        return ";".join(parts)

def apply_sgr(style: Style, params):
    i = 0
    if not params:
        params = [0]
    while i < len(params):
        p = params[i]
        if p == 0:
            style.reset()
        elif p == 1:
            style.bold = True
        elif p == 2:
            style.dim = True
        elif p == 3:
            style.italic = True
        elif p == 4:
            style.underline = True
        elif p == 7:
            style.reverse = True
        elif p == 22:
            style.bold = style.dim = False
        elif p == 23:
            style.italic = False
        elif p == 24:
            style.underline = False
        elif p == 27:
            style.reverse = False
        elif 30 <= p <= 37:
            style.fg = BASE16[p - 30]
        elif 90 <= p <= 97:
            style.fg = BASE16[p - 90 + 8]
        elif 40 <= p <= 47:
            style.bg = BASE16[p - 40]
        elif 100 <= p <= 107:
            style.bg = BASE16[p - 100 + 8]
        elif p == 39:
            style.fg = None
        elif p == 49:
            style.bg = None
        elif p in (38, 48):
            target = "fg" if p == 38 else "bg"
            if i + 1 < len(params) and params[i + 1] == 5:
                setattr(style, target, xterm256(params[i + 2]))
                i += 2
            elif i + 1 < len(params) and params[i + 1] == 2:
                r, g, b = params[i + 2], params[i + 3], params[i + 4]
                setattr(style, target, "#%02x%02x%02x" % (r, g, b))
                i += 4
        i += 1

SGR = re.compile(r"\x1b\[([0-9;]*)m")
# Strip cursor/erase CSI sequences and OSC strings, but NOT SGR: the final
# byte class deliberately excludes lowercase "m", or the colours would be
# removed before they were ever parsed.
OTHER_CSI = re.compile(r"\x1b\[[0-9;?]*[A-Za-ln-z]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[()][A-Za-z0-9]")

def convert(text: str) -> str:
    out = []
    style = Style()
    for raw_line in text.split("\n"):
        line = OTHER_CSI.sub("", raw_line.replace("\r", ""))
        spans, pos = [], 0
        for m in SGR.finditer(line):
            chunk = line[pos:m.start()]
            if chunk:
                spans.append(f'<span style="{style.css()}">{html.escape(chunk)}</span>')
            apply_sgr(style, [int(x) if x else 0 for x in m.group(1).split(";")])
            pos = m.end()
        tail = line[pos:]
        if tail:
            spans.append(f'<span style="{style.css()}">{html.escape(tail)}</span>')
        out.append("".join(spans) or "&nbsp;")
    return "\n".join(out)

def main():
    body = convert(sys.stdin.read().rstrip("\n"))
    title = sys.argv[1] if len(sys.argv) > 1 else ""
    sys.stdout.write(f"""<!doctype html>
<html><head><meta charset="utf-8"><style>
  html,body {{ margin:0; padding:0; background:transparent; }}
  .frame {{
    display:inline-block; background:{BG}; border-radius:10px;
    padding:0 0 14px 0; box-shadow:0 18px 50px rgba(0,0,0,.55);
    font-family:"DejaVu Sans Mono","Liberation Mono",monospace;
  }}
  .bar {{ height:30px; border-radius:10px 10px 0 0; background:#161b2e;
         display:flex; align-items:center; padding:0 12px; gap:8px; }}
  .dot {{ width:11px; height:11px; border-radius:50%; }}
  .title {{ color:#7f8ca6; font-size:12px; margin-left:8px;
            font-family:"DejaVu Sans",sans-serif; }}
  pre {{ margin:0; padding:14px 16px 0 16px; color:{FG};
         font-size:14px; line-height:1.32; white-space:pre; }}
</style></head><body>
<div class="frame">
  <div class="bar">
    <div class="dot" style="background:#ff5f57"></div>
    <div class="dot" style="background:#febc2e"></div>
    <div class="dot" style="background:#28c840"></div>
    <div class="title">{html.escape(title)}</div>
  </div>
  <pre>{body}</pre>
</div>
</body></html>""")

if __name__ == "__main__":
    main()
