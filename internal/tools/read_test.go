package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/permissions"
)

// fsTestWorkspace builds a workspace in a temporary directory populated with
// files, whose keys are slash-separated relative paths.
func fsTestWorkspace(t *testing.T, files map[string]string) *Workspace {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	return ws
}

// fsTestCall encodes args into a Call for tool.
func fsTestCall(t *testing.T, name string, args map[string]any) Call {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return Call{ID: "call-1", Name: name, Arguments: raw}
}

// fsTestExec runs tool and fails the test on a Go error, which the tools
// reserve for internal faults rather than recoverable problems.
func fsTestExec(t *testing.T, tool Tool, args map[string]any) Result {
	t.Helper()
	res, err := tool.Execute(context.Background(), fsTestCall(t, tool.Name(), args))
	if err != nil {
		t.Fatalf("%s.Execute returned a Go error: %v", tool.Name(), err)
	}
	if res.Tool != tool.Name() {
		t.Errorf("Result.Tool = %q, want %q", res.Tool, tool.Name())
	}
	if res.CallID != "call-1" {
		t.Errorf("Result.CallID = %q, want %q", res.CallID, "call-1")
	}
	return res
}

// fsTestEscapeWorkspace returns a workspace containing a symlink named
// "escape" that points at a directory outside it, plus that outside directory.
func fsTestEscapeWorkspace(t *testing.T) (ws *Workspace, outside string) {
	t.Helper()
	outside = t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("password=hunter2\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	ws = fsTestWorkspace(t, map[string]string{"inside.txt": "safe\n"})
	if err := os.Symlink(outside, filepath.Join(ws.Root(), "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	return ws, outside
}

func TestReadToolExecute(t *testing.T) {
	ws := fsTestWorkspace(t, map[string]string{
		"a.txt":         "one\ntwo\nthree\nfour\nfive\n",
		"empty.txt":     "",
		"nonl.txt":      "no trailing newline",
		"bin.dat":       "ELF\x00\x01\x02\x03binary\x00stuff",
		"docs/note.md":  "# Note\n",
		"crlf.txt":      "a\r\nb\r\n",
		"deep/dir/x.go": "package deep\n",
	})
	if err := os.Mkdir(filepath.Join(ws.Root(), "adir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	tool := NewReadTool(ws)

	tests := []struct {
		name     string
		args     map[string]any
		wantErr  bool
		contains []string
		absent   []string
	}{
		{
			name:     "whole file",
			args:     map[string]any{"path": "a.txt"},
			contains: []string{"one\ntwo\nthree\nfour\nfive\n"},
		},
		{
			name:     "offset and limit",
			args:     map[string]any{"path": "a.txt", "offset": 2, "limit": 2},
			contains: []string{"two\nthree\n", "read on with offset=4"},
			absent:   []string{"one", "five"},
		},
		{
			name:     "line numbers",
			args:     map[string]any{"path": "a.txt", "line_numbers": true},
			contains: []string{"     1\tone", "     5\tfive"},
		},
		{
			name:     "strips carriage returns",
			args:     map[string]any{"path": "crlf.txt"},
			contains: []string{"a\nb\n"},
			absent:   []string{"\r"},
		},
		{
			name:     "file without trailing newline",
			args:     map[string]any{"path": "nonl.txt"},
			contains: []string{"no trailing newline"},
		},
		{
			name:     "nested path",
			args:     map[string]any{"path": "docs/note.md"},
			contains: []string{"# Note"},
		},
		{
			name:     "empty file",
			args:     map[string]any{"path": "empty.txt"},
			contains: []string{"is empty"},
		},
		{
			name:     "binary file is described not dumped",
			args:     map[string]any{"path": "bin.dat"},
			contains: []string{"binary file"},
			absent:   []string{"\x00"},
		},
		{
			name:     "missing file",
			args:     map[string]any{"path": "nope.txt"},
			wantErr:  true,
			contains: []string{"file not found"},
		},
		{
			name:     "directory",
			args:     map[string]any{"path": "adir"},
			wantErr:  true,
			contains: []string{"is a directory", "list tool"},
		},
		{
			name:     "offset past end",
			args:     map[string]any{"path": "a.txt", "offset": 99},
			wantErr:  true,
			contains: []string{"past the end", "5 line"},
		},
		{
			name:     "missing path argument",
			args:     map[string]any{},
			wantErr:  true,
			contains: []string{"required"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := fsTestExec(t, tool, tc.args)
			if res.IsError != tc.wantErr {
				t.Fatalf("IsError = %v, want %v (content: %s)", res.IsError, tc.wantErr, res.Content)
			}
			for _, want := range tc.contains {
				if !strings.Contains(res.Content, want) {
					t.Errorf("content %q does not contain %q", res.Content, want)
				}
			}
			for _, bad := range tc.absent {
				if strings.Contains(res.Content, bad) {
					t.Errorf("content %q unexpectedly contains %q", res.Content, bad)
				}
			}
		})
	}
}

func TestReadToolBinaryData(t *testing.T) {
	ws := fsTestWorkspace(t, map[string]string{"bin.dat": "\x89PNG\x00\x00\x00\rIHDR\x00\x00"})
	res := fsTestExec(t, NewReadTool(ws), map[string]any{"path": "bin.dat"})
	data, ok := res.Data.(ReadData)
	if !ok {
		t.Fatalf("Data is %T, want ReadData", res.Data)
	}
	if !data.Binary {
		t.Error("ReadData.Binary = false, want true")
	}
	if data.MediaType == "" {
		t.Error("ReadData.MediaType is empty for a binary file")
	}
	if res.IsError {
		t.Error("reading a binary file should not be reported as a repairable error")
	}
}

func TestReadToolTruncatesLargeOutput(t *testing.T) {
	var b strings.Builder
	line := strings.Repeat("x", 200) + "\n"
	total := (fsMaxOutputBytes / len(line)) * 3
	for i := 0; i < total; i++ {
		b.WriteString(line)
	}
	ws := fsTestWorkspace(t, map[string]string{"big.txt": b.String()})

	res := fsTestExec(t, NewReadTool(ws), map[string]any{"path": "big.txt"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	data := res.Data.(ReadData)
	if !data.Truncated {
		t.Error("ReadData.Truncated = false, want true")
	}
	if data.TotalLines != total {
		t.Errorf("TotalLines = %d, want %d", data.TotalLines, total)
	}
	if data.LastLine >= data.TotalLines {
		t.Errorf("LastLine = %d, want fewer than TotalLines %d", data.LastLine, data.TotalLines)
	}
	if !strings.Contains(res.Content, "[truncated:") {
		t.Error("content does not announce truncation")
	}
	if len(res.Content) > fsMaxOutputBytes+1024 {
		t.Errorf("content is %d bytes, want at most the %d byte cap plus the note", len(res.Content), fsMaxOutputBytes)
	}
}

func TestReadToolRejectsEscapes(t *testing.T) {
	ws, outside := fsTestEscapeWorkspace(t)
	tool := NewReadTool(ws)

	for _, path := range []string{
		"escape/secret.txt",
		"../../etc/passwd",
		filepath.Join(outside, "secret.txt"),
	} {
		t.Run(path, func(t *testing.T) {
			res := fsTestExec(t, tool, map[string]any{"path": path})
			if !res.IsError {
				t.Fatalf("reading %q succeeded, want rejection: %s", path, res.Content)
			}
			if strings.Contains(res.Content, "hunter2") {
				t.Fatal("content outside the workspace leaked into the result")
			}
		})
	}
}

func TestReadToolPermission(t *testing.T) {
	ws := fsTestWorkspace(t, map[string]string{"a.txt": "x\n", ".env": "TOKEN=abc\n"})
	tool := NewReadTool(ws)

	tests := []struct {
		name     string
		args     map[string]any
		wantRisk permissions.Risk
		summary  string
	}{
		{"ordinary file", map[string]any{"path": "a.txt"}, permissions.RiskLow, "Read a.txt"},
		{"line range", map[string]any{"path": "a.txt", "offset": 2, "limit": 3}, permissions.RiskLow, "Read lines 2-4 from a.txt"},
		{"secret bearing file", map[string]any{"path": ".env"}, permissions.RiskMedium, "Read .env"},
		{"escaping path", map[string]any{"path": "../../etc/passwd"}, permissions.RiskCritical, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			action, err := tool.Permission(fsTestCall(t, tool.Name(), tc.args))
			if err != nil {
				t.Fatalf("Permission: %v", err)
			}
			if action.Category != permissions.CatFilesystemRead {
				t.Errorf("Category = %q, want %q", action.Category, permissions.CatFilesystemRead)
			}
			if action.Risk != tc.wantRisk {
				t.Errorf("Risk = %q, want %q", action.Risk, tc.wantRisk)
			}
			if action.Tool != "read" {
				t.Errorf("Tool = %q, want read", action.Tool)
			}
			if len(action.Paths) != 1 || action.Paths[0] == "" {
				t.Errorf("Paths = %v, want one populated path", action.Paths)
			}
			if tc.summary != "" && action.Summary != tc.summary {
				t.Errorf("Summary = %q, want %q", action.Summary, tc.summary)
			}
		})
	}
}

func TestReadToolSchema(t *testing.T) {
	tool := NewReadTool(fsTestWorkspace(t, nil))
	if tool.Name() != "read" {
		t.Errorf("Name = %q", tool.Name())
	}
	if !strings.Contains(tool.Description(), "offset") {
		t.Errorf("Description does not mention the line window: %q", tool.Description())
	}
	schema := tool.Schema()
	if schema["type"] != "object" {
		t.Fatalf("schema type = %v, want object", schema["type"])
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema has no properties object")
	}
	for _, key := range []string{"path", "offset", "limit", "line_numbers"} {
		if _, ok := props[key]; !ok {
			t.Errorf("schema is missing property %q", key)
		}
	}
	if req, _ := schema["required"].([]string); len(req) != 1 || req[0] != "path" {
		t.Errorf("required = %v, want [path]", schema["required"])
	}
	if _, err := json.Marshal(schema); err != nil {
		t.Fatalf("schema is not JSON serialisable: %v", err)
	}
}

func TestFsIsBinary(t *testing.T) {
	tests := []struct {
		name   string
		sample string
		want   bool
	}{
		{"empty", "", false},
		{"ascii", "package main\n\nfunc main() {}\n", false},
		{"utf8", "héllo wörld — ok\n", false},
		{"tabs and returns", "a\tb\r\nc\n", false},
		{"nul byte", "abc\x00def", true},
		{"control heavy", "\x01\x02\x03\x04\x05\x06\x07ab", true},
		{"invalid utf8", "abc\xff\xfe\xfd\xfc\xfb\xfa\xf9\xf8", true},
		{"escape sequences are text", "\x1b[31mred\x1b[0m\n", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := fsIsBinary([]byte(tc.sample)); got != tc.want {
				t.Errorf("fsIsBinary(%q) = %v, want %v", tc.sample, got, tc.want)
			}
		})
	}
}

func TestFsHumanBytes(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{2150, "2.1 KB"},
		{3 << 20, "3.0 MB"},
		{5 << 30, "5.0 GB"},
	}
	for _, tc := range tests {
		if got := fsHumanBytes(tc.in); got != tc.want {
			t.Errorf("fsHumanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFsSensitive(t *testing.T) {
	sensitive := []string{".env", ".env.local", "config/server.pem", "id_rsa", "keys/deploy.key", "credentials.json", "secrets.yaml"}
	ordinary := []string{"main.go", "README.md", "env.go", "keyboard.ts", "internal/secretsanta.go/x.txt"}
	for _, p := range sensitive {
		if !fsSensitive(p) {
			t.Errorf("fsSensitive(%q) = false, want true", p)
		}
	}
	for _, p := range ordinary {
		if fsSensitive(p) {
			t.Errorf("fsSensitive(%q) = true, want false", p)
		}
	}
}

func TestReadToolCancelledContext(t *testing.T) {
	ws := fsTestWorkspace(t, map[string]string{"a.txt": strings.Repeat("line\n", 4096)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewReadTool(ws).Execute(ctx, fsTestCall(t, "read", map[string]any{"path": "a.txt"}))
	if err == nil {
		t.Fatal("cancelled context should surface as a Go error, not a tool result")
	}
}

// Ensure every filesystem tool satisfies the Tool interface.
var _ = []func(*Workspace) Tool{
	func(w *Workspace) Tool { return NewReadTool(w) },
	func(w *Workspace) Tool { return NewWriteTool(w) },
	func(w *Workspace) Tool { return NewEditTool(w) },
	func(w *Workspace) Tool { return NewListTool(w) },
	func(w *Workspace) Tool { return NewFindTool(w) },
	func(w *Workspace) Tool { return NewSearchTool(w) },
}
