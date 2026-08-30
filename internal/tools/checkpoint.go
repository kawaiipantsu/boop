package tools

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kawaiipantsu/boop/internal/permissions"
)

// DefaultMaxCheckpoints caps retained snapshots to prevent unbounded memory growth.
const DefaultMaxCheckpoints = 50

// FileSnapshot captures the contents and metadata of a single file.
type FileSnapshot struct {
	RelPath  string      `json:"rel_path"`
	Content  string      `json:"content"`
	Perm     os.FileMode `json:"perm"`
	Existed  bool        `json:"existed"`
}

// Checkpoint represents a saved workspace state at a point in time.
type Checkpoint struct {
	ID          string                  `json:"id"`
	Description string                  `json:"description"`
	CreatedAt   time.Time               `json:"created_at"`
	Files       map[string]FileSnapshot `json:"files"`
}

// CheckpointManager manages in-memory rollback snapshots for a session.
type CheckpointManager struct {
	ws    *Workspace
	mu    sync.RWMutex
	order []string
	items map[string]Checkpoint
	seq   int
}

// NewCheckpointManager creates a manager for ws.
func NewCheckpointManager(ws *Workspace) *CheckpointManager {
	return &CheckpointManager{
		ws:    ws,
		items: make(map[string]Checkpoint),
	}
}

// Create captures the current state of the given workspace paths.
func (cm *CheckpointManager) Create(description string, paths []string) (Checkpoint, error) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	cm.seq++
	id := fmt.Sprintf("cp-%d", cm.seq)
	if description == "" {
		description = fmt.Sprintf("Checkpoint %d", cm.seq)
	}

	files := make(map[string]FileSnapshot)
	for _, p := range paths {
		abs, err := cm.ws.Resolve(p)
		if err != nil {
			continue
		}
		rel := cm.ws.Rel(abs)
		info, err := os.Stat(abs)
		if os.IsNotExist(err) {
			files[rel] = FileSnapshot{RelPath: rel, Existed: false}
			continue
		}
		if err != nil || info.IsDir() {
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		files[rel] = FileSnapshot{
			RelPath: rel,
			Content: string(data),
			Perm:    info.Mode().Perm(),
			Existed: true,
		}
	}

	cp := Checkpoint{
		ID:          id,
		Description: description,
		CreatedAt:   time.Now(),
		Files:       files,
	}

	cm.items[id] = cp
	cm.order = append(cm.order, id)

	if len(cm.order) > DefaultMaxCheckpoints {
		oldest := cm.order[0]
		delete(cm.items, oldest)
		cm.order = cm.order[1:]
	}

	return cp, nil
}

// List returns all active checkpoints.
func (cm *CheckpointManager) List() []Checkpoint {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	out := make([]Checkpoint, 0, len(cm.order))
	for _, id := range cm.order {
		out = append(out, cm.items[id])
	}
	return out
}

// Revert restores files from the checkpoint id.
func (cm *CheckpointManager) Revert(id string, pathFilter string) ([]string, error) {
	cm.mu.RLock()
	cp, ok := cm.items[id]
	cm.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("checkpoint %q not found", id)
	}

	var restored []string
	for rel, snap := range cp.Files {
		if pathFilter != "" && rel != pathFilter {
			continue
		}
		abs, err := cm.ws.Resolve(rel)
		if err != nil {
			return nil, err
		}

		if !snap.Existed {
			_ = os.Remove(abs)
			restored = append(restored, fmt.Sprintf("removed created file %s", rel))
			continue
		}

		perm := snap.Perm
		if perm == 0 {
			perm = 0o644
		}
		if err := fsAtomicWrite(abs, []byte(snap.Content), perm); err != nil {
			return nil, fmt.Errorf("revert %s: %w", rel, err)
		}
		restored = append(restored, fmt.Sprintf("restored %s", rel))
	}

	return restored, nil
}

// CheckpointTool exposes checkpoint and revert capabilities to the model.
type CheckpointTool struct {
	Manager *CheckpointManager
}

// NewCheckpointTool creates a CheckpointTool backed by manager.
func NewCheckpointTool(manager *CheckpointManager) *CheckpointTool {
	return &CheckpointTool{Manager: manager}
}

type checkpointArgs struct {
	Action      string   `json:"action"`
	ID          string   `json:"id,omitempty"`
	Description string   `json:"description,omitempty"`
	Paths       []string `json:"paths,omitempty"`
}

