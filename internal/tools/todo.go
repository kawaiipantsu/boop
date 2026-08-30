package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/kawaiipantsu/boop/internal/permissions"
)

// TaskStatus represents the progress status of a task item.
type TaskStatus string

const (
	StatusPending    TaskStatus = "pending"
	StatusInProgress TaskStatus = "in_progress"
	StatusDone       TaskStatus = "done"
	StatusBlocked    TaskStatus = "blocked"
	StatusCancelled  TaskStatus = "cancelled"
)

// TaskItem represents an individual task in a session's working memory.
type TaskItem struct {
	ID     string     `json:"id"`
	Title  string     `json:"title"`
	Status TaskStatus `json:"status"`
}

// TodoStore holds the working task list in memory for the current session.
type TodoStore struct {
	mu    sync.RWMutex
	tasks []TaskItem
}

// NewTodoStore creates a fresh in-memory task store.
func NewTodoStore() *TodoStore {
	return &TodoStore{tasks: []TaskItem{}}
}

// Set replaces the task list.
func (s *TodoStore) Set(tasks []TaskItem) []TaskItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks = make([]TaskItem, len(tasks))
	for i, t := range tasks {
		if t.ID == "" {
			t.ID = fmt.Sprintf("%d", i+1)
		}
		if t.Status == "" {
			t.Status = StatusPending
		}
		s.tasks[i] = t
	}
	return append([]TaskItem(nil), s.tasks...)
}

// Add appends a new task item.
func (s *TodoStore) Add(item TaskItem) TaskItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	if item.ID == "" {
		item.ID = fmt.Sprintf("%d", len(s.tasks)+1)
	}
	if item.Status == "" {
		item.Status = StatusPending
	}
	s.tasks = append(s.tasks, item)
	return item
}

// Update modifies an existing task by ID.
func (s *TodoStore) Update(id string, status TaskStatus, title string) (TaskItem, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.tasks {
		if s.tasks[i].ID == id {
			if status != "" {
				s.tasks[i].Status = status
			}
			if title != "" {
				s.tasks[i].Title = title
			}
			return s.tasks[i], true
		}
	}
	return TaskItem{}, false
}

// List returns a copy of current tasks.
func (s *TodoStore) List() []TaskItem {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]TaskItem(nil), s.tasks...)
}

// TodoTool owns a visible task list for tracking multi-step work.
type TodoTool struct {
	store *TodoStore
}

// NewTodoTool creates a todo tool backed by a shared or new store.
func NewTodoTool(store *TodoStore) *TodoTool {
	if store == nil {
		store = NewTodoStore()
	}
	return &TodoTool{store: store}
}

type todoArgs struct {
	Action string     `json:"action"` // "set", "add", "update", "list"
	Tasks  []TaskItem `json:"tasks,omitempty"`
	ID     string     `json:"id,omitempty"`
	Status TaskStatus `json:"status,omitempty"`
	Title  string     `json:"title,omitempty"`
}

// TodoData is the structured result payload of a todo tool call.
type TodoData struct {
	Action string     `json:"action"`
	Tasks  []TaskItem `json:"tasks"`
	Total  int        `json:"total"`
	Done   int        `json:"done"`
}

// Name implements Tool.
func (t *TodoTool) Name() string { return "todo" }

// Description implements Tool.
func (t *TodoTool) Description() string {
	return "Manage a visible task list for tracking multi-step work in the current session. " +
		"Use 'set' or 'add' to define tasks, 'update' to mark progress (pending, in_progress, done, blocked, cancelled), and 'list' to view tasks."
}

