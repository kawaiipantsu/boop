package tools

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/permissions"
)

func TestFindToolMatching(t *testing.T) {
	ws := fsTestTreeWorkspace(t)
	tool := NewFindTool(ws)

	tests := []struct {
		name    string
		args    map[string]any
		want    []string
		notWant []string
	}{
		{
			name:    "base name glob",
			args:    map[string]any{"pattern": "*.go"},
			want:    []string{"cmd/boop/main.go", "internal/app/app.go", "internal/app/app_test.go", "internal/tools/tools.go"},
			notWant: []string{"vendor/dep/dep.go", "README.md"},
		},
		{
			name:    "suffix glob",
			args:    map[string]any{"pattern": "*_test.go"},
			want:    []string{"internal/app/app_test.go"},
			notWant: []string{"internal/app/app.go"},
		},
		{
			name:    "exact name",
			args:    map[string]any{"pattern": "Makefile"},
			want:    []string{"Makefile"},
			notWant: []string{"README.md"},
		},
		{
			name:    "path glob with doublestar",
			args:    map[string]any{"pattern": "internal/**/*.go"},
			want:    []string{"internal/app/app.go", "internal/tools/tools.go"},
			notWant: []string{"cmd/boop/main.go"},
		},
		{
			name:    "path glob single segment",
			args:    map[string]any{"pattern": "cmd/boop/main.go"},
			want:    []string{"cmd/boop/main.go"},
			notWant: []string{"internal/app/app.go"},
		},
		{
			name:    "scoped to a subdirectory",
			args:    map[string]any{"pattern": "*.go", "path": "cmd"},
			want:    []string{"cmd/boop/main.go"},
			notWant: []string{"internal/app/app.go"},
		},
		{
			name:    "directories only",
			args:    map[string]any{"pattern": "app", "type": "dir"},
			want:    []string{"internal/app"},
			notWant: []string{"internal/app/app.go"},
		},
		{
			name:    "files only",
			args:    map[string]any{"pattern": "*", "type": "file", "limit": 100},
			want:    []string{"README.md"},
			notWant: []string{"internal\n"},
		},
		{
			name:    "include_ignored reaches vendored code",
			args:    map[string]any{"pattern": "*.go", "include_ignored": true},
			want:    []string{"vendor/dep/dep.go"},
			notWant: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := fsTestExec(t, tool, tc.args)
			if res.IsError {
				t.Fatalf("unexpected error: %s", res.Content)
			}
			data, ok := res.Data.(FindData)
			if !ok {
				t.Fatalf("Data is %T, want FindData", res.Data)
			}
			joined := strings.Join(data.Matches, "\n")
			for _, want := range tc.want {
				if !fsTestHasMatch(data.Matches, want) {
					t.Errorf("matches %v do not include %q", data.Matches, want)
				}
			}
			for _, bad := range tc.notWant {
				if fsTestHasMatch(data.Matches, strings.TrimSuffix(bad, "\n")) {
					t.Errorf("matches %v unexpectedly include %q", data.Matches, bad)
				}
			}
			if !strings.Contains(res.Content, joined) {
				t.Errorf("content does not render the matches:\n%s", res.Content)
			}
		})
	}
}

func fsTestHasMatch(matches []string, want string) bool {
	for _, m := range matches {
		if m == want {
			return true
		}
	}
	return false
}

