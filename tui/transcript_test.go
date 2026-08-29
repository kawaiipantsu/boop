package tui

import (
	"strings"
	"testing"
	"time"
)

func TestTranscriptStreamsTokensIntoOneEntry(t *testing.T) {
	tr := NewTranscript(0)
	for _, tok := range []string{"Hel", "lo, ", "world"} {
		tr.AppendToken(tok)
	}
	if tr.Len() != 1 {
		t.Fatalf("expected the tokens to coalesce, got %d entries", tr.Len())
	}
	if !tr.Streaming() {
		t.Fatal("expected the entry to still be open")
	}
	if got := tr.Entries()[0].Text; got != "Hello, world" {
		t.Fatalf("text = %q", got)
	}

	tr.CloseStream()
	if tr.Streaming() {
		t.Fatal("expected the stream to be closed")
	}
	tr.AppendToken("next")
	if tr.Len() != 2 {
		t.Fatalf("expected a new entry after the stream closed, got %d", tr.Len())
	}
}

func TestTranscriptDropsAnEmptyStream(t *testing.T) {
	// A model that streams only whitespace before calling a tool must not
	// leave a blank paragraph in the transcript.
	tr := NewTranscript(0)
	tr.AppendToken("  \n ")
	tr.CloseStream()
	if tr.Len() != 0 {
		t.Fatalf("expected the empty stream to be dropped, got %d entries", tr.Len())
	}
}

func TestTranscriptToolLifecycle(t *testing.T) {
	tr := NewTranscript(0)
	tr.StartTool("run", "go test ./...")
	tr.StartTool("read", "notes.txt")

	if !tr.FinishTool("read", ToolOK, 12*time.Millisecond) {
		t.Fatal("expected to finish the read call")
	}
	if !tr.FinishTool("run", ToolFailed, 2*time.Second) {
		t.Fatal("expected to finish the run call")
	}
	if tr.FinishTool("run", ToolOK, 0) {
		t.Fatal("expected no further running call to match")
	}

	entries := tr.Entries()
	if entries[0].State != ToolFailed || entries[1].State != ToolOK {
		t.Fatalf("states = %v, %v", entries[0].State, entries[1].State)
	}

	lines := renderText(tr, 60)
	if !strings.Contains(lines, "run  go test ./...  [failed 2.0s]") {
		t.Fatalf("missing the failed headline:\n%s", lines)
	}
	if !strings.Contains(lines, "read  notes.txt  [ok 12ms]") {
		t.Fatalf("missing the ok headline:\n%s", lines)
	}
}

func TestTranscriptFinishToolMatchesAnyRunningCallForAnEmptyName(t *testing.T) {
	tr := NewTranscript(0)
	tr.StartTool("run", "sleep")
	if !tr.FinishTool("", ToolFailed, 0) {
		t.Fatal("expected an empty name to match any running call")
	}
	if tr.FinishTool("", ToolFailed, 0) {
		t.Fatal("expected nothing left running")
	}
}

func TestTranscriptAttachToolOutputInsertsInPlace(t *testing.T) {
	tr := NewTranscript(0)
	tr.StartTool("run", "ls")
	tr.FinishTool("run", ToolOK, time.Millisecond)
	tr.AppendText(EntryAssistant, "and here is what I found")

	if !tr.AttachToolOutput("run", "a.go\nb.go", false) {
		t.Fatal("expected the output to attach")
	}
	kinds := []EntryKind{}
	for _, e := range tr.Entries() {
		kinds = append(kinds, e.Kind)
	}
	want := []EntryKind{EntryTool, EntryOutput, EntryAssistant}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}
	if tr.AttachToolOutput("run", "again", false) {
		t.Fatal("expected a second attach to find no unattached call")
	}
}

func TestTranscriptAttachToolOutputIgnoresEmptyAndUnknown(t *testing.T) {
	tr := NewTranscript(0)
	tr.StartTool("run", "ls")
	if tr.AttachToolOutput("run", "   \n ", false) {
		t.Fatal("blank output should not attach")
	}
	if tr.AttachToolOutput("write", "x", false) {
		t.Fatal("output for a call that was never made should not attach")
	}
}

func TestClipLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"under the limit", "a\nb", 3, "a\nb"},
		{"at the limit", "a\nb\nc", 3, "a\nb\nc"},
		{"one over", "a\nb\nc\nd", 3, "a\nb\nc\n… 1 more line"},
		{"several over", "a\nb\nc\nd\ne", 3, "a\nb\nc\n… 2 more lines"},
		{"no limit", "a\nb", 0, "a\nb"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := clipLines(tc.in, tc.n); got != tc.want {
				t.Errorf("clipLines = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTranscriptBoundEvictsOldestAndSaysSo(t *testing.T) {
	tr := NewTranscript(3)
	for _, s := range []string{"one", "two", "three", "four", "five"} {
		tr.AppendText(EntrySystem, s)
	}
	if tr.Len() != 3 {
		t.Fatalf("len = %d, want 3", tr.Len())
	}
	if tr.Dropped() != 2 {
		t.Fatalf("dropped = %d, want 2", tr.Dropped())
	}
	out := renderText(tr, 60)
	if strings.Contains(out, "one") {
		t.Fatalf("expected the oldest entry to be gone:\n%s", out)
	}
	if !strings.Contains(out, "2 earlier entries dropped") {
		t.Fatalf("expected the eviction to be reported:\n%s", out)
	}
}

func TestTranscriptWrapsAtWidth(t *testing.T) {
	tr := NewTranscript(0)
	tr.AppendText(EntryUser, "the quick brown fox jumps over the lazy dog")
	for _, width := range []int{10, 20, 40, 80} {
		for _, line := range tr.Lines(width) {
			if displayWidth(line.Text) > width {
				t.Fatalf("width %d produced %q", width, line.Text)
			}
		}
	}
}

func TestTranscriptSanitizesOutput(t *testing.T) {
	tr := NewTranscript(0)
	tr.AppendText(EntryOutput, "\x1b[31mFAIL\x1b[0m\ttest")
	out := renderText(tr, 60)
	if strings.Contains(out, "\x1b[31m") {
		t.Fatalf("escape sequence survived into the transcript: %q", out)
	}
	if !strings.Contains(out, "FAIL") {
		t.Fatalf("text lost: %q", out)
	}
}

func TestTranscriptClear(t *testing.T) {
	tr := NewTranscript(2)
	tr.AppendText(EntrySystem, "a")
	tr.AppendText(EntrySystem, "b")
	tr.AppendText(EntrySystem, "c")
	tr.Clear()
	if tr.Len() != 0 || tr.Dropped() != 0 {
		t.Fatalf("clear left len=%d dropped=%d", tr.Len(), tr.Dropped())
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{0, "0ms"},
		{500 * time.Microsecond, "500µs"},
		{12 * time.Millisecond, "12ms"},
		{1500 * time.Millisecond, "1.5s"},
		{90 * time.Second, "1m30s"},
	}
	for _, tc := range tests {
		if got := formatDuration(tc.in); got != tc.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// renderText joins a transcript's rendered lines for assertions.
func renderText(tr *Transcript, width int) string {
	var b strings.Builder
	for _, l := range tr.Lines(width) {
		b.WriteString(l.Text)
		b.WriteString("\n")
	}
	return b.String()
}
