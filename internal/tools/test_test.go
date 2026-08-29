package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/boop-dev/boop/internal/execution"
	"github.com/boop-dev/boop/internal/permissions"
)

// execWriteFixture creates a project fixture and returns its root.
func execWriteFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return root
}

const execMakefileFixture = `.PHONY: help test build fmt

help:
	@echo help

test:
	go test ./... -race

build:
	go build -o bin/app ./cmd/app
`

func TestExecDetectTask(t *testing.T) {
	tests := []struct {
		name          string
		files         map[string]string
		kind          execTaskKind
		wantFound     bool
		wantEcosystem string
		wantCommand   string
	}{
		{
			name:          "go module tests",
			files:         map[string]string{"go.mod": "module example.com/x\n"},
			kind:          execTaskTest,
			wantFound:     true,
			wantEcosystem: "go",
			wantCommand:   "go test ./...",
		},
		{
			name:          "go module build",
			files:         map[string]string{"go.mod": "module example.com/x\n"},
			kind:          execTaskBuild,
			wantFound:     true,
			wantEcosystem: "go",
			wantCommand:   "go build ./...",
		},
		{
			name:          "node test script",
			files:         map[string]string{"package.json": `{"name":"x","scripts":{"test":"jest","build":"tsc"}}`},
			kind:          execTaskTest,
			wantFound:     true,
			wantEcosystem: "npm",
			wantCommand:   "npm test",
		},
		{
			name:          "node build script",
			files:         map[string]string{"package.json": `{"name":"x","scripts":{"build":"tsc"}}`},
			kind:          execTaskBuild,
			wantFound:     true,
			wantEcosystem: "npm",
			wantCommand:   "npm run build",
		},
		{
			name:      "node without the script is not detected",
			files:     map[string]string{"package.json": `{"name":"x","scripts":{"lint":"eslint ."}}`},
			kind:      execTaskTest,
			wantFound: false,
		},
		{
			name: "pnpm lockfile selects pnpm",
			files: map[string]string{
				"package.json":    `{"scripts":{"test":"vitest"}}`,
				"pnpm-lock.yaml":  "lockfileVersion: 6.0\n",
				"package-lock.js": "",
			},
			kind:          execTaskTest,
			wantFound:     true,
			wantEcosystem: "pnpm",
			wantCommand:   "pnpm run test",
		},
		{
			name: "make wins over the toolchain",
			files: map[string]string{
				"Makefile": execMakefileFixture,
				"go.mod":   "module example.com/x\n",
			},
			kind:          execTaskTest,
			wantFound:     true,
			wantEcosystem: "make",
			wantCommand:   "make test",
		},
		{
			name: "make build target wins",
			files: map[string]string{
				"Makefile":     execMakefileFixture,
				"package.json": `{"scripts":{"build":"tsc"}}`,
			},
			kind:          execTaskBuild,
			wantFound:     true,
			wantEcosystem: "make",
			wantCommand:   "make build",
		},
		{
			name: "makefile without the target falls back",
			files: map[string]string{
				"Makefile": ".PHONY: lint\nlint:\n\tgolangci-lint run\n",
				"go.mod":   "module example.com/x\n",
			},
			kind:          execTaskTest,
			wantFound:     true,
			wantEcosystem: "go",
			wantCommand:   "go test ./...",
		},
		{
			name:          "lowercase makefile is honoured",
			files:         map[string]string{"makefile": execMakefileFixture},
			kind:          execTaskTest,
			wantFound:     true,
			wantEcosystem: "make",
			wantCommand:   "make test",
		},
		{
			name:          "rust",
			files:         map[string]string{"Cargo.toml": "[package]\nname = \"x\"\n"},
			kind:          execTaskTest,
			wantFound:     true,
			wantEcosystem: "cargo",
			wantCommand:   "cargo test",
		},
		{
			name:          "python tests",
			files:         map[string]string{"pyproject.toml": "[project]\nname = \"x\"\n"},
			kind:          execTaskTest,
			wantFound:     true,
			wantEcosystem: "python",
			wantCommand:   "pytest",
		},
		{
			name:          "python build",
			files:         map[string]string{"pyproject.toml": "[project]\nname = \"x\"\n"},
			kind:          execTaskBuild,
			wantFound:     true,
			wantEcosystem: "python",
			wantCommand:   "python -m build",
		},
		{
			name:      "requirements-only project has no build",
			files:     map[string]string{"requirements.txt": "pytest\n"},
			kind:      execTaskBuild,
			wantFound: false,
		},
		{
			name:      "unrecognised project",
			files:     map[string]string{"README.md": "# hello\n"},
			kind:      execTaskTest,
			wantFound: false,
		},
		{
			name:          "malformed package.json is ignored in favour of the next marker",
			files:         map[string]string{"package.json": "{not json", "Cargo.toml": "[package]\n"},
			kind:          execTaskTest,
			wantFound:     true,
			wantEcosystem: "cargo",
			wantCommand:   "cargo test",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := execWriteFixture(t, tc.files)
			det, ok := execDetectTask(root, tc.kind)
			if ok != tc.wantFound {
				t.Fatalf("detected = %v (%+v), want %v", ok, det, tc.wantFound)
			}
			if !tc.wantFound {
				return
			}
			if det.Ecosystem != tc.wantEcosystem {
				t.Errorf("ecosystem = %q, want %q", det.Ecosystem, tc.wantEcosystem)
			}
			if det.Command != tc.wantCommand {
				t.Errorf("command = %q, want %q", det.Command, tc.wantCommand)
			}
			if det.Reason == "" {
				t.Error("detection must explain itself")
			}
		})
	}
}