func TestFindToolNoMatches(t *testing.T) {
	ws := fsTestTreeWorkspace(t)
	res := fsTestExec(t, NewFindTool(ws), map[string]any{"pattern": "*.rs"})
	if res.IsError {
		t.Fatalf("an empty result set is not an error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "No files matching") {
		t.Errorf("content = %q", res.Content)
	}
	if data := res.Data.(FindData); len(data.Matches) != 0 || data.Scanned == 0 {
		t.Errorf("FindData = %+v, want no matches but a non-zero scan count", data)
	}
}

func TestFindToolLimit(t *testing.T) {
	files := make(map[string]string, 30)
	for i := 0; i < 30; i++ {
		files[fmt.Sprintf("pkg/f%02d.go", i)] = "package pkg\n"
	}
	ws := fsTestWorkspace(t, files)
	res := fsTestExec(t, NewFindTool(ws), map[string]any{"pattern": "*.go", "limit": 5})
	data := res.Data.(FindData)
	if len(data.Matches) != 5 {
		t.Errorf("returned %d matches, want the limit of 5", len(data.Matches))
	}
	if !data.Truncated {
		t.Error("FindData.Truncated = false, want true")
	}
	if !strings.Contains(res.Content, "[truncated at 5 matches") {
		t.Errorf("content does not announce truncation:\n%s", res.Content)
	}
}

func TestFindToolErrors(t *testing.T) {
	ws, _ := fsTestEscapeWorkspace(t)
	tool := NewFindTool(ws)

	tests := []struct {
		name     string
		args     map[string]any
		contains string
	}{
		{"missing pattern", map[string]any{}, "required"},
		{"invalid pattern", map[string]any{"pattern": "[a-"}, "invalid pattern"},
		{"invalid type", map[string]any{"pattern": "*", "type": "socket"}, "invalid type"},
		{"missing directory", map[string]any{"pattern": "*", "path": "nope"}, "directory not found"},
		{"file as search root", map[string]any{"pattern": "*", "path": "inside.txt"}, "is a file, not a directory"},
		{"traversal", map[string]any{"pattern": "*", "path": "../.."}, "escapes the workspace"},
		{"escaping symlink", map[string]any{"pattern": "*", "path": "escape"}, "escapes the workspace"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := fsTestExec(t, tool, tc.args)
			if !res.IsError {
				t.Fatalf("expected an error result, got %q", res.Content)
			}
			if !strings.Contains(res.Content, tc.contains) {
				t.Errorf("content %q does not contain %q", res.Content, tc.contains)
			}
		})
	}
}

func TestFindToolDoesNotFollowEscapingSymlink(t *testing.T) {
	ws, _ := fsTestEscapeWorkspace(t)
	res := fsTestExec(t, NewFindTool(ws), map[string]any{"pattern": "*.txt"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if strings.Contains(res.Content, "secret.txt") {
		t.Errorf("find followed a symlink out of the workspace:\n%s", res.Content)
	}
}

func TestFindToolPermission(t *testing.T) {
	ws := fsTestTreeWorkspace(t)
	action, err := NewFindTool(ws).Permission(fsTestCall(t, "find", map[string]any{"pattern": "*.go", "path": "internal"}))
	if err != nil {
		t.Fatalf("Permission: %v", err)
	}
	if action.Category != permissions.CatFilesystemRead || action.Risk != permissions.RiskLow {
		t.Errorf("action = %+v, want a low-risk filesystem read", action)
	}
	if action.Tool != "find" {
		t.Errorf("Tool = %q, want find", action.Tool)
	}
	if action.Summary != `Find files matching "*.go" under internal` {
		t.Errorf("Summary = %q", action.Summary)
	}
	if len(action.Paths) != 1 {
		t.Errorf("Paths = %v, want exactly one", action.Paths)
	}
}

func TestFsGlobMatch(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"*.go", "main.go", true},
		{"*.go", "cmd/boop/main.go", true},
		{"*.go", "main.rs", false},
		{"main.go", "cmd/main.go", true},
		{"cmd/*.go", "cmd/main.go", true},
		{"cmd/*.go", "cmd/boop/main.go", false},
		{"cmd/**/*.go", "cmd/boop/main.go", true},
		{"**/*.go", "a/b/c/x.go", true},
		{"**/*.go", "x.go", true},
		{"./cmd/*.go", "cmd/main.go", true},
		{"internal/**", "internal/app/app.go", true},
		{"internal/**", "cmd/main.go", false},
	}
	for _, tc := range tests {
		if got := fsGlobMatch(tc.pattern, tc.path); got != tc.want {
			t.Errorf("fsGlobMatch(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestFsValidateGlob(t *testing.T) {
	if err := fsValidateGlob("cmd/**/*.go"); err != nil {
		t.Errorf("valid pattern rejected: %v", err)
	}
	if err := fsValidateGlob("[a-"); err == nil {
		t.Error("malformed pattern accepted")
	}
}

func TestFindToolSchemaAndDescription(t *testing.T) {
	tool := NewFindTool(fsTestWorkspace(t, nil))
	if tool.Name() != "find" {
		t.Errorf("Name = %q", tool.Name())
	}
	if !strings.Contains(tool.Description(), "**") {
		t.Errorf("Description does not explain doublestar patterns: %q", tool.Description())
	}
	schema := tool.Schema()
	props := schema["properties"].(map[string]any)
	for _, key := range []string{"pattern", "path", "type", "limit", "include_ignored"} {
		if _, ok := props[key]; !ok {
			t.Errorf("schema is missing property %q", key)
		}
	}
	if req := schema["required"].([]string); len(req) != 1 || req[0] != "pattern" {
		t.Errorf("required = %v, want [pattern]", req)
	}
}
