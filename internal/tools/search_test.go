package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/permissions"
)

func TestSearchToolFindsMatches(t *testing.T) {
	ws := fsTestTreeWorkspace(t)
	res := fsTestExec(t, NewSearchTool(ws), map[string]any{"pattern": `func \w+\(`})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	data, ok := res.Data.(SearchData)
	if !ok {
		t.Fatalf("Data is %T, want SearchData", res.Data)
	}
	if len(data.Matches) != 2 {
		t.Fatalf("matches = %+v, want two", data.Matches)
	}
	byPath := map[string]SearchMatch{}
	for _, m := range data.Matches {
		byPath[m.Path] = m
	}
	main, ok := byPath[filepath.FromSlash("cmd/boop/main.go")]
	if !ok {
		t.Fatalf("no match in cmd/boop/main.go: %+v", data.Matches)
	}
	if main.Line != 3 {
		t.Errorf("line = %d, want 3", main.Line)
	}
	if main.Text != "func main() {}" {
		t.Errorf("text = %q", main.Text)
	}
	if !strings.Contains(res.Content, "main.go:3:func main() {}") {
		t.Errorf("content is not in file:line:text form:\n%s", res.Content)
	}
	if data.FilesMatched != 2 {
		t.Errorf("FilesMatched = %d, want 2", data.FilesMatched)
	}
	// vendor/ and node_modules/ are skipped by default.
	if strings.Contains(res.Content, "vendor/") || strings.Contains(res.Content, "node_modules") {
		t.Errorf("search descended into ignored directories:\n%s", res.Content)
	}
}

func TestSearchToolOptions(t *testing.T) {
	ws := fsTestWorkspace(t, map[string]string{
		"a.go":  "package a\n\n// TODO: fix\nfunc A() {}\n",
		"b.txt": "todo lower case\n",
		"c.md":  "line1\nline2\nTODO here\nline4\nline5\n",
	})
	tool := NewSearchTool(ws)

	t.Run("case sensitive by default", func(t *testing.T) {
		res := fsTestExec(t, tool, map[string]any{"pattern": "TODO"})
		data := res.Data.(SearchData)
		if len(data.Matches) != 2 {
			t.Errorf("matches = %+v, want two case-sensitive hits", data.Matches)
		}
	})

	t.Run("case insensitive", func(t *testing.T) {
		res := fsTestExec(t, tool, map[string]any{"pattern": "TODO", "case_insensitive": true})
		data := res.Data.(SearchData)
		if len(data.Matches) != 3 {
			t.Errorf("matches = %+v, want three", data.Matches)
		}
	})

	t.Run("glob filter", func(t *testing.T) {
		res := fsTestExec(t, tool, map[string]any{"pattern": "TODO", "case_insensitive": true, "glob": "*.go"})
		data := res.Data.(SearchData)
		if len(data.Matches) != 1 || !strings.HasSuffix(data.Matches[0].Path, "a.go") {
			t.Errorf("matches = %+v, want only a.go", data.Matches)
		}
	})

	t.Run("context lines", func(t *testing.T) {
		res := fsTestExec(t, tool, map[string]any{"pattern": "TODO here", "context": 1})
		for _, want := range []string{"c.md:2-line2", "c.md:3:TODO here", "c.md:4-line4"} {
			if !strings.Contains(res.Content, want) {
				t.Errorf("content does not contain %q:\n%s", want, res.Content)
			}
		}
		if strings.Contains(res.Content, "c.md:1-line1") {
			t.Errorf("context exceeded one line:\n%s", res.Content)
		}
	})

	t.Run("single file target", func(t *testing.T) {
		res := fsTestExec(t, tool, map[string]any{"pattern": "package", "path": "a.go"})
		data := res.Data.(SearchData)
		if len(data.Matches) != 1 {
			t.Errorf("matches = %+v, want one", data.Matches)
		}
	})

	t.Run("no matches", func(t *testing.T) {
		res := fsTestExec(t, tool, map[string]any{"pattern": "zzz-not-present"})
		if res.IsError {
			t.Fatalf("an empty result set is not an error: %s", res.Content)
		}
		if !strings.Contains(res.Content, "No matches") {
			t.Errorf("content = %q", res.Content)
		}
	})
}

func TestSearchToolSkipsBinaryFiles(t *testing.T) {
	ws := fsTestWorkspace(t, map[string]string{
		"text.txt": "needle here\n",
		"blob.bin": "\x00\x01needle\x00\x02",
	})
	res := fsTestExec(t, NewSearchTool(ws), map[string]any{"pattern": "needle"})
	data := res.Data.(SearchData)
	if len(data.Matches) != 1 {
		t.Fatalf("matches = %+v, want only the text file", data.Matches)
	}
	if data.FilesSkipped != 1 {
		t.Errorf("FilesSkipped = %d, want 1", data.FilesSkipped)
	}
	if strings.Contains(res.Content, "blob.bin") {
		t.Errorf("binary file appeared in the results:\n%s", res.Content)
	}
}

func TestSearchToolLimit(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&b, "needle %d\n", i)
	}
	ws := fsTestWorkspace(t, map[string]string{"a.txt": b.String()})
	res := fsTestExec(t, NewSearchTool(ws), map[string]any{"pattern": "needle", "limit": 7})
	data := res.Data.(SearchData)
	if len(data.Matches) != 7 {
		t.Errorf("returned %d matches, want the limit of 7", len(data.Matches))
	}
	if !data.Truncated {
		t.Error("SearchData.Truncated = false, want true")
	}
	if !strings.Contains(res.Content, "[truncated at 7 matches") {
		t.Errorf("content does not announce truncation:\n%s", res.Content)
	}
}

