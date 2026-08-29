package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kawaiipantsu/boop/internal/provider"
)

// Decomposer turns an objective into candidate tasks.
//
// It is an interface for one reason: planning needs a model call, and every
// test of the scheduler and the coordinator would otherwise need a provider.
type Decomposer interface {
	Decompose(ctx context.Context, objective string) ([]Task, error)
}

// DecomposerFunc adapts a function to Decomposer.
type DecomposerFunc func(ctx context.Context, objective string) ([]Task, error)

// Decompose implements Decomposer.
func (f DecomposerFunc) Decompose(ctx context.Context, objective string) ([]Task, error) {
	return f(ctx, objective)
}

// DefaultMaxPlanTasks bounds how many tasks a plan may contain. A model that
// returns forty tasks has misunderstood the objective, and running them would
// burn the agent budget on a plan nobody asked for.
const DefaultMaxPlanTasks = 16

// Planner turns an objective into a schedulable task graph.
type Planner struct {
	// Decomposer produces the candidate plan. Nil always degrades.
	Decomposer Decomposer
	// MaxTasks bounds the plan; zero uses DefaultMaxPlanTasks.
	MaxTasks int
}

// PlanResult is a plan that is always usable.
type PlanResult struct {
	Objective string `json:"objective"`
	Tasks     []Task `json:"tasks"`
	// Degraded reports that planning did not work and the objective is being
	// run as a single task.
	Degraded bool `json:"degraded"`
	// Reason explains a degraded plan, for logs and `/agents`.
	Reason string `json:"reason,omitempty"`
}

// Plan decomposes objective into tasks.
//
// It never fails. A planner that errors, times out, returns nothing usable or
// returns an unschedulable graph degrades to a single task carrying the
// original objective: the user asked for work to be done, and losing the
// request because the planning call misbehaved would be the worse outcome.
func (p *Planner) Plan(ctx context.Context, objective string) PlanResult {
	objective = strings.TrimSpace(objective)
	fallback := func(reason string) PlanResult {
		return PlanResult{
			Objective: objective,
			Tasks:     []Task{{ID: "task-1", Description: objective}},
			Degraded:  true,
			Reason:    reason,
		}
	}
	if objective == "" {
		return PlanResult{Objective: objective, Tasks: nil, Degraded: true, Reason: "the objective was empty"}
	}
	if p == nil || p.Decomposer == nil {
		return fallback("no planner is configured")
	}

	tasks, err := p.Decomposer.Decompose(ctx, objective)
	if err != nil {
		return fallback(fmt.Sprintf("planning failed: %v", err))
	}

	max := p.MaxTasks
	if max <= 0 {
		max = DefaultMaxPlanTasks
	}
	tasks = normalizePlan(tasks)
	switch {
	case len(tasks) == 0:
		return fallback("the planner produced no tasks")
	case len(tasks) > max:
		return fallback(fmt.Sprintf("the planner produced %d tasks, more than the limit of %d", len(tasks), max))
	}
	if err := Validate(tasks); err != nil {
		return fallback(fmt.Sprintf("the plan was not schedulable: %v", err))
	}
	if len(tasks) == 1 && tasks[0].Description == objective {
		// An honest single-step plan is not a degradation.
		return PlanResult{Objective: objective, Tasks: tasks}
	}
	return PlanResult{Objective: objective, Tasks: tasks}
}