func TestExecDetectTaskEmptyRoot(t *testing.T) {
	if _, ok := execDetectTask("", execTaskTest); ok {
		t.Error("an empty root must not detect anything")
	}
}

func TestExecMakeTargets(t *testing.T) {
	root := execWriteFixture(t, map[string]string{"Makefile": `# comment
.PHONY: test build

BINARY := boop
GOFLAGS = -trimpath

test:
	go test ./...

build: $(BINARY)
	go build

%.o: %.c
	cc -c $<

.DEFAULT_GOAL := help

coverage: test
	go tool cover
`})
	targets := execMakeTargets(filepath.Join(root, "Makefile"))
	for _, want := range []string{"test", "build", "coverage"} {
		if !targets[want] {
			t.Errorf("target %q not detected in %v", want, targets)
		}
	}
	for _, unwanted := range []string{"BINARY", "GOFLAGS", "%.o", ".DEFAULT_GOAL", ".PHONY"} {
		if targets[unwanted] {
			t.Errorf("%q must not be treated as a target", unwanted)
		}
	}
}

func TestExecMakeTargetsMissingFile(t *testing.T) {
	if targets := execMakeTargets(filepath.Join(t.TempDir(), "Makefile")); len(targets) != 0 {
		t.Errorf("want no targets for a missing makefile, got %v", targets)
	}
}

func TestTestToolExecuteDetectsAndRuns(t *testing.T) {
	root := execWriteFixture(t, map[string]string{"go.mod": "module example.com/x\n"})
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	fake := &execFakeExecutor{handler: func(req execution.RunRequest) (execution.RunResult, error) {
		return execution.RunResult{Stdout: "ok  \texample.com/x\t0.1s\n", Duration: 100 * time.Millisecond}, nil
	}}
	tool := NewTestTool(fake, ws)

	res, err := tool.Execute(context.Background(), execTestCall(t, "test", execTaskArgs{}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	req := fake.last(t)
	if req.Command != "go test ./..." {
		t.Errorf("command = %q, want the detected go command", req.Command)
	}
	if req.WorkingDir != ws.Root() {
		t.Errorf("working dir = %q", req.WorkingDir)
	}
	if req.Timeout != 15*time.Minute {
		t.Errorf("timeout = %s, want the test default", req.Timeout)
	}
	if res.IsError {
		t.Error("a passing suite must not be an error")
	}
	for _, want := range []string{"test: PASS", "runner: go (go.mod present)", "$ go test ./..."} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("content missing %q:\n%s", want, res.Content)
		}
	}
}

