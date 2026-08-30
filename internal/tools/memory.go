package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/project"
)

// MemoryTool allows the model to read and record durable project knowledge in Boop.md.
type MemoryTool struct {
	ws *Workspace
}

// NewMemoryTool returns a memory tool confined to ws.
func NewMemoryTool(ws *Workspace) *MemoryTool {
	return &MemoryTool{ws: ws}
}

type memoryArgs struct {
	Action  string `json:"action"` // "read", "append", "update_current_work", "set"
	Section string `json:"section,omitempty"`
	Text    string `json:"text,omitempty"`
}

// MemoryData is the structured result payload of a memory tool call.
type MemoryData struct {
	Action  string `json:"action"`
	Section string `json:"section,omitempty"`
	Path    string `json:"path"`
}

// Name implements Tool.
func (t *MemoryTool) Name() string { return "memory" }

// Description implements Tool.
func (t *MemoryTool) Description() string {
	return "Read and record durable project knowledge in Boop.md (decisions, architecture, known problems, useful commands, current work). " +
		"Use 'read' to inspect memory, 'append' to record a decision, known problem or note, or 'update_current_work' to update active task status."
}

// Schema implements Tool.
func (t *MemoryTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type": "string",
				"enum": []string{"read", "append", "update_current_work", "set"},
				"description": "Action to perform: 'read' (read full Boop.md or section), 'append' (add entry to section), 'update_current_work' (replace Current Work), 'set' (replace section content).",
			},
			"section": map[string]any{
				"type":        "string",
				"description": "Target section name, e.g. 'Decisions', 'Known Problems', 'Useful Commands', 'Architecture', 'Current Work', 'Agent Notes'.",
			},
			"text": map[string]any{
				"type":        "string",
				"description": "Content to append or set in project memory.",
			},
		},
		"required":             []string{"action"},
		"additionalProperties": false,
	}
}

// Permission implements Tool.
func (t *MemoryTool) Permission(call Call) (permissions.Action, error) {
	var a memoryArgs
	if err := call.Bind(&a); err != nil {
		return permissions.Action{}, err
	}
	memPath := filepath.Join(t.ws.Root(), project.MemoryFileName)
	if a.Action == "read" {
		return permissions.Action{
			Category: permissions.CatFilesystemRead,
			Risk:     permissions.RiskLow,
			Tool:     t.Name(),
			Summary:  "read project memory (Boop.md)",
			Paths:    []string{memPath},
		}, nil
	}
	return permissions.Action{
		Category: permissions.CatFilesystemWrite,
		Risk:     permissions.RiskLow,
		Tool:     t.Name(),
		Summary:  fmt.Sprintf("update project memory in %s (%s)", project.MemoryFileName, a.Action),
		Detail:   a.Text,
		Paths:    []string{memPath},
	}, nil
}

// Execute performs the requested memory operation against Boop.md.
func (t *MemoryTool) Execute(ctx context.Context, call Call) (Result, error) {
	var a memoryArgs
	if err := call.Bind(&a); err != nil {
		return Errorf(call, "memory: %v", err), nil
	}

	mem, err := project.LoadOrCreate(t.ws.Root())
	if err != nil {
		return Errorf(call, "memory: cannot load project memory: %v", err), nil
	}

	switch a.Action {
	case "read":
		if a.Section != "" {
			secText := mem.Document().Text(a.Section)
			if secText == "" {
				return Result{
					CallID:  call.ID,
					Tool:    t.Name(),
					Content: fmt.Sprintf("Section %q in %s is empty or not found.", a.Section, project.MemoryFileName),
					Data:    MemoryData{Action: "read", Section: a.Section, Path: mem.Path()},
				}, nil
			}
			return Result{
				CallID:  call.ID,
				Tool:    t.Name(),
				Content: fmt.Sprintf("## %s\n\n%s", a.Section, secText),
				Data:    MemoryData{Action: "read", Section: a.Section, Path: mem.Path()},
			}, nil
		}
		return Result{
			CallID:  call.ID,
			Tool:    t.Name(),
			Content: string(mem.Render()),
			Data:    MemoryData{Action: "read", Path: mem.Path()},
		}, nil

	case "append":
		if strings.TrimSpace(a.Text) == "" {
			return Errorf(call, "memory: text must not be empty for append action"), nil
		}
		targetSection := a.Section
		if targetSection == "" {
			targetSection = project.SectionDecisions
		}

		switch strings.ToLower(targetSection) {
		case "decisions", "decision":
			mem.AppendDecision(a.Text)
		case "known problems", "known problem", "problems", "problem":
			mem.RecordKnownProblem(a.Text)
		default:
			bullet := a.Text
			if !strings.HasPrefix(strings.TrimSpace(bullet), "-") && !strings.HasPrefix(strings.TrimSpace(bullet), "*") {
				bullet = "- " + bullet
			}
			mem.Document().AppendText(targetSection, bullet)
		}

		if err := mem.Save(); err != nil {
			return Errorf(call, "memory: failed to save %s: %v", project.MemoryFileName, err), nil
		}

		return Result{
			CallID:  call.ID,
			Tool:    t.Name(),
			Content: fmt.Sprintf("Appended knowledge to section %q in %s.", targetSection, project.MemoryFileName),
			Data:    MemoryData{Action: "append", Section: targetSection, Path: mem.Path()},
			Display: fmt.Sprintf("appended to %s", targetSection),
		}, nil

	case "update_current_work":
		if strings.TrimSpace(a.Text) == "" {
			return Errorf(call, "memory: text must not be empty for update_current_work"), nil
		}
		mem.SetCurrentWork(a.Text)
		if err := mem.Save(); err != nil {
			return Errorf(call, "memory: failed to save %s: %v", project.MemoryFileName, err), nil
		}
		return Result{
			CallID:  call.ID,
			Tool:    t.Name(),
			Content: fmt.Sprintf("Updated %s section in %s.", project.SectionCurrentWork, project.MemoryFileName),
			Data:    MemoryData{Action: "update_current_work", Section: project.SectionCurrentWork, Path: mem.Path()},
			Display: "updated Current Work",
		}, nil

	case "set":
		if a.Section == "" {
			return Errorf(call, "memory: section must be specified for set action"), nil
		}
		mem.Document().SetText(a.Section, a.Text)
		if err := mem.Save(); err != nil {
			return Errorf(call, "memory: failed to save %s: %v", project.MemoryFileName, err), nil
		}
		return Result{
			CallID:  call.ID,
			Tool:    t.Name(),
			Content: fmt.Sprintf("Set section %q in %s.", a.Section, project.MemoryFileName),
			Data:    MemoryData{Action: "set", Section: a.Section, Path: mem.Path()},
			Display: fmt.Sprintf("set %s", a.Section),
		}, nil

	default:
		return Errorf(call, "memory: unknown action %q (supported: read, append, update_current_work, set)", a.Action), nil
	}
}
