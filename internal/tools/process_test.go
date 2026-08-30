package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestProcessToolLifecycle(t *testing.T) {
	root := t.TempDir()
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}

	mgr := NewProcessManager(ws)
	tool := NewProcessTool(mgr)
	defer mgr.StopAll()

	// 1. Start background echo loop
	res, err := tool.Execute(context.Background(), Call{
		ID:   "call_bg_1",
		Name: "process",
		Arguments: []byte(`{
			"action": "start",
			"command": "echo test_bg_output"
		}`),
	})
	if err != nil || res.IsError {
		t.Fatalf("start failed: %v, content: %s", err, res.Content)
	}

	time.Sleep(100 * time.Millisecond)

	// 2. Fetch logs
	res, err = tool.Execute(context.Background(), Call{
		ID:   "call_bg_2",
		Name: "process",
		Arguments: []byte(`{
			"action": "logs",
			"id": "bg-1"
		}`),
	})
	if err != nil || res.IsError {
		t.Fatalf("logs failed: %v, content: %s", err, res.Content)
	}
	if !strings.Contains(res.Content, "test_bg_output") {
		t.Errorf("logs output missing 'test_bg_output':\n%s", res.Content)
	}

	// 3. Stop
	res, err = tool.Execute(context.Background(), Call{
		ID:   "call_bg_3",
		Name: "process",
		Arguments: []byte(`{
			"action": "stop",
			"id": "bg-1"
		}`),
	})
	if err != nil || res.IsError {
		t.Fatalf("stop failed: %v, content: %s", err, res.Content)
	}
}
