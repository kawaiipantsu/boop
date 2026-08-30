package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/project"
)

func TestMemoryToolReadAndAppend(t *testing.T) {
	root := execWriteFixture(t, map[string]string{
		"Boop.md": "# Boop Project Memory\n\n## Decisions\n\n- 2026-01-01 — Initial decision\n",
	})
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}

	tool := NewMemoryTool(ws)

	// Test read full
	res, err := tool.Execute(context.Background(), Call{
		ID:   "call_mem_1",
		Name: "memory",
		Arguments: []byte(`{"action": "read"}`),
	})
	if err != nil || res.IsError {
		t.Fatalf("memory read failed: %v, content: %s", err, res.Content)
	}
	if !strings.Contains(res.Content, "Initial decision") {
		t.Errorf("read content missing decision:\n%s", res.Content)
	}

	// Test append decision
	res, err = tool.Execute(context.Background(), Call{
		ID:   "call_mem_2",
		Name: "memory",
		Arguments: []byte(`{"action": "append", "section": "Decisions", "text": "Switched to SQLite WAL mode"}`),
	})
	if err != nil || res.IsError {
		t.Fatalf("memory append failed: %v, content: %s", err, res.Content)
	}

	// Verify file on disk
	content, err := os.ReadFile(filepath.Join(root, project.MemoryFileName))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(content), "Switched to SQLite WAL mode") {
		t.Errorf("Boop.md missing appended decision:\n%s", string(content))
	}

	// Test update_current_work
	res, err = tool.Execute(context.Background(), Call{
		ID:   "call_mem_3",
		Name: "memory",
		Arguments: []byte(`{"action": "update_current_work", "text": "Refactoring tool registry"}`),
	})
	if err != nil || res.IsError {
		t.Fatalf("memory update_current_work failed: %v, content: %s", err, res.Content)
	}

	content, _ = os.ReadFile(filepath.Join(root, project.MemoryFileName))
	if !strings.Contains(string(content), "Refactoring tool registry") {
		t.Errorf("Boop.md missing current work:\n%s", string(content))
	}
}
