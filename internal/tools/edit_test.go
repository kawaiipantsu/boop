package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/permissions"
)

func TestEditToolReplacesUniqueMatch(t *testing.T) {
	ws := fsTestWorkspace(t, map[string]string{
		"main.go": "package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n",
	})
	res := fsTestExec(t, NewEditTool(ws), map[string]any{
		"path":       "main.go",
		"old_string": "println(\"hi\")",
		"new_string": "println(\"hello\")",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	data, ok := res.Data.(EditData)
	if !ok {
		t.Fatalf("Data is %T, want EditData", res.Data)
	}
	if data.Replacements != 1 {
		t.Errorf("Replacements = %d, want 1", data.Replacements)
	}
	if len(data.MatchLines) != 1 || data.MatchLines[0] != 4 {
		t.Errorf("MatchLines = %v, want [4]", data.MatchLines)
	}
	for _, want := range []string{"@@ main.go:4 @@", "- println(\"hi\")", "+ println(\"hello\")"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("diff summary %q does not contain %q", res.Content, want)
		}
	}
	got, err := os.ReadFile(filepath.Join(ws.Root(), "main.go"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(got), "println(\"hello\")") {
		t.Errorf("file was not modified: %q", got)
	}
}

func TestEditToolReplaceAll(t *testing.T) {
	ws := fsTestWorkspace(t, map[string]string{"a.txt": "foo\nbar\nfoo\nbaz\nfoo\n"})
	res := fsTestExec(t, NewEditTool(ws), map[string]any{
		"path":        "a.txt",
		"old_string":  "foo",
		"new_string":  "qux",
		"replace_all": true,
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	data := res.Data.(EditData)
	if data.Replacements != 3 {
		t.Errorf("Replacements = %d, want 3", data.Replacements)
	}
	got, _ := os.ReadFile(filepath.Join(ws.Root(), "a.txt"))
	if string(got) != "qux\nbar\nqux\nbaz\nqux\n" {
		t.Errorf("file contains %q", got)
	}
	if !strings.Contains(res.Content, "and 0 more") && !strings.Contains(res.Content, "@@ a.txt:5 @@") {
		// three hunks fit under the cap, so the last one must be rendered
		t.Errorf("diff summary is missing the final hunk: %s", res.Content)
	}
}

func TestEditToolReportsAmbiguity(t *testing.T) {
	ws := fsTestWorkspace(t, map[string]string{"a.txt": "foo\nbar\nfoo\n"})
	res := fsTestExec(t, NewEditTool(ws), map[string]any{
		"path":       "a.txt",
		"old_string": "foo",
		"new_string": "qux",
	})
	if !res.IsError {
		t.Fatal("an ambiguous edit must be reported as an error result")
	}
	for _, want := range []string{"ambiguous", "matches 2 times", "lines 1, 3", "replace_all=true"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("message %q does not contain %q", res.Content, want)
		}
	}
	data := res.Data.(EditData)
	if len(data.MatchLines) != 2 {
		t.Errorf("MatchLines = %v, want two entries", data.MatchLines)
	}
	got, _ := os.ReadFile(filepath.Join(ws.Root(), "a.txt"))
	if string(got) != "foo\nbar\nfoo\n" {
		t.Errorf("file was modified despite the ambiguity: %q", got)
	}
}

func TestEditToolReportsMissingMatch(t *testing.T) {
	ws := fsTestWorkspace(t, map[string]string{
		"plain.txt":  "hello world\n",
		"indent.txt": "    if x {\n",
		"crlf.txt":   "alpha\r\nbeta\r\n",
	})
	tool := NewEditTool(ws)

	tests := []struct {
		name     string
		args     map[string]any
		contains []string
	}{
		{
			name:     "absent text",
			args:     map[string]any{"path": "plain.txt", "old_string": "nowhere", "new_string": "x"},
			contains: []string{"not found", "copy the target text exactly"},
		},
		{
			name:     "case difference",
			args:     map[string]any{"path": "plain.txt", "old_string": "HELLO WORLD", "new_string": "x"},
			contains: []string{"not found", "letter case"},
		},
		{
			name:     "whitespace difference",
			args:     map[string]any{"path": "indent.txt", "old_string": "if  x {", "new_string": "x"},
			contains: []string{"not found", "whitespace or indentation"},
		},
		{
			name:     "line ending difference",
			args:     map[string]any{"path": "crlf.txt", "old_string": "alpha\nbeta", "new_string": "x"},
			contains: []string{"not found", "line endings"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := fsTestExec(t, tool, tc.args)
			if !res.IsError {
				t.Fatalf("expected an error result, got %q", res.Content)
			}
			for _, want := range tc.contains {
				if !strings.Contains(res.Content, want) {
					t.Errorf("message %q does not contain %q", res.Content, want)
				}
			}
		})
	}
}

func TestEditToolArgumentAndTargetErrors(t *testing.T) {
	ws := fsTestWorkspace(t, map[string]string{
		"a.txt":   "content\n",
		"bin.dat": "\x00\x01\x02binary\x00",
		"sub/x":   "x\n",
	})
	tool := NewEditTool(ws)

	tests := []struct {
		name     string
		args     map[string]any
		contains string
	}{
		{"missing path", map[string]any{"old_string": "a", "new_string": "b"}, "required"},
		{"empty old_string", map[string]any{"path": "a.txt", "old_string": "", "new_string": "b"}, "must not be empty"},
		{"identical strings", map[string]any{"path": "a.txt", "old_string": "x", "new_string": "x"}, "identical"},
		{"missing file", map[string]any{"path": "nope.txt", "old_string": "a", "new_string": "b"}, "file not found"},
		{"directory", map[string]any{"path": "sub", "old_string": "a", "new_string": "b"}, "is a directory"},
		{"binary file", map[string]any{"path": "bin.dat", "old_string": "binary", "new_string": "b"}, "binary file"},
		{"traversal", map[string]any{"path": "../../etc/hosts", "old_string": "a", "new_string": "b"}, "escapes the workspace"},
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

func TestEditToolRejectsEscapingSymlink(t *testing.T) {
	ws, outside := fsTestEscapeWorkspace(t)
	res := fsTestExec(t, NewEditTool(ws), map[string]any{
		"path":       "escape/secret.txt",
		"old_string": "hunter2",
		"new_string": "owned",
	})
	if !res.IsError {
		t.Fatalf("editing through an escaping symlink succeeded: %s", res.Content)
	}
	got, err := os.ReadFile(filepath.Join(outside, "secret.txt"))
	if err != nil {
		t.Fatalf("read outside file: %v", err)
	}
	if string(got) != "password=hunter2\n" {
		t.Errorf("file outside the workspace was modified: %q", got)
	}
}

func TestEditToolPermission(t *testing.T) {
	ws := fsTestWorkspace(t, map[string]string{"a.txt": "x\n"})
	tool := NewEditTool(ws)

	tests := []struct {
		name     string
		args     map[string]any
		wantRisk permissions.Risk
		summary  string
	}{
		{
			name:     "single replacement",
			args:     map[string]any{"path": "a.txt", "old_string": "x", "new_string": "y"},
			wantRisk: permissions.RiskMedium,
			summary:  "Edit a.txt, replacing one occurrence",
		},
		{
			name:     "replace all",
			args:     map[string]any{"path": "a.txt", "old_string": "x", "new_string": "y", "replace_all": true},
			wantRisk: permissions.RiskHigh,
			summary:  "Edit a.txt, replacing every occurrence",
		},
		{
			name:     "escaping path",
			args:     map[string]any{"path": "../../x", "old_string": "x", "new_string": "y"},
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
			if tc.summary != "" && action.Summary != tc.summary {
				t.Errorf("Summary = %q, want %q", action.Summary, tc.summary)
			}
			if !strings.Contains(action.Detail, "--- old") || !strings.Contains(action.Detail, "+++ new") {
				t.Errorf("Detail %q does not show the replacement", action.Detail)
			}
		})
	}
}

func TestEditToolDiffCapsHunks(t *testing.T) {
	ws := fsTestWorkspace(t, map[string]string{"a.txt": strings.Repeat("target\n", 10)})
	res := fsTestExec(t, NewEditTool(ws), map[string]any{
		"path":        "a.txt",
		"old_string":  "target",
		"new_string":  "changed",
		"replace_all": true,
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if strings.Count(res.Content, "@@ a.txt:") != fsMaxEditHunks {
		t.Errorf("rendered %d hunks, want the cap of %d", strings.Count(res.Content, "@@ a.txt:"), fsMaxEditHunks)
	}
	if !strings.Contains(res.Content, "and 7 more identical replacements") {
		t.Errorf("content does not summarise the remaining replacements: %s", res.Content)
	}
}

func TestEditToolSchemaAndDescription(t *testing.T) {
	tool := NewEditTool(fsTestWorkspace(t, nil))
	if tool.Name() != "edit" {
		t.Errorf("Name = %q", tool.Name())
	}
	if !strings.Contains(tool.Description(), "replace_all") {
		t.Errorf("Description does not explain uniqueness: %q", tool.Description())
	}
	schema := tool.Schema()
	props := schema["properties"].(map[string]any)
	for _, key := range []string{"path", "old_string", "new_string", "replace_all"} {
		if _, ok := props[key]; !ok {
			t.Errorf("schema is missing property %q", key)
		}
	}
	if req := schema["required"].([]string); len(req) != 3 {
		t.Errorf("required = %v, want path, old_string and new_string", req)
	}
}

func TestFsWriteHunkSide(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		contains string
	}{
		{"deletion", "", "- (nothing)"},
		{"single line", "abc", "- abc"},
		{"capped", strings.Repeat("line\n", fsMaxHunkLines+5), fmt.Sprintf("- … %d more lines", 5)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			fsWriteHunkSide(&b, "-", tc.text)
			if !strings.Contains(b.String(), tc.contains) {
				t.Errorf("rendered %q, want it to contain %q", b.String(), tc.contains)
			}
		})
	}
}

func TestFsIndexAllAndOffsetLines(t *testing.T) {
	const s = "aa\nbb\naa\n"
	offsets := fsIndexAll(s, "aa")
	if len(offsets) != 2 || offsets[0] != 0 || offsets[1] != 6 {
		t.Fatalf("fsIndexAll = %v, want [0 6]", offsets)
	}
	lines := fsOffsetLines(s, offsets)
	if len(lines) != 2 || lines[0] != 1 || lines[1] != 3 {
		t.Fatalf("fsOffsetLines = %v, want [1 3]", lines)
	}
	if fsIndexAll(s, "") != nil {
		t.Error("fsIndexAll with an empty needle must return no offsets")
	}
	// Overlapping needles must not be double counted.
	if got := fsIndexAll("aaaa", "aa"); len(got) != 2 {
		t.Errorf("fsIndexAll(aaaa, aa) = %v, want two non-overlapping matches", got)
	}
}
