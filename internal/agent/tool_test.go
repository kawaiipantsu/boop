package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/tools"
)

func delegateCall(t *testing.T, args map[string]any) tools.Call {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return tools.Call{ID: "c1", Name: "delegate", Arguments: raw}
}

// Without a fleet the tool must say so usefully. Returning a bare error would
// leave the model guessing whether to retry.
func TestDelegateWithoutAFleetExplainsItself(t *testing.T) {
	res, err := NewDelegateTool(nil).Execute(context.Background(),
		delegateCall(t, map[string]any{"objective": "review the codebase"}))
	if err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}
	if !res.IsError {
		t.Fatal("delegating with no fleet should be an error result")
	}
	for _, want := range []string{"disabled", "directly"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("content = %q, want it to mention %q", res.Content, want)
		}
	}
}

func TestDelegatePermission(t *testing.T) {
	tool := NewDelegateTool(nil)

	act, err := tool.Permission(delegateCall(t, map[string]any{"objective": "refactor the parser"}))
	if err != nil {
		t.Fatalf("Permission() = %v", err)
	}
	if act.Risk != permissions.RiskMedium {
		t.Errorf("Risk = %s, want medium — spawning workers that will run tools", act.Risk)
	}
	if !strings.Contains(act.Summary, "refactor the parser") {
		t.Errorf("Summary = %q, want the objective visible in the prompt", act.Summary)
	}

	// Planning runs nothing, so it should not be graded like execution.
	act, err = tool.Permission(delegateCall(t, map[string]any{"objective": "refactor", "plan_only": true}))
	if err != nil {
		t.Fatalf("Permission() = %v", err)
	}
	if act.Risk != permissions.RiskLow {
		t.Errorf("plan_only Risk = %s, want low", act.Risk)
	}
	if !strings.Contains(strings.ToLower(act.Summary), "plan") {
		t.Errorf("Summary = %q, want it to say this only plans", act.Summary)
	}
}

func TestDelegateRejectsAnEmptyObjective(t *testing.T) {
	if _, err := NewDelegateTool(nil).Permission(delegateCall(t, map[string]any{"objective": "  "})); err == nil {
		t.Error("an empty objective should be rejected")
	}
}

func TestDelegateSatisfiesTheToolContract(t *testing.T) {
	var tool tools.Tool = NewDelegateTool(nil)
	if tool.Name() != "delegate" {
		t.Errorf("Name() = %q", tool.Name())
	}
	schema := tool.Schema()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema has no properties")
	}
	if _, ok := props["objective"]; !ok {
		t.Error("schema is missing the objective property")
	}
	if _, err := json.Marshal(schema); err != nil {
		t.Errorf("schema is not serialisable: %v", err)
	}
	// The description is how a model decides whether to reach for this, so it
	// has to warn against using it for trivial work.
	desc := strings.ToLower(tool.Description())
	if !strings.Contains(desc, "do not use") {
		t.Error("description should discourage delegating a single step")
	}
}

func TestTruncateObjective(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"short", "short"},
		{"a  b\n c", "a b c"},
		{strings.Repeat("x", 100), strings.Repeat("x", 79) + "…"},
	} {
		if got := truncateObjective(tc.in, 80); got != tc.want {
			t.Errorf("truncateObjective(%q) = %q, want %q", tc.in[:min(len(tc.in), 20)], got, tc.want)
		}
	}
}
