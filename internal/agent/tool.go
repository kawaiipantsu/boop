package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/tools"
)

// DelegateTool lets a model hand a self-contained objective to the agent fleet.
//
// It lives in this package rather than internal/tools because the fleet builds
// on app.Loop, which builds on the tool registry: a delegation tool in
// internal/tools would close that loop into an import cycle. Frontends register
// it explicitly, which also means delegation is only offered when a fleet
// actually exists.
//
// Without it the scheduler is unreachable — every control surface works, over
// an empty fleet, because nothing can put work into it.
type DelegateTool struct {
	coordinator *Coordinator
	// Timeout bounds a delegated run. Zero applies DefaultDelegateTimeout.
	Timeout time.Duration
}

// DefaultDelegateTimeout bounds a delegated run.
//
// Delegated work is open-ended by nature, so the bound is generous, but it is
// present: a fleet that never finishes blocks the turn that spawned it.
const DefaultDelegateTimeout = 15 * time.Minute

// NewDelegateTool returns a delegation tool over the given fleet.
func NewDelegateTool(c *Coordinator) *DelegateTool {
	return &DelegateTool{coordinator: c}
}

// delegateArgs are the decoded arguments of the delegate tool.
type delegateArgs struct {
	Objective string `json:"objective"`
	// Requirements are constraints the workers must respect.
	Requirements string `json:"requirements,omitempty"`
	// PlanOnly returns the decomposition without executing it, so a model can
	// show the user what it intends before committing to it.
	PlanOnly bool `json:"plan_only,omitempty"`
}

// Name implements tools.Tool.
func (t *DelegateTool) Name() string { return "delegate" }

// Description implements tools.Tool.
func (t *DelegateTool) Description() string {
	return "Break a large objective into tasks and run them across parallel agents. " +
		"Use this for work with several independent parts, such as reviewing many files " +
		"or implementing separate components. Do not use it for a single step you could " +
		"do directly — delegation costs a planning round trip. Set plan_only to see the " +
		"decomposition without running it."
}

// Schema implements tools.Tool.
func (t *DelegateTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"objective": map[string]any{
				"type":        "string",
				"description": "The overall goal, stated so a worker with no other context can act on it.",
			},
			"requirements": map[string]any{
				"type":        "string",
				"description": "Constraints every worker must respect.",
			},
			"plan_only": map[string]any{
				"type":        "boolean",
				"description": "Return the task breakdown without executing it.",
			},
		},
		"required": []string{"objective"},
	}
}

// Permission implements tools.Tool.
//
// Delegation itself is classified as shell execution at medium risk: the
// objective is not yet a command, but the workers it spawns will run tools of
// their own. Each of those passes the permission engine independently, so this
// gate is about spawning the fleet, not about what it will eventually do.
func (t *DelegateTool) Permission(call tools.Call) (permissions.Action, error) {
	var args delegateArgs
	if err := call.Bind(&args); err != nil {
		return permissions.Action{}, err
	}
	objective := strings.TrimSpace(args.Objective)
	if objective == "" {
		return permissions.Action{}, fmt.Errorf("delegate: objective is required")
	}
	risk := permissions.RiskMedium
	summary := fmt.Sprintf("Delegate to agents: %s", truncateObjective(objective, 80))
	if args.PlanOnly {
		// Planning runs no tools, so it is a read-only operation.
		risk = permissions.RiskLow
		summary = fmt.Sprintf("Plan (without running): %s", truncateObjective(objective, 80))
	}
	return permissions.Action{
		Category: permissions.CatShellExecute,
		Risk:     risk,
		Tool:     t.Name(),
		Summary:  summary,
		Detail:   objective,
	}, nil
}

// Execute implements tools.Tool.
func (t *DelegateTool) Execute(ctx context.Context, call tools.Call) (tools.Result, error) {
	started := time.Now()
	var args delegateArgs
	if err := call.Bind(&args); err != nil {
		return tools.Errorf(call, "delegate: %v", err), nil
	}
	objective := strings.TrimSpace(args.Objective)
	if objective == "" {
		return tools.Errorf(call, "delegate: objective is required"), nil
	}
	if t.coordinator == nil {
		return tools.Errorf(call,
			"agents are disabled, so this objective cannot be delegated. "+
				"Do the work directly, or ask the user to enable agents."), nil
	}

	timeout := t.Timeout
	if timeout <= 0 {
		timeout = DefaultDelegateTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if args.PlanOnly {
		plan := t.coordinator.Plan(runCtx, objective)
		body, _ := json.MarshalIndent(plan, "", "  ")
		return tools.Result{
			CallID: call.ID, Tool: call.Name, Data: plan,
			Display:  fmt.Sprintf("%d task(s) planned", len(plan.Tasks)),
			Content:  "Plan (not executed):\n" + string(body),
			Duration: time.Since(started),
		}, nil
	}

	req := PlanRequest{Objective: objective}
	if reqs := strings.TrimSpace(args.Requirements); reqs != "" {
		req.Requirements = []string{reqs}
	}
	report, err := t.coordinator.Run(runCtx, req)
	if err != nil {
		return tools.Errorf(call, "delegation failed: %v", err), nil
	}

	return tools.Result{
		CallID: call.ID, Tool: call.Name, Data: report,
		Display:  fmt.Sprintf("%d ok, %d failed", report.Succeeded, report.Failed),
		IsError:  report.Failed > 0,
		Content:  report.Summary(),
		Duration: time.Since(started),
	}, nil
}

// truncateObjective shortens an objective for an approval prompt.
func truncateObjective(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// compile-time proof that this satisfies the tool contract.
var _ tools.Tool = (*DelegateTool)(nil)
