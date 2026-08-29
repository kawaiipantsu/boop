package tui

import (
	"strings"
	"unicode"

	"github.com/charmbracelet/lipgloss"
)

// tabWidth is how many spaces a horizontal tab expands to. Command output is
// full of tabs and terminals disagree about stop positions, so Boop fixes one
// width and wraps against it rather than letting the terminal decide.
const tabWidth = 4

// ellipsis marks text that truncate cut short.
const ellipsis = "…"

// displayWidth reports how many terminal cells s occupies.
//
// It is a thin wrapper so the rest of the package measures width one way, and
// so the dependency on Lip Gloss stays at the edge of the layout maths.
func displayWidth(s string) int { return lipgloss.Width(s) }

// sanitize makes arbitrary program output safe to place in the transcript.
//
// Tool output is not trusted to be well-behaved text: a build tool can emit
// colour codes, cursor movement, or a bare carriage return that would corrupt
// the frame Boop just drew. Escape sequences are stripped rather than
// forwarded, tabs are expanded so wrapping maths is honest, and CRLF is
// normalised. Newlines survive; nothing else that moves the cursor does.
func sanitize(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))

	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == 0x1b: // ESC — skip the whole sequence.
			i += escapeLength(runes[i:]) - 1
		case r == '\r':
			// Swallow CR; a following LF still ends the line.
			if i+1 < len(runes) && runes[i+1] == '\n' {
				continue
			}
			b.WriteByte('\n')
		case r == '\n':
			b.WriteByte('\n')
		case r == '\t':
			b.WriteString(strings.Repeat(" ", tabWidth))
		case r < 0x20 || r == 0x7f:
			// Other C0 controls and DEL are dropped outright.
		case unicode.Is(unicode.Cf, r):
			// Format characters (zero-width joiners, bidi overrides) can
			// disguise one string as another; they have no place in a
			// transcript of what a tool actually did.
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// escapeLength returns how many runes the escape sequence starting at runes[0]
// occupies, including the ESC itself. An unterminated sequence consumes the
// remainder, which is the safe reading: a half-written control sequence must
// never be handed to the terminal.
func escapeLength(runes []rune) int {
	if len(runes) < 2 {
		return len(runes)
	}
	switch runes[1] {
	case '[': // CSI: parameters then a final byte in @..~
		for i := 2; i < len(runes); i++ {
			if runes[i] >= 0x40 && runes[i] <= 0x7e {
				return i + 1
			}
		}
		return len(runes)
	case ']': // OSC: terminated by BEL or ST (ESC \)
		for i := 2; i < len(runes); i++ {
			if runes[i] == 0x07 {
				return i + 1
			}
			if runes[i] == 0x1b && i+1 < len(runes) && runes[i+1] == '\\' {
				return i + 2
			}
		}
		return len(runes)
	default:
		return 2
	}
}

// wrapText wraps text to width cells, honouring the newlines already in it.
//
// A width of zero or less is treated as "do not wrap", because a terminal that
// reports no width is a transient state during startup and losing the text
// would be worse than overflowing.
func wrapText(text string, width int) []string {
	lines := strings.Split(text, "\n")
	if width <= 0 {
		return lines
	}
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, wrapLine(line, width)...)
	}
	return out
}

// wrapLine greedily wraps a single newline-free line.
func wrapLine(line string, width int) []string {
	if line == "" {
		return []string{""}
	}
	if displayWidth(line) <= width {
		return []string{line}
	}

	var out []string
	var cur strings.Builder
	curWidth := 0

	flush := func() {
		out = append(out, cur.String())
		cur.Reset()
		curWidth = 0
	}

	for _, word := range splitWords(line) {
		w := displayWidth(word)
		switch {
		case isSpace(word):
			// Trailing whitespace never justifies a wrap of its own.
			if curWidth > 0 && curWidth+w <= width {
				cur.WriteString(word)
				curWidth += w
			}
		case curWidth == 0 && w > width:
			// A single word wider than the line (a path, a hash, a base64
			// blob) is hard-split; refusing to break it would overflow.
			for _, chunk := range hardSplit(word, width) {
				if curWidth > 0 {
					flush()
				}
				cur.WriteString(chunk)
				curWidth = displayWidth(chunk)
				if curWidth >= width {
					flush()
				}
			}
		case curWidth+w > width:
			flush()
			if w > width {
				for _, chunk := range hardSplit(word, width) {
					if curWidth > 0 {
						flush()
					}
					cur.WriteString(chunk)
					curWidth = displayWidth(chunk)
					if curWidth >= width {
						flush()
					}
				}
				continue
			}
			cur.WriteString(word)
			curWidth = w
		default:
			cur.WriteString(word)
			curWidth += w
		}
	}
	if curWidth > 0 || len(out) == 0 {
		flush()
	}
	// Greedy wrapping can leave trailing spaces at a break point.
	for i := range out {
		out[i] = strings.TrimRight(out[i], " ")
	}
	return out
}

// splitWords splits a line into alternating runs of spaces and non-spaces, so
// the wrapper can keep interior spacing while dropping it at line breaks.
func splitWords(line string) []string {
	var out []string
	var cur strings.Builder
	var inSpace bool
	for i, r := range line {
		isSp := r == ' '
		if i > 0 && isSp != inSpace {
			out = append(out, cur.String())
			cur.Reset()
		}
		inSpace = isSp
		cur.WriteRune(r)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func isSpace(s string) bool { return strings.TrimLeft(s, " ") == "" }

// hardSplit breaks an unbreakable run into width-sized chunks.
func hardSplit(word string, width int) []string {
	if width <= 0 {
		return []string{word}
	}
	var out []string
	var cur strings.Builder
	curWidth := 0
	for _, r := range word {
		rw := displayWidth(string(r))
		if curWidth+rw > width && curWidth > 0 {
			out = append(out, cur.String())
			cur.Reset()
			curWidth = 0
		}
		cur.WriteRune(r)
		curWidth += rw
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// wrapBlock wraps text into width cells with a prefix on the first line and a
// (usually blank) continuation prefix on the rest, so a wrapped tool result
// stays visually attached to its marker.
//
// When the prefixes leave too little room to be useful the text is wrapped
// bare: an unreadable two-character column is worse than a missing marker.
func wrapBlock(text, first, cont string, width int) []string {
	firstW := displayWidth(first)
	contW := displayWidth(cont)
	inner := width - maxInt(firstW, contW)
	if width <= 0 {
		lines := wrapText(text, 0)
		return applyPrefixes(lines, first, cont)
	}
	if inner < minWrapWidth {
		return wrapText(text, width)
	}
	return applyPrefixes(wrapText(text, inner), first, cont)
}

// minWrapWidth is the narrowest content column worth prefixing.
const minWrapWidth = 4

func applyPrefixes(lines []string, first, cont string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		if i == 0 {
			out[i] = strings.TrimRight(first+line, " ")
			continue
		}
		out[i] = strings.TrimRight(cont+line, " ")
	}
	return out
}

// truncate shortens s to at most width cells, marking the cut with an ellipsis.
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if displayWidth(s) <= width {
		return s
	}
	if width == 1 {
		return ellipsis
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		rw := displayWidth(string(r))
		if used+rw > width-1 {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	return b.String() + ellipsis
}

// padRight pads s with spaces to exactly width cells, truncating if needed.
// Fixed-width cells are what keep the header and footer from reflowing as
// values change length.
func padRight(s string, width int) string {
	if width <= 0 {
		return ""
	}
	s = truncate(s, width)
	if gap := width - displayWidth(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}
	return s
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
