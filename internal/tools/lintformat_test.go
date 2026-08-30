package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/execution"
	"github.com/kawaiipantsu/boop/internal/permissions"
)

func TestExecDetectLintAndFormat(t *testing.T) {
	tests := []struct {
		name          string
		files         map[string]string
		kind          execTaskKind
		checkOnly     bool
		wantFound     bool
		wantEcosystem string
		wantCommand   string
	}{
		// Go
		{"go vet without golangci config", map[string]string{"go.mod": "module x\n"}, execTaskLint, false, true, "go", "go vet ./..."},
		{"golangci-lint when configured", map[string]string{"go.mod": "module x\n", ".golangci.yml": "run:\n"}, execTaskLint, false, true, "golangci-lint", "golangci-lint run"},
		{"gofmt write", map[string]string{"go.mod": "module x\n"}, execTaskFormat, false, true, "gofmt", "gofmt -w ."},
		{"gofmt check", map[string]string{"go.mod": "module x\n"}, execTaskFormat, true, true, "gofmt", "gofmt -l ."},

		// Make targets win
		{"make lint target", map[string]string{"Makefile": ".PHONY: lint\nlint:\n\tgolangci-lint run\n", "go.mod": "module x\n"}, execTaskLint, false, true, "make", "make lint"},
		{"make fmt target for format", map[string]string{"Makefile": ".PHONY: fmt\nfmt:\n\tgofmt -w .\n", "go.mod": "module x\n"}, execTaskFormat, false, true, "make", "make fmt"},
		{"make fmt is skipped for a check", map[string]string{"Makefile": ".PHONY: fmt\nfmt:\n\tgofmt -w .\n", "go.mod": "module x\n"}, execTaskFormat, true, true, "gofmt", "gofmt -l ."},
		{"make fmt-check target for a check", map[string]string{"Makefile": ".PHONY: fmt-check\nfmt-check:\n\tgofmt -l .\n", "go.mod": "module x\n"}, execTaskFormat, true, true, "make", "make fmt-check"},

		// Cargo
		{"cargo clippy", map[string]string{"Cargo.toml": "[package]\n"}, execTaskLint, false, true, "cargo", "cargo clippy"},
		{"cargo fmt write", map[string]string{"Cargo.toml": "[package]\n"}, execTaskFormat, false, true, "cargo", "cargo fmt"},
		{"cargo fmt check", map[string]string{"Cargo.toml": "[package]\n"}, execTaskFormat, true, true, "cargo", "cargo fmt --check"},

		// Node
		{"npm lint script", map[string]string{"package.json": `{"scripts":{"lint":"eslint ."}}`}, execTaskLint, false, true, "npm", "npm run lint"},
		{"eslint config without a script", map[string]string{"package.json": `{"scripts":{}}`, "eslint.config.js": "export default []\n"}, execTaskLint, false, true, "npm", "npx eslint ."},
		{"pnpm format script", map[string]string{"package.json": `{"scripts":{"format":"prettier -w ."}}`, "pnpm-lock.yaml": "lockfileVersion: 6.0\n"}, execTaskFormat, false, true, "pnpm", "pnpm run format"},
		{"format:check script preferred for a check", map[string]string{"package.json": `{"scripts":{"format":"prettier -w .","format:check":"prettier -c ."}}`}, execTaskFormat, true, true, "npm", "npm run format:check"},
		{"prettier check fallback", map[string]string{"package.json": `{"scripts":{"format":"prettier -w ."},"devDependencies":{"prettier":"^3"}}`}, execTaskFormat, true, true, "npm", "npx prettier --check ."},
		{"no format check without prettier", map[string]string{"package.json": `{"scripts":{"format":"biome format"}}`}, execTaskFormat, true, false, "", ""},

		// Python
		{"ruff lint", map[string]string{"pyproject.toml": "[tool.ruff]\n"}, execTaskLint, false, true, "ruff", "ruff check ."},
		{"ruff format check", map[string]string{"pyproject.toml": "[tool.ruff]\n"}, execTaskFormat, true, true, "ruff", "ruff format --check ."},
		{"black format write", map[string]string{"pyproject.toml": "[tool.black]\n"}, execTaskFormat, false, true, "black", "black ."},
		{"flake8 lint", map[string]string{"setup.py": "", ".flake8": "[flake8]\n"}, execTaskLint, false, true, "flake8", "flake8"},
		{"python with no formatter configured", map[string]string{"requirements.txt": "requests\n"}, execTaskFormat, false, false, "", ""},

		// PHP
		{"phpstan lint", map[string]string{"composer.json": "{}", "phpstan.neon": "parameters:\n"}, execTaskLint, false, true, "phpstan", "phpstan analyse"},
		{"php-cs-fixer format check", map[string]string{"composer.json": "{}", ".php-cs-fixer.dist.php": "<?php\n"}, execTaskFormat, true, true, "php-cs-fixer", "php-cs-fixer fix --dry-run --diff"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := execWriteFixture(t, tc.files)
			det, ok := execDetectTask(root, tc.kind, tc.checkOnly)
			if ok != tc.wantFound {
				t.Fatalf("found = %v (%+v), want %v", ok, det, tc.wantFound)
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

func TestLintToolPermissionIsRead(t *testing.T) {
	ws, _ := NewWorkspace(execWriteFixture(t, map[string]string{"go.mod": "module x\n"}))
	action, err := NewLintTool(&execFakeExecutor{}, ws).Permission(execTestCall(t, "lint", execTaskArgs{}))
	if err != nil {
		t.Fatalf("Permission: %v", err)
	}
	if action.Category != permissions.CatFilesystemRead {
		t.Errorf("category = %q, want filesystem.read for a detected linter", action.Category)
	}
	if action.Risk != permissions.RiskLow {
		t.Errorf("risk = %q, want low", action.Risk)
	}
	if action.Detail != "go vet ./..." {
		t.Errorf("detail = %q", action.Detail)
	}
}

func TestFormatToolPermissionReadVsWrite(t *testing.T) {
	ws, _ := NewWorkspace(execWriteFixture(t, map[string]string{"go.mod": "module x\n"}))
	tool := NewFormatTool(&execFakeExecutor{}, ws)

	write, err := tool.Permission(execTestCall(t, "format", execTaskArgs{}))
	if err != nil {
		t.Fatalf("Permission: %v", err)
	}
	if write.Category != permissions.CatFilesystemWrite {
		t.Errorf("unchecked format category = %q, want filesystem.write", write.Category)
	}

	check, err := tool.Permission(execTestCall(t, "format", execTaskArgs{Check: true}))
	if err != nil {
		t.Fatalf("Permission: %v", err)
	}
	if check.Category != permissions.CatFilesystemRead {
		t.Errorf("format --check category = %q, want filesystem.read", check.Category)
	}
	if !strings.HasPrefix(check.Summary, "Check ") {
		t.Errorf("summary = %q, want it to say it is a check", check.Summary)
	}
}

func TestFormatToolExplicitCommandIsClassified(t *testing.T) {
	ws, _ := NewWorkspace(execWriteFixture(t, map[string]string{"go.mod": "module x\n"}))
	tool := NewFormatTool(&execFakeExecutor{}, ws)
	tool.Classify = func(string) permissions.Classification {
		return permissions.Classification{Category: permissions.CatShellExecute, Risk: permissions.RiskCritical}
	}
	action, err := tool.Permission(execTestCall(t, "format", execTaskArgs{Command: "rm -rf /"}))
	if err != nil {
		t.Fatalf("Permission: %v", err)
	}
	if action.Risk != permissions.RiskCritical || action.Category != permissions.CatShellExecute {
		t.Errorf("an explicit command must go through the classifier, got %+v", action)
	}
}

func TestFormatCheckFailsWhenFilesAreUnformatted(t *testing.T) {
	ws, _ := NewWorkspace(execWriteFixture(t, map[string]string{"go.mod": "module x\n"}))
	// gofmt -l exits 0 and lists the files that need formatting.
	fake := &execFakeExecutor{handler: func(execution.RunRequest) (execution.RunResult, error) {
		return execution.RunResult{ExitCode: 0, Stdout: "main.go\ninternal/x/y.go\n"}, nil
	}}
	res, err := NewFormatTool(fake, ws).Execute(context.Background(), execTestCall(t, "format", execTaskArgs{Check: true}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Error("a check that lists unformatted files must be an error result")
	}
	for _, want := range []string{"NEEDS FORMATTING", "main.go", "internal/x/y.go"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("content missing %q:\n%s", want, res.Content)
		}
	}
}

func TestFormatCheckPassesWhenClean(t *testing.T) {
	ws, _ := NewWorkspace(execWriteFixture(t, map[string]string{"go.mod": "module x\n"}))
	fake := &execFakeExecutor{} // exit 0, no output
	res, err := NewFormatTool(fake, ws).Execute(context.Background(), execTestCall(t, "format", execTaskArgs{Check: true}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Errorf("a clean check must not be an error:\n%s", res.Content)
	}
	if got := fake.last(t).Command; got != "gofmt -l ." {
		t.Errorf("command = %q, want the check form", got)
	}
}

func TestLintToolRunsDetectedCommand(t *testing.T) {
	ws, _ := NewWorkspace(execWriteFixture(t, map[string]string{
		"go.mod":        "module x\n",
		".golangci.yml": "run:\n",
	}))
	fake := &execFakeExecutor{}
	res, err := NewLintTool(fake, ws).Execute(context.Background(), execTestCall(t, "lint", execTaskArgs{}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := fake.last(t).Command; got != "golangci-lint run" {
		t.Errorf("command = %q", got)
	}
	if !strings.Contains(res.Content, "lint: PASS") {
		t.Errorf("content missing verdict:\n%s", res.Content)
	}
}

func TestLintAndFormatImplementTool(t *testing.T) {
	for _, tool := range []Tool{
		NewLintTool(&execFakeExecutor{}, execTestWorkspace(t)),
		NewFormatTool(&execFakeExecutor{}, execTestWorkspace(t)),
	} {
		if tool.Name() == "" || tool.Description() == "" {
			t.Errorf("%T must have a name and description", tool)
		}
		if _, ok := tool.Schema()["type"]; !ok {
			t.Errorf("%T schema must be an object", tool)
		}
	}
}
