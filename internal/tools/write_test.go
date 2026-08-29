package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/permissions"
)

func TestWriteToolCreatesAndReplaces(t *testing.T) {
	ws := fsTestWorkspace(t, map[string]string{"existing.txt": "old contents\n"})
	tool := NewWriteTool(ws)

	t.Run("creates file and parent directories", func(t *testing.T) {
		res := fsTestExec(t, tool, map[string]any{"path": "a/b/c/new.txt", "content": "hello\nworld\n"})
		if res.IsError {
			t.Fatalf("unexpected error: %s", res.Content)
		}
		data, ok := res.Data.(WriteData)
		if !ok {
			t.Fatalf("Data is %T, want WriteData", res.Data)
		}
		if !data.Created {
			t.Error("WriteData.Created = false, want true")
		}
		if data.Bytes != 12 || data.Lines != 2 {
			t.Errorf("Bytes/Lines = %d/%d, want 12/2", data.Bytes, data.Lines)
		}
		if !strings.Contains(res.Content, "Created a/b/c/new.txt") {
			t.Errorf("content %q does not report creation", res.Content)
		}
		got, err := os.ReadFile(filepath.Join(ws.Root(), "a", "b", "c", "new.txt"))
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if string(got) != "hello\nworld\n" {
			t.Errorf("file contains %q", got)
		}
	})

	t.Run("replaces existing file and preserves mode", func(t *testing.T) {
		target := filepath.Join(ws.Root(), "existing.txt")
		if err := os.Chmod(target, 0o600); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		res := fsTestExec(t, tool, map[string]any{"path": "existing.txt", "content": "new\n"})
		if res.IsError {
			t.Fatalf("unexpected error: %s", res.Content)
		}
		data := res.Data.(WriteData)
		if data.Created {
			t.Error("WriteData.Created = true, want false for a replacement")
		}
		if data.PreviousBytes != 13 {
			t.Errorf("PreviousBytes = %d, want 13", data.PreviousBytes)
		}
		if !strings.Contains(res.Content, "Replaced existing.txt") {
			t.Errorf("content %q does not report replacement", res.Content)
		}
		info, err := os.Stat(target)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("mode = %v, want 0600 preserved across the atomic replace", perm)
		}
	})

	t.Run("leaves no temporary files behind", func(t *testing.T) {
		entries, err := os.ReadDir(ws.Root())
		if err != nil {
			t.Fatalf("readdir: %v", err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".boop-") {
				t.Errorf("temporary file %q was not cleaned up", e.Name())
			}
		}
	})

	t.Run("empty content is allowed", func(t *testing.T) {
		res := fsTestExec(t, tool, map[string]any{"path": "empty.txt", "content": ""})
		if res.IsError {
			t.Fatalf("unexpected error: %s", res.Content)
		}
		if _, err := os.Stat(filepath.Join(ws.Root(), "empty.txt")); err != nil {
			t.Fatalf("empty file was not created: %v", err)
		}
	})
}

