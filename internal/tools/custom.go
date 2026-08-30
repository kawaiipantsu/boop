package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/kawaiipantsu/boop/internal/config"
	"github.com/kawaiipantsu/boop/internal/permissions"
)

// CustomTool implements Tool for user-defined tools declared in config.yaml.
type CustomTool struct {
	name        string
	description string
	command     []string
	schema      map[string]any
	permission  config.CustomToolPermission
	timeout     time.Duration
}

// NewCustomTool creates a new CustomTool from configuration.
func NewCustomTool(name string, cfg config.CustomToolConfig) *CustomTool {
	timeout := cfg.Timeout.Std()
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	schema := cfg.Schema
	if schema == nil {
		schema = map[string]any{
			"type": "object",
		}
	}
	return &CustomTool{
		name:        name,
		description: cfg.Description,
		command:     cfg.Command,
		schema:      schema,
		permission:  cfg.Permission,
		timeout:     timeout,
	}
}

// Name returns the identifier of the custom tool.
func (t *CustomTool) Name() string {
	return t.name
}

// Description returns the tool summary visible to LLM models.
func (t *CustomTool) Description() string {
	if t.description != "" {
		return t.description
	}
	return fmt.Sprintf("Run user-defined tool: %s", t.name)
}

// Schema returns the JSON schema parameter specification.
func (t *CustomTool) Schema() map[string]any {
	return t.schema
}

// Permission classifies the custom tool using its user-declared category and risk.
func (t *CustomTool) Permission(call Call) (permissions.Action, error) {
	category := t.permission.Category
	if category == "" {
		category = permissions.CatShellExecute
	}
	risk := t.permission.Risk
	if risk == "" {
		risk = permissions.RiskMedium
	}

	summary := fmt.Sprintf("Run custom tool %s: %s", t.name, strings.Join(t.command, " "))
	return permissions.Action{
		Tool:     t.name,
		Category: category,
		Risk:     risk,
		Summary:  summary,
		Detail:   fmt.Sprintf("Execute user-declared custom tool %q", t.name),
	}, nil
}

// Execute runs the declared command directly without shell interpolation.
func (t *CustomTool) Execute(ctx context.Context, call Call) (Result, error) {
	if len(t.command) == 0 {
		return Errorf(call, "no command configured for custom tool %s", t.name), nil
	}

	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	cmdName := t.command[0]
	cmdArgs := make([]string, len(t.command)-1)
	copy(cmdArgs, t.command[1:])

	// Append passed arguments as CLI flags safely without shell escaping issues
	if len(call.Arguments) > 0 && string(call.Arguments) != "{}" && string(call.Arguments) != "null" {
		var argsMap map[string]any
		if err := json.Unmarshal(call.Arguments, &argsMap); err == nil {
			for k, v := range argsMap {
				cmdArgs = append(cmdArgs, fmt.Sprintf("--%s=%v", k, v))
			}
		}
	}

	start := time.Now()
	cmd := exec.CommandContext(ctx, cmdName, cmdArgs...)
	out, err := cmd.CombinedOutput()
	duration := time.Since(start)

	if err != nil {
		return Result{
			CallID:   call.ID,
			Tool:     t.name,
			Content:  fmt.Sprintf("execution error: %v\nOutput:\n%s", err, string(out)),
			IsError:  true,
			Duration: duration,
		}, nil
	}

	return Result{
		CallID:   call.ID,
		Tool:     t.name,
		Content:  string(out),
		IsError:  false,
		Display:  fmt.Sprintf("exit 0 (%d bytes)", len(out)),
		Duration: duration,
	}, nil
}

// RegisterCustomTools registers all custom tools from configuration into the Registry.
func (r *Registry) RegisterCustomTools(customTools map[string]config.CustomToolConfig) {
	for name, cfg := range customTools {
		trimmedName := strings.TrimSpace(name)
		if trimmedName == "" {
			continue
		}
		r.Register(NewCustomTool(trimmedName, cfg))
	}
}