// Schema implements Tool.
func (t *TodoTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"set", "add", "update", "list"},
				"description": "Action: 'set' (replace task list), 'add' (append task), 'update' (change status/title of a task), 'list' (view tasks).",
			},
			"tasks": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":     map[string]any{"type": "string"},
						"title":  map[string]any{"type": "string"},
						"status": map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "done", "blocked", "cancelled"}},
					},
					"required": []string{"title"},
				},
				"description": "List of tasks for 'set' action.",
			},
			"id": map[string]any{
				"type":        "string",
				"description": "Task identifier for 'update' action.",
			},
			"status": map[string]any{
				"type":        "string",
				"enum":        []string{"pending", "in_progress", "done", "blocked", "cancelled"},
				"description": "New status for 'update' or initial status for 'add'.",
			},
			"title": map[string]any{
				"type":        "string",
				"description": "Task title for 'add' or updated title for 'update'.",
			},
		},
		"required":             []string{"action"},
		"additionalProperties": false,
	}
}

// Permission implements Tool.
func (t *TodoTool) Permission(call Call) (permissions.Action, error) {
	var a todoArgs
	if err := call.Bind(&a); err != nil {
		return permissions.Action{}, err
	}
	return permissions.Action{
		Category: permissions.CatFilesystemRead,
		Risk:     permissions.RiskLow,
		Tool:     t.Name(),
		Summary:  fmt.Sprintf("manage session tasks (%s)", a.Action),
	}, nil
}

// Execute handles todo operations.
func (t *TodoTool) Execute(ctx context.Context, call Call) (Result, error) {
	var a todoArgs
	if err := call.Bind(&a); err != nil {
		return Errorf(call, "todo: %v", err), nil
	}

	switch a.Action {
	case "set":
		tasks := t.store.Set(a.Tasks)
		return t.renderResult(call, "set", tasks), nil

	case "add":
		if strings.TrimSpace(a.Title) == "" {
			return Errorf(call, "todo: title must not be empty for add action"), nil
		}
		t.store.Add(TaskItem{
			ID:     a.ID,
			Title:  a.Title,
			Status: a.Status,
		})
		return t.renderResult(call, "add", t.store.List()), nil

	case "update":
		if a.ID == "" {
			return Errorf(call, "todo: id must be specified for update action"), nil
		}
		item, ok := t.store.Update(a.ID, a.Status, a.Title)
		if !ok {
			return Errorf(call, "todo: task with id %q not found", a.ID), nil
		}
		_ = item
		return t.renderResult(call, "update", t.store.List()), nil

	case "list":
		return t.renderResult(call, "list", t.store.List()), nil

	default:
		return Errorf(call, "todo: unknown action %q (supported: set, add, update, list)", a.Action), nil
	}
}

func (t *TodoTool) renderResult(call Call, action string, tasks []TaskItem) Result {
	doneCount := 0
	for _, task := range tasks {
		if task.Status == StatusDone {
			doneCount++
		}
	}

	var sb strings.Builder
	if len(tasks) == 0 {
		sb.WriteString("Task list is empty.")
	} else {
		fmt.Fprintf(&sb, "Tasks (%d/%d completed):\n", doneCount, len(tasks))
		for _, task := range tasks {
			mark := "[ ]"
			switch task.Status {
			case StatusDone:
				mark = "[x]"
			case StatusInProgress:
				mark = "[>]"
			case StatusBlocked:
				mark = "[!]"
			case StatusCancelled:
				mark = "[-]"
			}
			statusSuffix := ""
			if task.Status != StatusPending && task.Status != StatusDone {
				statusSuffix = fmt.Sprintf(" (%s)", task.Status)
			}
			fmt.Fprintf(&sb, "%s %s. %s%s\n", mark, task.ID, task.Title, statusSuffix)
		}
	}

	summary := strings.TrimSpace(sb.String())
	return Result{
		CallID:  call.ID,
		Tool:    t.Name(),
		Content: summary,
		Data: TodoData{
			Action: action,
			Tasks:  tasks,
			Total:  len(tasks),
			Done:   doneCount,
		},
		Display: fmt.Sprintf("%d/%d tasks done", doneCount, len(tasks)),
	}
}
