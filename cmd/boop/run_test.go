package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionSubcommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run([]string{"version"}, &out, &errOut); err != nil {
		t.Fatalf("run(version) = %v, want nil", err)
	}
	if !strings.HasPrefix(out.String(), "boop v") {
		t.Errorf("output = %q, want it to start with %q", out.String(), "boop v")
	}
}

func TestVersionFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run([]string{"--version"}, &out, &errOut); err != nil {
		t.Fatalf("run(--version) = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "commit:") {
		t.Errorf("output = %q, want it to include the commit", out.String())
	}
}

func TestUnimplementedModesReportTheirMilestone(t *testing.T) {
	for _, tc := range []struct{ flag, want string }{
		{"--gui", "milestone 13"},
		{"--web", "milestone 9"},
		{"--no-tui", "milestone 2"},
	} {
		var out, errOut bytes.Buffer
		err := run([]string{tc.flag}, &out, &errOut)
		if err == nil {
			t.Fatalf("run(%s) = nil, want an error", tc.flag)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("run(%s) error = %q, want it to mention %q", tc.flag, err, tc.want)
		}
	}
}

func TestBarePromptIsCapturedAsPositionalArg(t *testing.T) {
	var out, errOut bytes.Buffer
	// The TUI is not implemented, so this exercises parsing only.
	err := run([]string{"fix the failing tests"}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "TUI") {
		t.Fatalf("run(prompt) = %v, want the TUI milestone error", err)
	}
}
