package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/permissions"
)

// fsTestTreeWorkspace builds a small project-shaped tree used by the listing,
// find and search tests.
func fsTestTreeWorkspace(t *testing.T) *Workspace {
	t.Helper()
	return fsTestWorkspace(t, map[string]string{
		"README.md":                    "# Project\n",
		"Makefile":                     "all:\n\tgo build ./...\n",
		"cmd/boop/main.go":             "package main\n\nfunc main() {}\n",
		"internal/app/app.go":          "package app\n\nfunc Run() error { return nil }\n",
		"internal/app/app_test.go":     "package app\n",
		"internal/tools/tools.go":      "package tools\n",
		"node_modules/left-pad/idx.js": "module.exports = 1\n",
		".git/config":                  "[core]\n",
		"vendor/dep/dep.go":            "package dep\n",
		"docs/deep/a/b/c/leaf.txt":     "leaf\n",
	})
}

func TestListToolFlatListing(t *testing.T) {
	ws := fsTestTreeWorkspace(t)
	res := fsTestExec(t, NewListTool(ws), map[string]any{"path": "."})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	data, ok := res.Data.(ListData)
	if !ok {
		t.Fatalf("Data is %T, want ListData", res.Data)
	}
	if data.MaxDepth != 1 {
		t.Errorf("MaxDepth = %d, want 1 for a non-recursive listing", data.MaxDepth)
	}
	names := map[string]ListEntry{}
	for _, e := range data.Entries {
		names[e.Path] = e
	}
	for _, want := range []string{"README.md", "Makefile", "cmd", "internal", "docs"} {
		if _, ok := names[want]; !ok {
			t.Errorf("listing is missing %q (got %v)", want, data.Entries)
		}
	}
	for _, noise := range []string{".git", "node_modules", "vendor"} {
		if _, ok := names[noise]; ok {
			t.Errorf("noise directory %q should be skipped by default", noise)
		}
	}
	if e := names["README.md"]; e.Type != "file" || e.Size != 10 {
		t.Errorf("README.md entry = %+v, want a 10 byte file", e)
	}
	if e := names["cmd"]; e.Type != "dir" {
		t.Errorf("cmd entry = %+v, want a dir", e)
	}
	if len(data.Skipped) != 3 {
		t.Errorf("Skipped = %v, want the three noise directories", data.Skipped)
	}
	if !strings.Contains(res.Content, "include_ignored=true") {
		t.Error("content does not tell the caller how to see skipped directories")
	}
	// A non-recursive listing must not descend.
	if strings.Contains(res.Content, "cmd/boop") {
		t.Errorf("non-recursive listing descended into cmd: %s", res.Content)
	}
}

func TestListToolRecursionAndDepth(t *testing.T) {
	ws := fsTestTreeWorkspace(t)
	tool := NewListTool(ws)

	t.Run("recursive default depth", func(t *testing.T) {
		res := fsTestExec(t, tool, map[string]any{"path": ".", "recursive": true})
		data := res.Data.(ListData)
		if data.MaxDepth != fsDefaultListDepth {
			t.Errorf("MaxDepth = %d, want %d", data.MaxDepth, fsDefaultListDepth)
		}
		if !strings.Contains(res.Content, "cmd/boop/main.go") {
			t.Errorf("recursive listing is missing cmd/boop/main.go:\n%s", res.Content)
		}
		// docs/deep/a/b/c/leaf.txt is deeper than the default depth.
		if strings.Contains(res.Content, "leaf.txt") {
			t.Errorf("listing exceeded the default depth cap:\n%s", res.Content)
		}
	})

	t.Run("explicit depth", func(t *testing.T) {
		res := fsTestExec(t, tool, map[string]any{"path": ".", "recursive": true, "max_depth": 1})
		if strings.Contains(res.Content, "cmd/boop") {
			t.Errorf("max_depth=1 descended anyway:\n%s", res.Content)
		}
	})

	t.Run("depth cap is enforced", func(t *testing.T) {
		res := fsTestExec(t, tool, map[string]any{"path": ".", "recursive": true, "max_depth": 500})
		data := res.Data.(ListData)
		if data.MaxDepth != fsMaxListDepth {
			t.Errorf("MaxDepth = %d, want it clamped to %d", data.MaxDepth, fsMaxListDepth)
		}
	})

	t.Run("include_ignored", func(t *testing.T) {
		res := fsTestExec(t, tool, map[string]any{"path": ".", "recursive": true, "include_ignored": true})
		if !strings.Contains(res.Content, "node_modules") {
			t.Errorf("include_ignored did not include node_modules:\n%s", res.Content)
		}
		if data := res.Data.(ListData); len(data.Skipped) != 0 {
			t.Errorf("Skipped = %v, want empty when include_ignored is set", data.Skipped)
		}
	})
}