// normalizePlan makes a model's output schedulable without rejecting it.
//
// Missing ids are assigned, blank descriptions drop the task, duplicate ids are
// dropped, and dependencies that name nothing real (or the task itself) are
// removed rather than failing the plan. What survives is either a valid graph
// or a cycle, which Validate then catches.
func normalizePlan(in []Task) []Task {
	out := make([]Task, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for i, t := range in {
		t.Description = strings.TrimSpace(t.Description)
		if t.Description == "" {
			continue
		}
		t.ID = strings.TrimSpace(t.ID)
		if t.ID == "" {
			t.ID = fmt.Sprintf("task-%d", i+1)
		}
		if _, dup := seen[t.ID]; dup {
			continue
		}
		seen[t.ID] = struct{}{}
		t.Status = TaskPending
		t.Writes = normalizeAll(t.Writes)
		t.Reads = normalizeAll(t.Reads)
		t.AllowedTools = dedupe(t.AllowedTools)
		out = append(out, t)
	}
	for i := range out {
		deps := make([]string, 0, len(out[i].Dependencies))
		for _, dep := range out[i].Dependencies {
			dep = strings.TrimSpace(dep)
			if dep == "" || dep == out[i].ID {
				continue
			}
			if _, ok := seen[dep]; !ok {
				continue
			}
			deps = append(deps, dep)
		}
		out[i].Dependencies = dedupe(deps)
	}
	return out
}

// ModelCaller performs one completion. Planning is a single request with no
// tools, so it does not need — and must not duplicate — app.Loop.
type ModelCaller interface {
	Complete(ctx context.Context, messages []provider.Message) (string, error)
}

// ModelDecomposer plans by asking a model and parsing its JSON answer.
type ModelDecomposer struct {
	// Caller performs the completion. Required.
	Caller ModelCaller
	// Prompt overrides the embedded planner prompt.
	Prompt string
}

// Decompose implements Decomposer.
func (d *ModelDecomposer) Decompose(ctx context.Context, objective string) ([]Task, error) {
	if d == nil || d.Caller == nil {
		return nil, errors.New("planner: no model caller is configured")
	}
	prompt := d.Prompt
	if strings.TrimSpace(prompt) == "" {
		prompt = PlannerPrompt()
	}
	raw, err := d.Caller.Complete(ctx, []provider.Message{
		{Role: provider.RoleSystem, Content: prompt},
		{Role: provider.RoleUser, Content: objective},
	})
	if err != nil {
		return nil, err
	}
	return ParsePlan(raw)
}

// planPayload is the wire shape declared in planResponseContract.
type planPayload struct {
	Tasks []planTask `json:"tasks"`
}

type planTask struct {
	ID           string   `json:"id"`
	Description  string   `json:"description"`
	Dependencies []string `json:"dependencies"`
	Reads        []string `json:"reads"`
	Writes       []string `json:"writes"`
	Tools        []string `json:"tools"`
	Requirements []string `json:"requirements"`
	Validation   string   `json:"validation"`
}

// ParsePlan extracts tasks from a model's reply.
//
// Models wrap JSON in prose and fences however they feel, so the parser looks
// for the first balanced object or array rather than demanding a clean body.
func ParsePlan(raw string) ([]Task, error) {
	body := extractJSON(raw)
	if body == "" {
		return nil, fmt.Errorf("planner: no JSON found in the model's reply")
	}

	var payload planPayload
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		// A bare array of tasks is a common and harmless variation.
		var bare []planTask
		if arrErr := json.Unmarshal([]byte(body), &bare); arrErr != nil {
			return nil, fmt.Errorf("planner: the plan was not valid JSON: %w", err)
		}
		payload.Tasks = bare
	}

	tasks := make([]Task, 0, len(payload.Tasks))
	for _, pt := range payload.Tasks {
		tasks = append(tasks, Task{
			ID:           pt.ID,
			Description:  pt.Description,
			Dependencies: pt.Dependencies,
			Reads:        pt.Reads,
			Writes:       pt.Writes,
			AllowedTools: pt.Tools,
			Requirements: pt.Requirements,
			Validation:   pt.Validation,
			Status:       TaskPending,
		})
	}
	return tasks, nil
}

// extractJSON returns the first balanced JSON object or array in text.
func extractJSON(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	start := strings.IndexAny(text, "{[")
	if start < 0 {
		return ""
	}
	open := rune(text[start])
	shut := '}'
	if open == '[' {
		shut = ']'
	}

	depth := 0
	inString := false
	escaped := false
	for i, r := range text[start:] {
		switch {
		case escaped:
			escaped = false
		case r == '\\' && inString:
			escaped = true
		case r == '"':
			inString = !inString
		case inString:
			// Braces inside strings are not structure.
		case r == open:
			depth++
		case r == shut:
			depth--
			if depth == 0 {
				return text[start : start+i+1]
			}
		}
	}
	return ""
}

// RouterCaller is the production ModelCaller: one non-streaming-shaped request
// through the router, with the stream drained into a single string.
type RouterCaller struct {
	// Router resolves the provider. Required.
	Router *provider.Router
	// Selection pins the provider and model used for planning.
	Selection provider.Selection
	// MaxTokens bounds the plan; zero leaves it to the provider.
	MaxTokens int
}

// Complete implements ModelCaller.
func (c *RouterCaller) Complete(ctx context.Context, messages []provider.Message) (string, error) {
	if c == nil || c.Router == nil {
		return "", errors.New("planner: no router is configured")
	}
	events, _, err := c.Router.Chat(ctx, c.Selection, provider.ChatRequest{
		Messages:  messages,
		Stream:    true,
		MaxTokens: c.MaxTokens,
	})
	if err != nil {
		return "", err
	}

	var text strings.Builder
	for ev := range events {
		switch ev.Type {
		case provider.EventDelta:
			text.WriteString(ev.Text)
		case provider.EventError:
			// Drain so the adapter is never left blocked on a channel nobody
			// is reading, matching app.Loop.collect.
			for range events {
			}
			return "", ev.Err
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return text.String(), nil
}
