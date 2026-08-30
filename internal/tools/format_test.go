package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/execution"
)

func TestFormatToolPrefersMakeTarget(t *testing.T) {
	root := execWriteFixture(t, map[string]string{
		"Makefile": "fmt:\n\tgofmt -w .\n",
		"go.mod":   "module example.com/x\n",
	})
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	fake := &execFakeExecutor{}
	res, err := NewFormatTool(fake, ws).Execute(context.Background(), execTestCall(t, "format", execTaskArgs{}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := fake.last(t).Command; got != "make fmt" {
		t.Errorf("command = %q, want make fmt", got)
	}
	if !strings.Contains(res.Content, "format: PASS") {
		t.Errorf("content missing format: PASS:\n%s", res.Content)
	}
}

func TestFormatToolDetectsToolchains(t *testing.T) {
	tests := []struct {
		name        string
		files       map[string]string
		wantCommand string
	}{
		{"gofmt", map[string]string{"go.mod": "module example.com/x\n"}, "gofmt -w ."},
		{"node-prettier", map[string]string{"package.json": `{"scripts":{"format":"prettier --write ."}}`}, "npm run format"},
		{"rust-fmt", map[string]string{"Cargo.toml": "[package]\n"}, "cargo fmt"},
		{"python-ruff-format", map[string]string{"pyproject.toml": "[tool.ruff]\nline-length = 88\n"}, "ruff format ."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws, err := NewWorkspace(execWriteFixture(t, tc.files))
			if err != nil {
				t.Fatalf("NewWorkspace: %v", err)
			}
			fake := &execFakeExecutor{}
			if _, err := NewFormatTool(fake, ws).Execute(context.Background(), execTestCall(t, "format", execTaskArgs{})); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if got := fake.last(t).Command; got != tc.wantCommand {
				t.Errorf("command = %q, want %q", got, tc.wantCommand)
			}
		})
	}
}

func TestFormatToolFailureIsData(t *testing.T) {
	ws, _ := NewWorkspace(execWriteFixture(t, map[string]string{"go.mod": "module example.com/x\n"}))
	fake := &execFakeExecutor{handler: func(execution.RunRequest) (execution.RunResult, error) {
		return execution.RunResult{
			ExitCode: 1,
			Stderr:   "./main.go:5: syntax error\n",
		}, nil
	}}
	res, err := NewFormatTool(fake, ws).Execute(context.Background(), execTestCall(t, "format", execTaskArgs{}))
	if err != nil {
		t.Fatalf("a broken format must not be a Go error: %v", err)
	}
	if !res.IsError {
		t.Error("a failing format must be an error result")
	}
	if !strings.Contains(res.Content, "format: FAIL") {
		t.Errorf("content missing format: FAIL:\n%s", res.Content)
	}
}