func TestListToolEntryCap(t *testing.T) {
	files := make(map[string]string, 40)
	for i := 0; i < 40; i++ {
		files[fmt.Sprintf("f%02d.txt", i)] = "x\n"
	}
	ws := fsTestWorkspace(t, files)
	res := fsTestExec(t, NewListTool(ws), map[string]any{"path": ".", "limit": 10})
	data := res.Data.(ListData)
	if len(data.Entries) != 10 {
		t.Errorf("returned %d entries, want the limit of 10", len(data.Entries))
	}
	if !data.Truncated {
		t.Error("ListData.Truncated = false, want true")
	}
	if !strings.Contains(res.Content, "[truncated at 10 entries") {
		t.Errorf("content does not announce truncation:\n%s", res.Content)
	}
}

func TestListToolSymlinkHandling(t *testing.T) {
	ws, _ := fsTestEscapeWorkspace(t)
	res := fsTestExec(t, NewListTool(ws), map[string]any{"path": ".", "recursive": true})
	data := res.Data.(ListData)
	var link *ListEntry
	for i := range data.Entries {
		if data.Entries[i].Path == "escape" {
			link = &data.Entries[i]
		}
	}
	if link == nil {
		t.Fatalf("the symlink entry is missing from the listing: %+v", data.Entries)
	}
	if link.Type != "symlink" {
		t.Errorf("entry type = %q, want symlink", link.Type)
	}
	if strings.Contains(res.Content, "secret.txt") {
		t.Errorf("listing followed a symlink out of the workspace:\n%s", res.Content)
	}
}

func TestListToolErrors(t *testing.T) {
	ws, _ := fsTestEscapeWorkspace(t)
	tool := NewListTool(ws)

	tests := []struct {
		name     string
		args     map[string]any
		contains string
	}{
		{"missing directory", map[string]any{"path": "nope"}, "directory not found"},
		{"file target", map[string]any{"path": "inside.txt"}, "is a file, not a directory"},
		{"traversal", map[string]any{"path": "../.."}, "escapes the workspace"},
		{"escaping symlink", map[string]any{"path": "escape"}, "escapes the workspace"},
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

func TestListToolEmptyDirectory(t *testing.T) {
	ws := fsTestWorkspace(t, nil)
	if err := os.Mkdir(filepath.Join(ws.Root(), "empty"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	res := fsTestExec(t, NewListTool(ws), map[string]any{"path": "empty"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "is empty") {
		t.Errorf("content = %q, want it to say the directory is empty", res.Content)
	}
}

func TestListToolPermission(t *testing.T) {
	ws := fsTestTreeWorkspace(t)
	tool := NewListTool(ws)

	action, err := tool.Permission(fsTestCall(t, tool.Name(), map[string]any{"path": "internal", "recursive": true}))
	if err != nil {
		t.Fatalf("Permission: %v", err)
	}
	if action.Category != permissions.CatFilesystemRead {
		t.Errorf("Category = %q, want %q", action.Category, permissions.CatFilesystemRead)
	}
	if action.Risk != permissions.RiskLow {
		t.Errorf("Risk = %q, want low", action.Risk)
	}
	if action.Summary != "List internal recursively" {
		t.Errorf("Summary = %q", action.Summary)
	}
	if len(action.Paths) != 1 || !strings.HasSuffix(action.Paths[0], "internal") {
		t.Errorf("Paths = %v", action.Paths)
	}

	escaping, err := tool.Permission(fsTestCall(t, tool.Name(), map[string]any{"path": "../.."}))
	if err != nil {
		t.Fatalf("Permission: %v", err)
	}
	if escaping.Risk != permissions.RiskCritical {
		t.Errorf("Risk = %q, want critical for an escaping path", escaping.Risk)
	}
}

func TestFsClamp(t *testing.T) {
	tests := []struct{ in, def, max, want int }{
		{0, 10, 100, 10},
		{-5, 10, 100, 10},
		{7, 10, 100, 7},
		{1000, 10, 100, 100},
	}
	for _, tc := range tests {
		if got := fsClamp(tc.in, tc.def, tc.max); got != tc.want {
			t.Errorf("fsClamp(%d, %d, %d) = %d, want %d", tc.in, tc.def, tc.max, got, tc.want)
		}
	}
}

func TestListToolSchemaAndDescription(t *testing.T) {
	tool := NewListTool(fsTestWorkspace(t, nil))
	if tool.Name() != "list" {
		t.Errorf("Name = %q", tool.Name())
	}
	if !strings.Contains(tool.Description(), "node_modules") {
		t.Errorf("Description does not mention the skipped directories: %q", tool.Description())
	}
	schema := tool.Schema()
	props := schema["properties"].(map[string]any)
	for _, key := range []string{"path", "recursive", "max_depth", "limit", "include_ignored"} {
		if _, ok := props[key]; !ok {
			t.Errorf("schema is missing property %q", key)
		}
	}
	if _, ok := schema["required"]; ok {
		t.Error("list has no required arguments, so the schema must not declare any")
	}
}
