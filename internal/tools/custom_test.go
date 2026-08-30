package tools

import (
	"context"
	"encoding/json"
	"runtime"
	"testing"
	"time"

	"github.com/kawaiipantsu/boop/internal/config"
	"github.com/kawaiipantsu/boop/internal/permissions"
)

func TestCustomToolPropertiesAndPermission(t *testing.T) {
	cfg := config.CustomToolConfig{
		Description: "Deploy branch to staging",
		Command:     []string{"./deploy.sh", "staging"},
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"skip_tests": map[string]any{"type": "boolean"},
			},
		},
		Permission: config.CustomToolPermission{
			Category: permissions.CatProductionChange,
			Risk:     permissions.RiskCritical,
		},
		Timeout: config.Duration(10 * time.Second),
	}

	tool := NewCustomTool("deploy_staging", cfg)

	if tool.Name() != "deploy_staging" {
		t.Errorf("Name = %q, want %q", tool.Name(), "deploy_staging")
	}
	if tool.Description() != "Deploy branch to staging" {
		t.Errorf("Description = %q, want %q", tool.Description(), "Deploy branch to staging")
	}
	if tool.Schema()["type"] != "object" {
		t.Errorf("Schema[type] = %v, want object", tool.Schema()["type"])
	}

	perm, err := tool.Permission(Call{Name: "deploy_staging"})
	if err != nil {
		t.Fatalf("Permission error: %v", err)
	}
	if perm.Category != permissions.CatProductionChange {
		t.Errorf("Category = %v, want %v", perm.Category, permissions.CatProductionChange)
	}
	if perm.Risk != permissions.RiskCritical {
		t.Errorf("Risk = %v, want %v", perm.Risk, permissions.RiskCritical)
	}
}

func TestCustomToolExecution(t *testing.T) {
	var cmd []string
	if runtime.GOOS == "windows" {
		cmd = []string{"cmd.exe", "/c", "echo", "custom tool output"}
	} else {
		cmd = []string{"echo", "custom tool output"}
	}

	cfg := config.CustomToolConfig{
		Description: "Echo test tool",
		Command:     cmd,
	}

	tool := NewCustomTool("echo_test", cfg)
	call := Call{
		ID:        "call_1",
		Name:      "echo_test",
		Arguments: json.RawMessage(`{}`),
	}

	res, err := tool.Execute(context.Background(), call)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if res.IsError {
		t.Fatalf("Expected success, got error: %s", res.Content)
	}
	if res.Tool != "echo_test" {
		t.Errorf("Tool = %q, want echo_test", res.Tool)
	}
}

func TestRegistryCustomToolsIntegration(t *testing.T) {
	reg := NewRegistry()
	customConfigs := map[string]config.CustomToolConfig{
		"my_script": {
			Description: "Run my custom script",
			Command:     []string{"node", "index.js"},
		},
	}

	reg.RegisterCustomTools(customConfigs)

	tool, ok := reg.Get("my_script")
	if !ok {
		t.Fatalf("my_script not found in registry")
	}
	if tool.Name() != "my_script" {
		t.Errorf("Name = %q, want my_script", tool.Name())
	}
}
