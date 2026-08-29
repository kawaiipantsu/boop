package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kawaiipantsu/boop/internal/execution"
	"github.com/kawaiipantsu/boop/internal/permissions"
)

func TestBuildToolPrefersMakeTarget(t *testing.T) {
	root := execWriteFixture(t, map[string]string{
		"Makefile": execMakefileFixture,
		"go.mod":   "module example.com/x\n",
	})
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	fake := &execFakeExecutor{}
	res, err := NewBuildTool(fake, ws).Execute(context.Background(), execTestCall(t, "build", execTaskArgs{}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := fake.last(t).Command; got != "make build" {
		t.Errorf("command = %q, want the project's own make target", got)
	}
	for _, want := range []string{"build: PASS", "runner: make (Makefile defines a build target)"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("content missing %q:\n%s", want, res.Content)
		}
	}
}

func TestBuildToolDetectsToolchains(t *testing.T) {
	tests := []struct {
		name        string
		files       map[string]string
		wantCommand string
	}{
		{"go", map[string]string{"go.mod": "module example.com/x\n"}, "go build ./..."},
		{"node", map[string]string{"package.json": `{"scripts":{"build":"vite build"}}`}, "npm run build"},
		{"rust", map[string]string{"Cargo.toml": "[package]\n"}, "cargo build"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws, err := NewWorkspace(execWriteFixture(t, tc.files))
			if err != nil {
				t.Fatalf("NewWorkspace: %v", err)
			}
			fake := &execFakeExecutor{}
			if _, err := NewBuildTool(fake, ws).Execute(context.Background(), execTestCall(t, "build", execTaskArgs{})); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if got := fake.last(t).Command; got != tc.wantCommand {
				t.Errorf("command = %q, want %q", got, tc.wantCommand)
			}
		})
	}
}

func TestBuildToolCompilationFailureIsData(t *testing.T) {
	ws, _ := NewWorkspace(execWriteFixture(t, map[string]string{"go.mod": "module example.com/x\n"}))
	fake := &execFakeExecutor{handler: func(execution.RunRequest) (execution.RunResult, error) {
		return execution.RunResult{
			ExitCode: 2,
			Stderr:   "./main.go:7:2: undefined: helper\n",
		}, nil
	}}
	res, err := NewBuildTool(fake, ws).Execute(context.Background(), execTestCall(t, "build", execTaskArgs{}))
	if err != nil {
		t.Fatalf("a broken build must not be a Go error: %v", err)
	}
	if !res.IsError {
		t.Error("a failing build must be an error result")
	}
	for _, want := range []string{"build: FAIL", "undefined: helper", "exit_code: 2"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("content missing %q:\n%s", want, res.Content)
		}
	}
	if _, ok := res.Data.(execution.RunResult); !ok {
		t.Errorf("Data = %T, want execution.RunResult", res.Data)
	}
}

func TestBuildToolOverrideAndTimeout(t *testing.T) {
	ws, _ := NewWorkspace(execWriteFixture(t, map[string]string{"go.mod": "module example.com/x\n"}))
	fake := &execFakeExecutor{}
	tool := NewBuildTool(fake, ws)
	if _, err := tool.Execute(context.Background(), execTestCall(t, "build", execTaskArgs{
		Command:        "go build -tags integration ./cmd/...",
		TimeoutSeconds: 45,
	})); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	req := fake.last(t)
	if req.Command != "go build -tags integration ./cmd/..." {
		t.Errorf("command = %q, want the override", req.Command)
	}
	if req.Timeout != 45*time.Second {
		t.Errorf("timeout = %s, want 45s", req.Timeout)
	}
}

func TestBuildToolNoDetection(t *testing.T) {
	fake := &execFakeExecutor{}
	res, err := NewBuildTool(fake, execTestWorkspace(t)).Execute(context.Background(), execTestCall(t, "build", execTaskArgs{}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "no build command could be detected") {
		t.Errorf("want a detection failure result, got %+v", res)
	}
	if len(fake.calls()) != 0 {
		t.Error("nothing may run when detection fails")
	}
}

func TestBuildToolPermission(t *testing.T) {
	ws, _ := NewWorkspace(execWriteFixture(t, map[string]string{"go.mod": "module example.com/x\n"}))
	tool := NewBuildTool(&execFakeExecutor{}, ws)
	action, err := tool.Permission(execTestCall(t, "build", execTaskArgs{}))
	if err != nil {
		t.Fatalf("Permission: %v", err)
	}
	if action.Category != permissions.CatShellExecute {
		t.Errorf("category = %q", action.Category)
	}
	if action.Detail != "go build ./..." {
		t.Errorf("detail = %q, want the detected command", action.Detail)
	}
	if action.Tool != "build" {
		t.Errorf("tool = %q", action.Tool)
	}

	tool.Classify = func(string) permissions.Risk { return permissions.RiskLow }
	action, err = tool.Permission(execTestCall(t, "build", execTaskArgs{}))
	if err != nil {
		t.Fatalf("Permission: %v", err)
	}
	if action.Risk != permissions.RiskLow {
		t.Errorf("risk = %q, want the injected classifier's verdict", action.Risk)
	}
}

func TestBuildToolImplementsTool(t *testing.T) {
	var tool Tool = NewBuildTool(&execFakeExecutor{}, execTestWorkspace(t))
	if tool.Name() != "build" {
		t.Errorf("name = %q", tool.Name())
	}
	if tool.Description() == "" {
		t.Error("description must not be empty")
	}
}
