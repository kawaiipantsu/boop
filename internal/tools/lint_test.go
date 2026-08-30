package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/execution"
)

func TestLintToolPrefersMakeTarget(t *testing.T) {
	root := execWriteFixture(t, map[string]string{
		"Makefile": "lint:\n\tgolangci-lint run\n",
		"go.mod":   "module example.com/x\n",
	})
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	fake := &execFakeExecutor{}
	res, err := NewLintTool(fake, ws).Execute(context.Background(), execTestCall(t, "lint", execTaskArgs{}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := fake.last(t).Command; got != "make lint" {
		t.Errorf("command = %q, want make lint", got)
	}
	if !strings.Contains(res.Content, "lint: PASS") {
		t.Errorf("content missing lint: PASS:\n%s", res.Content)
	}
}

func TestLintToolDetectsToolchains(t *testing.T) {
	tests := []struct {
		name        string
		files       map[string]string
		wantCommand string
	}{
		{"go-vet", map[string]string{"go.mod": "module example.com/x\n"}, "go vet ./..."},
		{"golangci-lint", map[string]string{"go.mod": "module example.com/x\n", ".golangci.yml": "version: 2\n"}, "golangci-lint run"},
		{"node-eslint", map[string]string{"package.json": `{"scripts":{"lint":"eslint ."}}`}, "npm run lint"},
		{"rust-clippy", map[string]string{"Cargo.toml": "[package]\n"}, "cargo clippy"},
		{"python-ruff", map[string]string{"pyproject.toml": "[tool.ruff]\nline-length = 88\n"}, "ruff check ."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws, err := NewWorkspace(execWriteFixture(t, tc.files))
			if err != nil {
				t.Fatalf("NewWorkspace: %v", err)
			}
			fake := &execFakeExecutor{}
			if _, err := NewLintTool(fake, ws).Execute(context.Background(), execTestCall(t, "lint", execTaskArgs{})); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if got := fake.last(t).Command; got != tc.wantCommand {
				t.Errorf("command = %q, want %q", got, tc.wantCommand)
			}
		})
	}
}

func TestLintToolFailureIsData(t *testing.T) {
	ws, _ := NewWorkspace(execWriteFixture(t, map[string]string{"go.mod": "module example.com/x\n"}))
	fake := &execFakeExecutor{handler: func(execution.RunRequest) (execution.RunResult, error) {
		return execution.RunResult{
			ExitCode: 1,
			Stderr:   "./main.go:10:1: unreachable code\n",
		}, nil
	}}
	res, err := NewLintTool(fake, ws).Execute(context.Background(), execTestCall(t, "lint", execTaskArgs{}))
	if err != nil {
		t.Fatalf("a broken lint must not be a Go error: %v", err)
	}
	if !res.IsError {
		t.Error("a failing lint must be an error result")
	}
	for _, want := range []string{"lint: FAIL", "unreachable code", "exit_code: 1"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("content missing %q:\n%s", want, res.Content)
		}
	}
}