func TestTestToolFailingSuiteIsData(t *testing.T) {
	root := execWriteFixture(t, map[string]string{"go.mod": "module example.com/x\n"})
	ws, _ := NewWorkspace(root)
	fake := &execFakeExecutor{handler: func(execution.RunRequest) (execution.RunResult, error) {
		return execution.RunResult{
			ExitCode: 1,
			Stdout:   "--- FAIL: TestThing (0.00s)\n    thing_test.go:12: want 3, got 4\nFAIL\n",
		}, nil
	}}
	res, err := NewTestTool(fake, ws).Execute(context.Background(), execTestCall(t, "test", execTaskArgs{}))
	if err != nil {
		t.Fatalf("a failing suite must not be a Go error: %v", err)
	}
	if !res.IsError {
		t.Error("a failing suite must be an error result")
	}
	for _, want := range []string{"test: FAIL", "want 3, got 4", "exit_code: 1"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("content missing %q:\n%s", want, res.Content)
		}
	}
}

func TestTestToolExplicitCommandOverride(t *testing.T) {
	root := execWriteFixture(t, map[string]string{"go.mod": "module example.com/x\n"})
	ws, _ := NewWorkspace(root)
	fake := &execFakeExecutor{}
	res, err := NewTestTool(fake, ws).Execute(context.Background(), execTestCall(t, "test", execTaskArgs{
		Command: "go test ./internal/tools/ -run TestRun",
	}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := fake.last(t).Command; got != "go test ./internal/tools/ -run TestRun" {
		t.Errorf("command = %q, want the override", got)
	}
	if !strings.Contains(res.Content, "runner: explicit") {
		t.Errorf("content should record that the command was supplied:\n%s", res.Content)
	}
}

func TestTestToolNoDetection(t *testing.T) {
	ws := execTestWorkspace(t)
	fake := &execFakeExecutor{}
	res, err := NewTestTool(fake, ws).Execute(context.Background(), execTestCall(t, "test", execTaskArgs{}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Fatal("an undetectable project must produce an error result")
	}
	if !strings.Contains(res.Content, "no test command could be detected") {
		t.Errorf("content = %q", res.Content)
	}
	if !strings.Contains(res.Content, `"command"`) {
		t.Error("the message should tell the model how to recover")
	}
	if len(fake.calls()) != 0 {
		t.Error("nothing may run when detection fails")
	}
}

func TestTestToolPermission(t *testing.T) {
	root := execWriteFixture(t, map[string]string{"Makefile": execMakefileFixture})
	ws, _ := NewWorkspace(root)
	tool := NewTestTool(&execFakeExecutor{}, ws)

	action, err := tool.Permission(execTestCall(t, "test", execTaskArgs{}))
	if err != nil {
		t.Fatalf("Permission: %v", err)
	}
	if action.Category != permissions.CatShellExecute {
		t.Errorf("category = %q, want shell.execute", action.Category)
	}
	if action.Risk != permissions.RiskMedium {
		t.Errorf("risk = %q, want medium for an ordinary make test", action.Risk)
	}
	if action.Detail != "make test" {
		t.Errorf("detail = %q, want the detected command", action.Detail)
	}
	if !strings.Contains(action.Summary, "make") {
		t.Errorf("summary = %q, want it to name the runner", action.Summary)
	}
}

func TestTestToolPermissionWithoutDetection(t *testing.T) {
	tool := NewTestTool(&execFakeExecutor{}, execTestWorkspace(t))
	action, err := tool.Permission(execTestCall(t, "test", execTaskArgs{}))
	if err != nil {
		t.Fatalf("Permission: %v", err)
	}
	if action.Category != permissions.CatShellExecute || action.Risk != permissions.RiskMedium {
		t.Errorf("undetectable projects must still classify conservatively: %+v", action)
	}
}

func TestTestToolImplementsTool(t *testing.T) {
	var tool Tool = NewTestTool(&execFakeExecutor{}, execTestWorkspace(t))
	if tool.Name() != "test" {
		t.Errorf("name = %q", tool.Name())
	}
	if _, ok := tool.Schema()["properties"].(map[string]any)["command"]; !ok {
		t.Error("schema must expose the command override")
	}
}
