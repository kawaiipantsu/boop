package tools

import (
	"testing"

	"github.com/kawaiipantsu/boop/internal/execution"
)

// A tool's Display is what a watching user reads while the model works, so a
// bare "ok" wastes the one line they get. These pin the wording.
func TestExecOutcome(t *testing.T) {
	tests := []struct {
		name string
		res  execution.RunResult
		want string
	}{
		{"success with output", execution.RunResult{Stdout: "a\nb\nc\n"}, "exit 0 · 3 lines"},
		{"success one line", execution.RunResult{Stdout: "only\n"}, "exit 0 · 1 line"},
		{"success no output", execution.RunResult{}, "exit 0"},
		{"failure", execution.RunResult{ExitCode: 2}, "exit 2"},
		{"timeout beats exit code", execution.RunResult{ExitCode: 143, TimedOut: true}, "timed out"},
		{"cancelled", execution.RunResult{ExitCode: -1, Cancelled: true}, "cancelled"},
		{"signalled", execution.RunResult{ExitCode: 137, Signal: "SIGKILL"}, "killed by SIGKILL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := execOutcome(tc.res); got != tc.want {
				t.Errorf("execOutcome() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFsOutcome(t *testing.T) {
	tests := []struct {
		name string
		data any
		want string
	}{
		{"read range", ReadData{FirstLine: 1, LastLine: 42, TotalLines: 100}, "42 lines"},
		{"read whole", ReadData{TotalLines: 7}, "7 lines"},
		{"write created", WriteData{Created: true, Bytes: 2048}, "created · 2.0 KB"},
		{"write updated", WriteData{Bytes: 12}, "updated · 12 B"},
		{"edit", EditData{Replacements: 3}, "3 replacements"},
		{"edit singular", EditData{Replacements: 1}, "1 replacement"},
		{"list", ListData{Directories: 2, Files: 5}, "2 dirs, 5 files"},
		{"find", FindData{Matches: []string{"a", "b"}}, "2 matches"},
		{"find truncated", FindData{Matches: []string{"a"}, Truncated: true}, "1 match+"},
		{"search", SearchData{Matches: make([]SearchMatch, 4), FilesMatched: 2}, "4 matches in 2 files"},
		{"unknown payload", struct{}{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := fsOutcome(tc.data); got != tc.want {
				t.Errorf("fsOutcome() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPlural(t *testing.T) {
	for _, tc := range []struct {
		n    int
		noun string
		want string
	}{
		{0, "result", "0 results"},
		{1, "result", "1 result"},
		{2, "result", "2 results"},
		// The -es cases: "2 matchs" would look like a bug in everything else.
		{2, "match", "2 matches"},
		{1, "match", "1 match"},
		{3, "box", "3 boxes"},
		{2, "dir", "2 dirs"},
	} {
		if got := plural(tc.n, tc.noun); got != tc.want {
			t.Errorf("plural(%d, %q) = %q, want %q", tc.n, tc.noun, got, tc.want)
		}
	}
}
