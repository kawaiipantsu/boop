package tui

import (
	"strings"
	"testing"
)

func TestSanitize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello", "hello"},
		{"csi colour", "\x1b[31mred\x1b[0m", "red"},
		{"csi cursor move", "a\x1b[2Jb", "ab"},
		{"osc title", "\x1b]0;title\x07done", "done"},
		{"osc st terminated", "\x1b]8;;http://x\x1b\\link", "link"},
		{"bare escape", "a\x1bXb", "ab"},
		{"crlf", "a\r\nb", "a\nb"},
		{"lone cr", "a\rb", "a\nb"},
		{"tab expands", "a\tb", "a    b"},
		{"bell dropped", "a\x07b", "ab"},
		{"newline kept", "a\nb", "a\nb"},
		{"zero width joiner dropped", "a‍b", "ab"},
		{"unterminated csi", "a\x1b[31", "a"},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitize(tc.in); got != tc.want {
				t.Errorf("sanitize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestWrapText(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		width int
		want  []string
	}{
		{"fits", "hello world", 20, []string{"hello world"}},
		{"wraps on space", "hello world", 5, []string{"hello", "world"}},
		{"keeps blank lines", "a\n\nb", 10, []string{"a", "", "b"}},
		{"hard splits a long word", "abcdefghij", 4, []string{"abcd", "efgh", "ij"}},
		{"long word after short", "hi abcdefghij", 4, []string{"hi", "abcd", "efgh", "ij"}},
		{"zero width does not wrap", "hello world", 0, []string{"hello world"}},
		{"empty string", "", 10, []string{""}},
		{"exact fit", "abcd", 4, []string{"abcd"}},
		{"trailing space trimmed", "aa bb", 3, []string{"aa", "bb"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := wrapText(tc.in, tc.width)
			if !equalStrings(got, tc.want) {
				t.Errorf("wrapText(%q, %d) = %q, want %q", tc.in, tc.width, got, tc.want)
			}
			for i, line := range got {
				if tc.width > 0 && displayWidth(line) > tc.width {
					t.Errorf("line %d %q exceeds width %d", i, line, tc.width)
				}
			}
		})
	}
}

func TestWrapTextNeverExceedsWidth(t *testing.T) {
	text := "the quick brown fox jumps over supercalifragilisticexpialidocious lazy dogs\nand a second paragraph"
	for width := 1; width <= 40; width++ {
		for _, line := range wrapText(text, width) {
			if displayWidth(line) > width {
				t.Fatalf("width %d produced an over-long line %q", width, line)
			}
		}
	}
}

func TestWrapBlock(t *testing.T) {
	got := wrapBlock("one two three four", "> ", "  ", 8)
	want := []string{"> one", "  two", "  three", "  four"}
	if !equalStrings(got, want) {
		t.Fatalf("wrapBlock = %q, want %q", got, want)
	}
}

func TestWrapBlockDropsPrefixWhenTooNarrow(t *testing.T) {
	// A prefix that leaves fewer than minWrapWidth cells would produce an
	// unreadable column, so the text is wrapped bare instead.
	got := wrapBlock("hello world", "      > ", "        ", 10)
	for _, line := range got {
		if strings.HasPrefix(line, "      >") {
			t.Fatalf("expected the prefix to be dropped, got %q", got)
		}
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		in    string
		width int
		want  string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello", 4, "hel" + ellipsis},
		{"hello", 1, ellipsis},
		{"hello", 0, ""},
		{"hello", -1, ""},
	}
	for _, tc := range tests {
		if got := truncate(tc.in, tc.width); got != tc.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.in, tc.width, got, tc.want)
		}
	}
}

func TestPadRight(t *testing.T) {
	if got := padRight("ab", 5); got != "ab   " {
		t.Errorf("padRight = %q", got)
	}
	if got := padRight("abcdef", 3); displayWidth(got) != 3 {
		t.Errorf("padRight over-long value has width %d, want 3", displayWidth(got))
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
