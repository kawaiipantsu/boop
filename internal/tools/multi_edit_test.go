package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMultiEditSequentialSuccess(t *testing.T) {
	origContent := `package main

import "fmt"

func hello() {
	fmt.Println("foo")
}

func world() {
	fmt.Println("bar")
}
`
	root := execWriteFixture(t, map[string]string{
		"main.go": origContent,
	})
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}

	tool := NewMultiEditTool(ws)
	call := Call{
		ID:   "call_multi_1",
		Name: "multi_edit",
		Arguments: []byte(`{
			"path": "main.go",
			"edits": [
				{"old_string": "\"foo\"", "new_string": "\"hello\""},
				{"old_string": "\"bar\"", "new_string": "\"world\""}
			]
		}`),
	}

	res, err := tool.Execute(context.Background(), call)
	if err != nil || res.IsError {
		t.Fatalf("multi_edit failed: %v, content: %s", err, res.Content)
	}

	newBytes, err := os.ReadFile(filepath.Join(root, "main.go"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	newStr := string(newBytes)
	if !strings.Contains(newStr, `"hello"`) || !strings.Contains(newStr, `"world"`) {
		t.Errorf("multi_edit did not apply all replacements:\n%s", newStr)
	}
}

func TestMultiEditAtomicFailure(t *testing.T) {
	origContent := `package main

var alpha = 1
var beta = 2
`
	root := execWriteFixture(t, map[string]string{
		"main.go": origContent,
	})
	ws, _ := NewWorkspace(root)

	tool := NewMultiEditTool(ws)
	// 2nd edit will fail (non-existent string)
	call := Call{
		ID:   "call_multi_2",
		Name: "multi_edit",
		Arguments: []byte(`{
			"path": "main.go",
			"edits": [
				{"old_string": "var alpha = 1", "new_string": "var alpha = 100"},
				{"old_string": "var gamma = 3", "new_string": "var gamma = 300"}
			]
		}`),
	}

	res, err := tool.Execute(context.Background(), call)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected error result on partial match failure")
	}

	// Verify file was NOT modified (atomic guarantee)
	newBytes, _ := os.ReadFile(filepath.Join(root, "main.go"))
	if strings.Contains(string(newBytes), "100") {
		t.Errorf("file was modified despite atomic failure:\n%s", string(newBytes))
	}
}
