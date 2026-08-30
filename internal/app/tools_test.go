package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/config"
	"github.com/kawaiipantsu/boop/internal/execution"
	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/tools"
)

type stubExecutor struct{}

func (stubExecutor) Run(context.Context, execution.RunRequest) (execution.RunResult, error) {
	return execution.RunResult{}, nil
}

func testDeps(t *testing.T) ToolDeps {
	t.Helper()
	ws, err := tools.NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ToolDeps{Workspace: ws, Executor: stubExecutor{}}
}

func runCall(t *testing.T, command string) tools.Call {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"command": command})
	if err != nil {
		t.Fatal(err)
	}
	return tools.Call{ID: "c1", Name: "run", Arguments: raw}
}

// The run tool's built-in fallback classifier reports every command as
// shell.execute with production false. Wiring the real classifier is what
// makes the production gate reachable through the tool path — without it,
// `terraform apply` in auto mode would never be confirmed.
func TestBuildToolsWiresTheRealClassifier(t *testing.T) {
	reg, err := BuildTools(config.Default(), testDeps(t))
	if err != nil {
		t.Fatalf("BuildTools() = %v", err)
	}
	run, ok := reg.Get("run")
	if !ok {
		t.Fatal("run tool not registered")
	}

	tests := []struct {
		name       string
		command    string
		wantCat    permissions.Category
		wantRisk   permissions.Risk
		production bool
	}{
		{"production deploy", "terraform apply -auto-approve", permissions.CatProductionChange, permissions.RiskCritical, true},
		{"force push", "git push --force origin main", permissions.CatGitPush, permissions.RiskCritical, true},
		{"ordinary build", "go build ./...", permissions.CatShellExecute, permissions.RiskLow, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			act, err := run.Permission(runCall(t, tc.command))
			if err != nil {
				t.Fatalf("Permission() = %v", err)
			}
			if act.Category != tc.wantCat {
				t.Errorf("Category = %s, want %s", act.Category, tc.wantCat)
			}
			if act.Risk != tc.wantRisk {
				t.Errorf("Risk = %s, want %s", act.Risk, tc.wantRisk)
			}
			if act.Production != tc.production {
				t.Errorf("Production = %v, want %v", act.Production, tc.production)
			}
		})
	}
}

// End to end: a production command routed through the assembled registry and
// the configured policy must still require confirmation in auto mode.
func TestProductionCommandConfirmsThroughTheAssembledStack(t *testing.T) {
	cfg := config.Default()
	cfg.Execution.Mode = permissions.ModeAuto
	cfg.Permissions.Shell.Execute = permissions.RuleAllow

	reg, err := BuildTools(cfg, testDeps(t))
	if err != nil {
		t.Fatalf("BuildTools() = %v", err)
	}
	run, _ := reg.Get("run")
	act, err := run.Permission(runCall(t, "terraform apply -auto-approve"))
	if err != nil {
		t.Fatalf("Permission() = %v", err)
	}

	decision := permissions.NewEvaluator(cfg.Policy()).Evaluate(act)
	t.Logf("auto + shell.execute:allow -> %s (%s)", decision.Outcome, decision.Reason)
	if decision.Outcome == permissions.OutcomeAllow {
		t.Error("terraform apply was allowed without confirmation in auto mode")
	}
}

func TestNetworkToolsRegisterOnlyWhenEnabled(t *testing.T) {
	cfg := config.Default()
	reg, err := BuildTools(cfg, testDeps(t))
	if err != nil {
		t.Fatalf("BuildTools() = %v", err)
	}
	for _, name := range []string{"fetch", "websearch", "http"} {
		if _, ok := reg.Get(name); ok {
			t.Errorf("%q registered while outbound access is disabled", name)
		}
	}

	// Offering a tool that is configured to refuse only wastes a turn.
	cfg.Network.Enabled = true
	deps := testDeps(t)
	web, err := newWebClient(cfg)
	if err != nil {
		t.Fatalf("newWebClient() = %v", err)
	}
	deps.Web = web
	reg, err = BuildTools(cfg, deps)
	if err != nil {
		t.Fatalf("BuildTools() = %v", err)
	}
	for _, name := range []string{"fetch", "websearch", "http"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("%q missing while outbound access is enabled", name)
		}
	}
}

func TestBuildToolsRegistersTheCoreSet(t *testing.T) {
	reg, err := BuildTools(config.Default(), testDeps(t))
	if err != nil {
		t.Fatalf("BuildTools() = %v", err)
	}
	for _, name := range []string{"read", "write", "edit", "list", "find", "search", "run", "git", "test", "build", "lint", "format"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("core tool %q not registered (have %v)", name, reg.Names())
		}
	}
}

func TestBuildToolsRequiresItsDependencies(t *testing.T) {
	if _, err := BuildTools(config.Default(), ToolDeps{Executor: stubExecutor{}}); err == nil {
		t.Error("a missing workspace should be rejected")
	}
	ws, _ := tools.NewWorkspace(t.TempDir())
	if _, err := BuildTools(config.Default(), ToolDeps{Workspace: ws}); err == nil {
		t.Error("a missing executor should be rejected")
	}
}

// A tool that exists but is never registered is invisible to the model, which
// is indistinguishable from not having written it.
func TestAttachToolIsRegistered(t *testing.T) {
	reg, err := BuildTools(config.Default(), testDeps(t))
	if err != nil {
		t.Fatalf("BuildTools() = %v", err)
	}
	tool, ok := reg.Get("attach")
	if !ok {
		t.Fatalf("attach not registered (have %v)", reg.Names())
	}
	// The model chooses tools from their descriptions, so this one has to say
	// what it is for without the user knowing the tool exists.
	desc := strings.ToLower(tool.Description())
	for _, want := range []string{"pdf"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description should mention %q so the model reaches for it: %q", want, desc)
		}
	}
}