func TestSearchToolTruncatesLongLines(t *testing.T) {
	ws := fsTestWorkspace(t, map[string]string{"min.js": "needle" + strings.Repeat("x", fsMaxSearchLineChars*2) + "\n"})
	res := fsTestExec(t, NewSearchTool(ws), map[string]any{"pattern": "needle"})
	data := res.Data.(SearchData)
	if len(data.Matches) != 1 {
		t.Fatalf("matches = %+v", data.Matches)
	}
	if !strings.HasSuffix(data.Matches[0].Text, "(line truncated)") {
		t.Errorf("long line was not truncated: %d chars", len(data.Matches[0].Text))
	}
}

func TestSearchToolErrors(t *testing.T) {
	ws, _ := fsTestEscapeWorkspace(t)
	tool := NewSearchTool(ws)

	tests := []struct {
		name     string
		args     map[string]any
		contains string
	}{
		{"missing pattern", map[string]any{}, "required"},
		{"invalid regex", map[string]any{"pattern": "func ("}, "invalid regular expression"},
		{"invalid glob", map[string]any{"pattern": "x", "glob": "[a-"}, "invalid glob"},
		{"missing path", map[string]any{"pattern": "x", "path": "nope"}, "path not found"},
		{"traversal", map[string]any{"pattern": "x", "path": "../.."}, "escapes the workspace"},
		{"escaping symlink", map[string]any{"pattern": "hunter2", "path": "escape"}, "escapes the workspace"},
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
			if strings.Contains(res.Content, "hunter2\n") {
				t.Error("content outside the workspace leaked into the result")
			}
		})
	}
}

func TestSearchToolDoesNotFollowEscapingSymlink(t *testing.T) {
	ws, _ := fsTestEscapeWorkspace(t)
	res := fsTestExec(t, NewSearchTool(ws), map[string]any{"pattern": "hunter2"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if strings.Contains(res.Content, "hunter2") && !strings.Contains(res.Content, "No matches") {
		t.Errorf("search followed a symlink out of the workspace:\n%s", res.Content)
	}
	if data := res.Data.(SearchData); len(data.Matches) != 0 {
		t.Errorf("matches = %+v, want none", data.Matches)
	}
}

func TestSearchToolInvalidRegexIsRecoverable(t *testing.T) {
	ws := fsTestWorkspace(t, map[string]string{"a.txt": "x\n"})
	res := fsTestExec(t, NewSearchTool(ws), map[string]any{"pattern": `(?<=x)y`})
	if !res.IsError {
		t.Fatal("lookbehind is not RE2 syntax and must be reported as an error result")
	}
	if !strings.Contains(res.Content, "RE2") {
		t.Errorf("message %q does not explain the supported syntax", res.Content)
	}
}

func TestSearchToolPermission(t *testing.T) {
	ws := fsTestTreeWorkspace(t)
	tool := NewSearchTool(ws)

	action, err := tool.Permission(fsTestCall(t, "search", map[string]any{"pattern": "func", "path": "internal"}))
	if err != nil {
		t.Fatalf("Permission: %v", err)
	}
	if action.Category != permissions.CatFilesystemRead || action.Risk != permissions.RiskLow {
		t.Errorf("action = %+v, want a low-risk filesystem read", action)
	}
	if action.Summary != `Search internal for "func"` {
		t.Errorf("Summary = %q", action.Summary)
	}
	withGlob, err := tool.Permission(fsTestCall(t, "search", map[string]any{"pattern": "func", "glob": "*.go"}))
	if err != nil {
		t.Fatalf("Permission: %v", err)
	}
	if !strings.Contains(withGlob.Summary, "*.go files") {
		t.Errorf("Summary = %q, want it to mention the glob", withGlob.Summary)
	}
}

func TestSearchToolSchema(t *testing.T) {
	schema := NewSearchTool(fsTestWorkspace(t, nil)).Schema()
	props := schema["properties"].(map[string]any)
	for _, key := range []string{"pattern", "path", "glob", "case_insensitive", "context", "limit", "include_ignored"} {
		if _, ok := props[key]; !ok {
			t.Errorf("schema is missing property %q", key)
		}
	}
	if req := schema["required"].([]string); len(req) != 1 || req[0] != "pattern" {
		t.Errorf("required = %v, want [pattern]", req)
	}
}

func TestSearchToolUnreadableFileIsSkipped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}
	ws := fsTestWorkspace(t, map[string]string{"ok.txt": "needle\n", "locked.txt": "needle\n"})
	if err := os.Chmod(filepath.Join(ws.Root(), "locked.txt"), 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(ws.Root(), "locked.txt"), 0o644) })

	res := fsTestExec(t, NewSearchTool(ws), map[string]any{"pattern": "needle"})
	if res.IsError {
		t.Fatalf("an unreadable file must not fail the whole search: %s", res.Content)
	}
	data := res.Data.(SearchData)
	if len(data.Matches) != 1 {
		t.Errorf("matches = %+v, want only the readable file", data.Matches)
	}
}