// Name implements Tool.
func (t *CheckpointTool) Name() string { return "checkpoint" }

// Description implements Tool.
func (t *CheckpointTool) Description() string {
	return "Manage workspace snapshots and undo agent edits. Actions: create, list, revert. " +
		"Use revert to restore files to a known good state after a failing edit."
}

// Schema implements Tool.
func (t *CheckpointTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"create", "list", "revert"},
				"description": "Action to perform: create a snapshot, list existing snapshots, or revert to a snapshot.",
			},
			"id": map[string]any{
				"type":        "string",
				"description": "Checkpoint ID for revert (e.g. 'cp-1').",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "Human description when creating a snapshot.",
			},
			"paths": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "string",
				},
				"description": "File paths to include in the snapshot (for create) or filter (for revert).",
			},
		},
		"required":             []string{"action"},
		"additionalProperties": false,
	}
}

// Permission implements Tool.
func (t *CheckpointTool) Permission(call Call) (permissions.Action, error) {
	var a checkpointArgs
	if err := call.Bind(&a); err != nil {
		return permissions.Action{}, err
	}
	if a.Action == "revert" {
		return permissions.Action{
			Category: permissions.CatFilesystemWrite,
			Risk:     permissions.RiskMedium,
			Tool:     t.Name(),
			Summary:  fmt.Sprintf("Revert workspace to checkpoint %s", a.ID),
			Detail:   fmt.Sprintf("Restoring snapshot %s", a.ID),
		}, nil
	}
	return permissions.Action{
		Category: permissions.CatFilesystemRead,
		Risk:     permissions.RiskLow,
		Tool:     t.Name(),
		Summary:  fmt.Sprintf("Checkpoint %s", a.Action),
	}, nil
}

// Execute performs the checkpoint operation.
func (t *CheckpointTool) Execute(ctx context.Context, call Call) (Result, error) {
	var a checkpointArgs
	if err := call.Bind(&a); err != nil {
		return Errorf(call, "checkpoint: %v", err), nil
	}

	switch strings.ToLower(a.Action) {
	case "create":
		if len(a.Paths) == 0 {
			return Errorf(call, "checkpoint create: paths array is required"), nil
		}
		cp, err := t.Manager.Create(a.Description, a.Paths)
		if err != nil {
			return Errorf(call, "checkpoint create: %v", err), nil
		}
		return Result{
			CallID:  call.ID,
			Tool:    t.Name(),
			Content: fmt.Sprintf("Created checkpoint %s (%d files snapshotted): %s", cp.ID, len(cp.Files), cp.Description),
			Display: fmt.Sprintf("checkpoint %s created", cp.ID),
		}, nil

	case "list":
		list := t.Manager.List()
		if len(list) == 0 {
			return Result{
				CallID:  call.ID,
				Tool:    t.Name(),
				Content: "No checkpoints recorded for this session.",
				Display: "no checkpoints",
			}, nil
		}
		var sb strings.Builder
		sb.WriteString("Checkpoints:\n")
		for _, cp := range list {
			fmt.Fprintf(&sb, "- %s (%s): %s [%d files]\n",
				cp.ID, cp.CreatedAt.Format("15:04:05"), cp.Description, len(cp.Files))
		}
		return Result{
			CallID:  call.ID,
			Tool:    t.Name(),
			Content: strings.TrimSpace(sb.String()),
			Display: fmt.Sprintf("%d checkpoints", len(list)),
		}, nil

	case "revert":
		if a.ID == "" {
			return Errorf(call, "checkpoint revert: id is required (e.g. 'cp-1')"), nil
		}
		filter := ""
		if len(a.Paths) > 0 {
			filter = a.Paths[0]
		}
		restored, err := t.Manager.Revert(a.ID, filter)
		if err != nil {
			return Errorf(call, "checkpoint revert: %v", err), nil
		}
		return Result{
			CallID:  call.ID,
			Tool:    t.Name(),
			Content: fmt.Sprintf("Reverted to checkpoint %s:\n- %s", a.ID, strings.Join(restored, "\n- ")),
			Display: fmt.Sprintf("reverted to %s", a.ID),
		}, nil

	default:
		return Errorf(call, "unknown action %q; use create, list, or revert", a.Action), nil
	}
}
