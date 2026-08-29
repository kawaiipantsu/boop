package main

import (
	"bytes"
	"io"
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

// Modes that are still stubs must say which milestone they belong to rather
// than failing opaquely.
//
// Only --gui remains. --no-tui and --web are implemented, and asserting they
// are not would start a real server inside the test and hang it, which is how
// this list going stale announces itself.
func TestUnimplementedModesReportTheirMilestone(t *testing.T) {
	for _, tc := range []struct{ flag, want string }{
		{"--gui", "milestone 13"},
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

func TestBarePromptJoinsAllArguments(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"single word", []string{"help"}, "help"},
		{"unquoted sentence", []string{"serve", "this", "folder", "up", "via", "http"},
			"serve this folder up via http"},
		{"with punctuation", []string{"build", "me", "a", "website", "in", "html,css,js"},
			"build me a website in html,css,js"},
		{"with a path", []string{"i", "have", "added", "sdc", "device,", "create", "lvm",
			"and", "mount", "on", "/mnt/storage"},
			"i have added sdc device, create lvm and mount on /mnt/storage"},
		{"flags then prompt", []string{"--mode", "auto", "fix", "the", "failing", "tests"},
			"fix the failing tests"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parse(tc.args, io.Discard)
			if err != nil {
				t.Fatalf("parse(%q) = %v, want nil", tc.args, err)
			}
			if got.prompt != tc.want {
				t.Errorf("prompt = %q, want %q", got.prompt, tc.want)
			}
		})
	}
}

func TestPromptFlagAndBareArgsConflict(t *testing.T) {
	_, err := parse([]string{"--prompt", "one", "and", "two"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("parse() = %v, want a conflict error", err)
	}
}

func TestModeFlagIsParsedBeforeBarePrompt(t *testing.T) {
	got, err := parse([]string{"--mode", "auto", "deploy", "the", "thing"}, io.Discard)
	if err != nil {
		t.Fatalf("parse() = %v, want nil", err)
	}
	if got.mode != "auto" {
		t.Errorf("mode = %q, want %q", got.mode, "auto")
	}
	if got.prompt != "deploy the thing" {
		t.Errorf("prompt = %q, want %q", got.prompt, "deploy the thing")
	}
}
