package tui

import (
	"strings"
	"testing"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		ok      bool
		cmdName string
		args    []string
		rest    string
	}{
		{name: "bare", in: "/help", ok: true, cmdName: "help"},
		{name: "leading space", in: "  /help  ", ok: true, cmdName: "help"},
		{name: "upper case", in: "/HELP", ok: true, cmdName: "help"},
		{name: "one argument", in: "/model llama3", ok: true, cmdName: "model", args: []string{"llama3"}, rest: "llama3"},
		{name: "several arguments", in: "/session load abc-123", ok: true, cmdName: "session",
			args: []string{"load", "abc-123"}, rest: "load abc-123"},
		{name: "rest keeps spacing words", in: "/session save my great run", ok: true, cmdName: "session",
			args: []string{"save", "my", "great", "run"}, rest: "save my great run"},
		{name: "not a command", in: "hello", ok: false},
		{name: "empty", in: "", ok: false},
		{name: "slash alone", in: "/", ok: false},
		{name: "escaped slash", in: "//not a command", ok: false},
		{name: "path is not a command", in: "/usr/bin/env", ok: false},
		{name: "question keeps its slash", in: "what is /etc for?", ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseCommand(tc.in)
			if ok != tc.ok {
				t.Fatalf("ParseCommand(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			}
			if !ok {
				return
			}
			if got.Name != tc.cmdName {
				t.Errorf("name = %q, want %q", got.Name, tc.cmdName)
			}
			if !equalStrings(got.Args, tc.args) && !(len(got.Args) == 0 && len(tc.args) == 0) {
				t.Errorf("args = %q, want %q", got.Args, tc.args)
			}
			if got.Rest != tc.rest {
				t.Errorf("rest = %q, want %q", got.Rest, tc.rest)
			}
		})
	}
}

func TestCommandArg(t *testing.T) {
	cmd, _ := ParseCommand("/session load abc")
	if cmd.Arg(0) != "load" || cmd.Arg(1) != "abc" || cmd.Arg(2) != "" {
		t.Fatalf("args = %q", cmd.Args)
	}
}

func TestUnescapeMessage(t *testing.T) {
	tests := []struct{ in, want string }{
		{"//help", "/help"},
		{"  //help", "  /help"},
		{"/help", "/help"},
		{"hello", "hello"},
		{"a // b", "a // b"},
	}
	for _, tc := range tests {
		if got := UnescapeMessage(tc.in); got != tc.want {
			t.Errorf("UnescapeMessage(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEveryCommandInTheSpecIsAccountedFor(t *testing.T) {
	// §20 lists the initial command set. Anything on that list must either be
	// implemented or say specifically what it is waiting for.
	spec := []string{
		"help", "prep", "init", "config", "provider", "model", "models",
		"agents", "run", "permissions", "status", "stats", "tokens",
		"session", "context", "files", "tree", "search", "test", "build",
		"web", "gui", "clear", "reset", "quit", "exit", "boop",
	}
	for _, name := range spec {
		got, ok := lookupCommand(name)
		if !ok {
			t.Errorf("/%s from §20 is missing from the command table", name)
			continue
		}
		if got.Status == cmdPending && got.Blocker == "" {
			t.Errorf("/%s is pending but does not say what it is waiting for", name)
		}
		if got.Summary == "" {
			t.Errorf("/%s has no summary", name)
		}
	}
}

func TestSuggestCommand(t *testing.T) {
	tests := []struct {
		in    string
		want  string
		found bool
	}{
		{"stat", "status", true},
		{"mod", "model", true},
		{"quti", "quit", true},
		{"zzz", "", false},
	}
	for _, tc := range tests {
		got, ok := suggestCommand(tc.in)
		if ok != tc.found || (ok && got != tc.want) {
			t.Errorf("suggestCommand(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.found)
		}
	}
}

func TestHelpTextListsBothSections(t *testing.T) {
	help := helpText()
	for _, want := range []string{"/help", "/quit", "/boop", "not built yet", "Ctrl+C", "Alt+Enter"} {
		if !strings.Contains(help, want) {
			t.Errorf("help text is missing %q", want)
		}
	}
}

func TestBoopTextIsHarmless(t *testing.T) {
	if !strings.Contains(boopText(), "boop") {
		t.Fatal("the easter egg should at least say boop")
	}
}
