package tools

import (
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/mcp"
)

func TestMCPToolProperties(t *testing.T) {
	def := mcp.ToolDefinition{
		Name:        "query_db",
		Description: "Run SQL query on database",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"sql": map[string]any{"type": "string"},
			},
		},
	}

	tool := NewMCPTool(nil, "postgres", def)
	if tool.Name() != "postgres__query_db" {
		t.Errorf("tool.Name() = %q, want 'postgres__query_db'", tool.Name())
	}
	if !strings.Contains(tool.Description(), "[MCP postgres]") {
		t.Errorf("tool.Description() missing [MCP postgres]: %s", tool.Description())
	}

	act, err := tool.Permission(Call{
		ID:        "call_1",
		Name:      "postgres__query_db",
		Arguments: []byte(`{"sql": "SELECT 1"}`),
	})
	if err != nil {
		t.Fatalf("Permission error: %v", err)
	}
	if act.Category != "mcp.call" {
		t.Errorf("Category = %q, want 'mcp.call'", act.Category)
	}
}
