package tools

import (
	"context"
	"strings"
	"testing"
)

func TestTodoToolOperations(t *testing.T) {
	tool := NewTodoTool(nil)

	// Test set
	res, err := tool.Execute(context.Background(), Call{
		ID:   "call_todo_1",
		Name: "todo",
		Arguments: []byte(`{
			"action": "set",
			"tasks": [
				{"id": "1", "title": "First task", "status": "done"},
				{"id": "2", "title": "Second task", "status": "pending"}
			]
		}`),
	})
	if err != nil || res.IsError {
		t.Fatalf("todo set failed: %v, content: %s", err, res.Content)
	}
	if !strings.Contains(res.Content, "[x] 1. First task") || !strings.Contains(res.Content, "[ ] 2. Second task") {
		t.Errorf("todo set content incorrect:\n%s", res.Content)
	}

	// Test add
	res, err = tool.Execute(context.Background(), Call{
		ID:   "call_todo_2",
		Name: "todo",
		Arguments: []byte(`{
			"action": "add",
			"title": "Third task",
			"status": "in_progress"
		}`),
	})
	if err != nil || res.IsError {
		t.Fatalf("todo add failed: %v, content: %s", err, res.Content)
	}
	if !strings.Contains(res.Content, "[>] 3. Third task") {
		t.Errorf("todo add content missing item:\n%s", res.Content)
	}

	// Test update
	res, err = tool.Execute(context.Background(), Call{
		ID:   "call_todo_3",
		Name: "todo",
		Arguments: []byte(`{
			"action": "update",
			"id": "2",
			"status": "done"
		}`),
	})
	if err != nil || res.IsError {
		t.Fatalf("todo update failed: %v, content: %s", err, res.Content)
	}
	if !strings.Contains(res.Content, "[x] 2. Second task") {
		t.Errorf("todo update content incorrect:\n%s", res.Content)
	}

	// Test list
	res, err = tool.Execute(context.Background(), Call{
		ID:        "call_todo_4",
		Name:      "todo",
		Arguments: []byte(`{"action": "list"}`),
	})
	if err != nil || res.IsError {
		t.Fatalf("todo list failed: %v, content: %s", err, res.Content)
	}
	if !strings.Contains(res.Content, "Tasks (2/3 completed)") {
		t.Errorf("todo list header incorrect:\n%s", res.Content)
	}
}
