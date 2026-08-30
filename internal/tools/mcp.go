package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kawaiipantsu/boop/internal/mcp"
	"github.com/kawaiipantsu/boop/internal/permissions"
)

// MCPTool adapts an MCP server tool definition into Boop's Tool interface.
type MCPTool struct {
	Client     *mcp.Client
	ServerName string
	ToolName   string
	fullName   string
	desc       string
	schema     map[string]any
}

// NewMCPTool creates a Tool backed by an MCP client.
func NewMCPTool(client *mcp.Client, serverName string, def mcp.ToolDefinition) *MCPTool {
	fullName := fmt.Sprintf("%s__%s", serverName, def.Name)
	return &MCPTool{
		Client:     client,
		ServerName: serverName,
		ToolName:   def.Name,
		fullName:   fullName,
		desc:       def.Description,
		schema:     def.InputSchema,
	}
}

// Name implements Tool.
func (t *MCPTool) Name() string { return t.fullName }

// Description implements Tool.
func (t *MCPTool) Description() string {
	if t.desc != "" {
		return fmt.Sprintf("[MCP %s] %s", t.ServerName, t.desc)
	}
	return fmt.Sprintf("[MCP %s] %s tool", t.ServerName, t.ToolName)
}

// Schema implements Tool.
func (t *MCPTool) Schema() map[string]any {
	if t.schema != nil {
		return t.schema
	}
	return map[string]any{
		"type": "object",
	}
}

// Permission implements Tool.
func (t *MCPTool) Permission(call Call) (permissions.Action, error) {
	return permissions.Action{
		Category: permissions.CatMCP,
		Risk:     permissions.RiskMedium,
		Tool:     t.Name(),
		Summary:  fmt.Sprintf("Call MCP tool %s on server %s", t.ToolName, t.ServerName),
		Detail:   string(call.Arguments),
	}, nil
}

// Execute invokes the MCP tool on the server.
func (t *MCPTool) Execute(ctx context.Context, call Call) (Result, error) {
	var args map[string]any
	if len(call.Arguments) > 0 {
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return Errorf(call, "mcp tool %s: invalid JSON arguments: %v", t.Name(), err), nil
		}
	}
	if args == nil {
		args = make(map[string]any)
	}

	res, err := t.Client.CallTool(ctx, t.ToolName, args)
	if err != nil {
		return Errorf(call, "mcp tool %s call failed: %v", t.Name(), err), nil
	}

	var sb strings.Builder
	for _, c := range res.Content {
		if c.Text != "" {
			sb.WriteString(c.Text)
		}
	}
	content := sb.String()
	if content == "" {
		content = "(empty response)"
	}

	if res.IsError {
		return Result{
			CallID:  call.ID,
			Tool:    t.Name(),
			Content: content,
			IsError: true,
		}, nil
	}

	return Result{
		CallID:  call.ID,
		Tool:    t.Name(),
		Content: content,
	}, nil
}