func TestWriteToolErrors(t *testing.T) {
	ws := fsTestWorkspace(t, map[string]string{"sub/file.txt": "x\n"})
	tool := NewWriteTool(ws)

	tests := []struct {
		name     string
		args     map[string]any
		contains string
	}{
		{"missing path", map[string]any{"content": "x"}, "required"},
		{"directory target", map[string]any{"path": "sub", "content": "x"}, "is a directory"},
		{"traversal", map[string]any{"path": "../../evil.txt", "content": "x"}, "escapes the workspace"},
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

func TestWriteToolRejectsEscapingSymlink(t *testing.T) {
	ws, outside := fsTestEscapeWorkspace(t)
	res := fsTestExec(t, NewWriteTool(ws), map[string]any{"path": "escape/planted.txt", "content": "x"})
	if !res.IsError {
		t.Fatalf("write through an escaping symlink succeeded: %s", res.Content)
	}
	if _, err := os.Stat(filepath.Join(outside, "planted.txt")); err == nil {
		t.Fatal("a file was created outside the workspace")
	}
}

func TestWriteToolPermission(t *testing.T) {
	ws := fsTestWorkspace(t, map[string]string{"existing.txt": strings.Repeat("a", 1900)})
	tool := NewWriteTool(ws)

	tests := []struct {
		name        string
		args        map[string]any
		wantRisk    permissions.Risk
		wantSummary string
	}{
		{
			name:        "new file",
			args:        map[string]any{"path": "src/main.go", "content": strings.Repeat("x", 2150)},
			wantRisk:    permissions.RiskMedium,
			wantSummary: "Write 2.1 KB to src/main.go",
		},
		{
			name:        "overwrite is destructive",
			args:        map[string]any{"path": "existing.txt", "content": "short"},
			wantRisk:    permissions.RiskHigh,
			wantSummary: "Overwrite existing.txt with 5 B, replacing 1.9 KB",
		},
		{
			name:     "secret bearing file",
			args:     map[string]any{"path": ".env", "content": "TOKEN=x"},
			wantRisk: permissions.RiskHigh,
		},
		{
			name:     "escaping path",
			args:     map[string]any{"path": "../../evil", "content": "x"},
			wantRisk: permissions.RiskCritical,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			action, err := tool.Permission(fsTestCall(t, tool.Name(), tc.args))
			if err != nil {
				t.Fatalf("Permission: %v", err)
			}
			if action.Category != permissions.CatFilesystemWrite {
				t.Errorf("Category = %q, want %q", action.Category, permissions.CatFilesystemWrite)
			}
			if action.Risk != tc.wantRisk {
				t.Errorf("Risk = %q, want %q", action.Risk, tc.wantRisk)
			}
			if action.Tool != "write" {
				t.Errorf("Tool = %q, want write", action.Tool)
			}
			if len(action.Paths) != 1 {
				t.Errorf("Paths = %v, want exactly one", action.Paths)
			}
			if tc.wantSummary != "" && action.Summary != tc.wantSummary {
				t.Errorf("Summary = %q, want %q", action.Summary, tc.wantSummary)
			}
		})
	}
}

func TestWriteToolSchema(t *testing.T) {
	schema := NewWriteTool(fsTestWorkspace(t, nil)).Schema()
	props := schema["properties"].(map[string]any)
	for _, key := range []string{"path", "content"} {
		if _, ok := props[key]; !ok {
			t.Errorf("schema is missing property %q", key)
		}
	}
	req := schema["required"].([]string)
	if len(req) != 2 {
		t.Errorf("required = %v, want path and content", req)
	}
}

func TestFsCountLines(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"a\n", 1},
		{"a\nb", 2},
		{"a\nb\n", 2},
		{"\n", 1},
	}
	for _, tc := range tests {
		if got := fsCountLines(tc.in); got != tc.want {
			t.Errorf("fsCountLines(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestFsAtomicWritePreservesOriginalOnFailure(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "sub", "file.txt")
	if err := fsAtomicWrite(target, []byte("first\n"), 0o644); err != nil {
		t.Fatalf("fsAtomicWrite: %v", err)
	}
	if err := fsAtomicWrite(target, []byte("second\n"), 0o644); err != nil {
		t.Fatalf("fsAtomicWrite (replace): %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "second\n" {
		t.Errorf("file contains %q, want %q", got, "second\n")
	}
	entries, _ := os.ReadDir(filepath.Dir(target))
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only the target file", len(entries))
	}
}

func TestFsPreview(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		max      int
		contains string
	}{
		{"empty", "", 5, "(empty file)"},
		{"short", "a\nb\n", 5, "a\nb\n"},
		{"capped", "a\nb\nc\nd\n", 2, "… 3 more lines"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := fsPreview(tc.in, tc.max); !strings.Contains(got, tc.contains) {
				t.Errorf("fsPreview(%q, %d) = %q, want it to contain %q", tc.in, tc.max, got, tc.contains)
			}
		})
	}
}

func TestWriteToolDescription(t *testing.T) {
	tool := NewWriteTool(fsTestWorkspace(t, nil))
	if tool.Name() != "write" {
		t.Errorf("Name = %q", tool.Name())
	}
	if !strings.Contains(tool.Description(), "edit tool") {
		t.Errorf("Description does not point at the edit tool: %q", tool.Description())
	}
}
