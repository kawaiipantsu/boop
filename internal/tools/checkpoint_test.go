package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckpointCreateAndRevert(t *testing.T) {
	root := execWriteFixture(t, map[string]string{
		"file1.txt": "original text 1",
		"file2.txt": "original text 2",
	})
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}

	mgr := NewCheckpointManager(ws)
	tool := NewCheckpointTool(mgr)

	// 1. Create checkpoint
	res, err := tool.Execute(context.Background(), Call{
		ID:   "call_cp_1",
		Name: "checkpoint",
		Arguments: []byte(`{
			"action": "create",
			"description": "before refactor",
			"paths": ["file1.txt", "file2.txt"]
		}`),
	})
	if err != nil || res.IsError {
		t.Fatalf("checkpoint create failed: %v, content: %s", err, res.Content)
	}

	// 2. Modify files
	_ = os.WriteFile(filepath.Join(root, "file1.txt"), []byte("damaged text 1"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "file2.txt"), []byte("damaged text 2"), 0o644)

	// 3. Revert checkpoint
	res, err = tool.Execute(context.Background(), Call{
		ID:   "call_cp_2",
		Name: "checkpoint",
		Arguments: []byte(`{
			"action": "revert",
			"id": "cp-1"
		}`),
	})
	if err != nil || res.IsError {
		t.Fatalf("checkpoint revert failed: %v, content: %s", err, res.Content)
	}

	// 4. Verify original text restored
	b1, _ := os.ReadFile(filepath.Join(root, "file1.txt"))
	b2, _ := os.ReadFile(filepath.Join(root, "file2.txt"))
	if string(b1) != "original text 1" || string(b2) != "original text 2" {
		t.Errorf("files not reverted properly: f1=%q, f2=%q", string(b1), string(b2))
	}
}
