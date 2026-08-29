package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/provider"
)

func TestPlannerDegradesToASingleTask(t *testing.T) {
	objective := "Add retry to the router."

	tests := []struct {
		name    string
		planner *Planner
		reason  string
	}{
		{"nil planner", nil, "no planner"},
		{"no decomposer", &Planner{}, "no planner"},
		{
			name: "decomposer fails",
			planner: &Planner{Decomposer: DecomposerFunc(func(context.Context, string) ([]Task, error) {
				return nil, errors.New("the model timed out")
			})},
			reason: "planning failed",
		},
		{
			name: "no tasks produced",
			planner: &Planner{Decomposer: DecomposerFunc(func(context.Context, string) ([]Task, error) {
				return nil, nil
			})},
			reason: "no tasks",
		},
		{
			name: "only blank tasks produced",
			planner: &Planner{Decomposer: DecomposerFunc(func(context.Context, string) ([]Task, error) {
				return []Task{{ID: "a", Description: "   "}}, nil
			})},
			reason: "no tasks",
		},
		{
			name:    "too many tasks",
			planner: &Planner{MaxTasks: 2, Decomposer: DecomposerFunc(manyTasks(5))},
			reason:  "more than the limit",
		},
		{
			name: "unschedulable plan",
			planner: &Planner{Decomposer: DecomposerFunc(func(context.Context, string) ([]Task, error) {
				return []Task{
					{ID: "a", Description: "a", Dependencies: []string{"b"}},
					{ID: "b", Description: "b", Dependencies: []string{"a"}},
				}, nil
			})},
			reason: "not schedulable",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := tc.planner.Plan(context.Background(), objective)
			if !plan.Degraded {
				t.Fatalf("Degraded = false, want true")
			}
			if len(plan.Tasks) != 1 {
				t.Fatalf("got %d tasks, want the objective as one task", len(plan.Tasks))
			}
			if plan.Tasks[0].Description != objective {
				t.Errorf("task description = %q, want the original objective", plan.Tasks[0].Description)
			}
			if !strings.Contains(plan.Reason, tc.reason) {
				t.Errorf("Reason = %q, want it to mention %q", plan.Reason, tc.reason)
			}
			if err := Validate(plan.Tasks); err != nil {
				t.Errorf("the fallback plan is not schedulable: %v", err)
			}
		})
	}
}

func manyTasks(n int) func(context.Context, string) ([]Task, error) {
	return func(context.Context, string) ([]Task, error) {
		tasks := make([]Task, n)
		for i := range tasks {
			tasks[i] = Task{ID: string(rune('a' + i)), Description: "step"}
		}
		return tasks, nil
	}
}

func TestPlannerEmptyObjective(t *testing.T) {
	plan := (&Planner{}).Plan(context.Background(), "   ")
	if len(plan.Tasks) != 0 {
		t.Errorf("Tasks = %v, want none for an empty objective", plan.Tasks)
	}
	if !plan.Degraded {
		t.Error("Degraded = false, want true")
	}
}

func TestPlannerAcceptsAGoodPlan(t *testing.T) {
	p := &Planner{Decomposer: DecomposerFunc(func(_ context.Context, objective string) ([]Task, error) {
		return []Task{
			{ID: "inspect", Description: "inspect the architecture"},
			{ID: "implement", Description: "implement the provider", Dependencies: []string{"inspect"}, Writes: []string{"p.go"}},
			{ID: "tests", Description: "write tests", Dependencies: []string{"inspect"}, Writes: []string{"p_test.go"}},
		}, nil
	})}
	plan := p.Plan(context.Background(), "add a provider")
	if plan.Degraded {
		t.Fatalf("Degraded = true (%s), want a usable plan", plan.Reason)
	}
	if len(plan.Tasks) != 3 {
		t.Fatalf("got %d tasks, want 3", len(plan.Tasks))
	}
	for _, task := range plan.Tasks {
		if task.Status != TaskPending {
			t.Errorf("task %s status = %q, want pending", task.ID, task.Status)
		}
	}
}

func TestNormalizePlan(t *testing.T) {
	tests := []struct {
		name  string
		in    []Task
		wantN int
		check func(t *testing.T, out []Task)
	}{
		{
			name:  "assigns missing ids",
			in:    []Task{{Description: "one"}, {Description: "two"}},
			wantN: 2,
			check: func(t *testing.T, out []Task) {
				if out[0].ID == "" || out[1].ID == "" || out[0].ID == out[1].ID {
					t.Errorf("ids = %q, %q; want distinct generated ids", out[0].ID, out[1].ID)
				}
			},
		},
		{
			name:  "drops blank descriptions",
			in:    []Task{{ID: "a", Description: " "}, {ID: "b", Description: "real"}},
			wantN: 1,
		},
		{
			name:  "drops duplicate ids",
			in:    []Task{{ID: "a", Description: "first"}, {ID: "a", Description: "second"}},
			wantN: 1,
			check: func(t *testing.T, out []Task) {
				if out[0].Description != "first" {
					t.Errorf("kept %q, want the first occurrence", out[0].Description)
				}
			},
		},
		{
			name:  "drops unknown and self dependencies",
			in:    []Task{{ID: "a", Description: "a", Dependencies: []string{"ghost", "a", "b"}}, {ID: "b", Description: "b"}},
			wantN: 2,
			check: func(t *testing.T, out []Task) {
				if strings.Join(out[0].Dependencies, ",") != "b" {
					t.Errorf("dependencies = %v, want only the real one", out[0].Dependencies)
				}
			},
		},
		{
			name:  "normalises paths",
			in:    []Task{{ID: "a", Description: "a", Writes: []string{"./x/../x/y.go", " "}}},
			wantN: 1,
			check: func(t *testing.T, out []Task) {
				if strings.Join(out[0].Writes, ",") != "x/y.go" {
					t.Errorf("Writes = %v, want the cleaned path", out[0].Writes)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := normalizePlan(tc.in)
			if len(out) != tc.wantN {
				t.Fatalf("got %d tasks, want %d: %+v", len(out), tc.wantN, out)
			}
			if tc.check != nil {
				tc.check(t, out)
			}
		})
	}
}

func TestParsePlan(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantIDs []string
		wantErr bool
	}{
		{
			name:    "plain object",
			raw:     `{"tasks":[{"id":"a","description":"do a"},{"id":"b","description":"do b","dependencies":["a"]}]}`,
			wantIDs: []string{"a", "b"},
		},
		{
			name:    "fenced json",
			raw:     "Here is the plan:\n```json\n{\"tasks\":[{\"id\":\"a\",\"description\":\"do a\"}]}\n```\nHope that helps.",
			wantIDs: []string{"a"},
		},
		{
			name:    "bare array",
			raw:     `[{"id":"a","description":"do a"}]`,
			wantIDs: []string{"a"},
		},
		{
			name:    "braces inside strings",
			raw:     `{"tasks":[{"id":"a","description":"write func x() { return }"}]}`,
			wantIDs: []string{"a"},
		},
		{
			name:    "escaped quote inside string",
			raw:     `{"tasks":[{"id":"a","description":"say \"hi\""}]}`,
			wantIDs: []string{"a"},
		},
		{"no json at all", "I would start by reading the code.", nil, true},
		{"unbalanced", `{"tasks":[{"id":"a"`, nil, true},
		{"not a plan", `{"nope":1}`, []string{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tasks, err := ParsePlan(tc.raw)
			if tc.wantErr != (err != nil) {
				t.Fatalf("ParsePlan() error = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			got := make([]string, 0, len(tasks))
			for _, task := range tasks {
				got = append(got, task.ID)
			}
			if strings.Join(got, ",") != strings.Join(tc.wantIDs, ",") {
				t.Errorf("ids = %v, want %v", got, tc.wantIDs)
			}
		})
	}
}

func TestParsePlanMapsEveryField(t *testing.T) {
	tasks, err := ParsePlan(`{"tasks":[{"id":"a","description":"d","dependencies":["b"],
	  "reads":["r.go"],"writes":["w.go"],"tools":["read"],"requirements":["be quick"],
	  "validation":"go test ./..."}, {"id":"b","description":"e"}]}`)
	if err != nil {
		t.Fatalf("ParsePlan() = %v", err)
	}
	got := tasks[0]
	if got.Validation != "go test ./..." || got.AllowedTools[0] != "read" ||
		got.Writes[0] != "w.go" || got.Reads[0] != "r.go" ||
		got.Requirements[0] != "be quick" || got.Dependencies[0] != "b" {
		t.Errorf("task = %+v, want every field mapped", got)
	}
}

// fakeCaller returns a canned completion.
type fakeCaller struct {
	reply    string
	err      error
	messages []provider.Message
}

func (f *fakeCaller) Complete(_ context.Context, msgs []provider.Message) (string, error) {
	f.messages = msgs
	return f.reply, f.err
}

func TestModelDecomposer(t *testing.T) {
	caller := &fakeCaller{reply: `{"tasks":[{"id":"a","description":"do a"}]}`}
	d := &ModelDecomposer{Caller: caller}

	tasks, err := d.Decompose(context.Background(), "an objective")
	if err != nil {
		t.Fatalf("Decompose() = %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "a" {
		t.Errorf("tasks = %+v", tasks)
	}
	if len(caller.messages) != 2 {
		t.Fatalf("sent %d messages, want a system prompt and the objective", len(caller.messages))
	}
	if !strings.Contains(caller.messages[0].Content, "Boop's planner") {
		t.Error("the system message is not the planner prompt")
	}
	if !strings.Contains(caller.messages[0].Content, "Response format") {
		t.Error("the planner prompt is missing the JSON response contract")
	}
	if caller.messages[1].Content != "an objective" {
		t.Errorf("user message = %q", caller.messages[1].Content)
	}
}

func TestModelDecomposerErrors(t *testing.T) {
	if _, err := (&ModelDecomposer{}).Decompose(context.Background(), "x"); err == nil {
		t.Error("Decompose() without a caller = nil, want an error")
	}
	boom := errors.New("http 500")
	if _, err := (&ModelDecomposer{Caller: &fakeCaller{err: boom}}).Decompose(context.Background(), "x"); !errors.Is(err, boom) {
		t.Errorf("Decompose() = %v, want %v", err, boom)
	}
}

func TestRouterCallerDrainsTheStream(t *testing.T) {
	p := &scriptedProvider{turns: [][]provider.ChatEvent{{
		{Type: provider.EventDelta, Text: `{"tasks":`},
		{Type: provider.EventDelta, Text: `[{"id":"a","description":"do a"}]}`},
		{Type: provider.EventDone, Finish: provider.FinishStop},
	}}}
	providers := provider.NewRegistry()
	if err := providers.Register(p); err != nil {
		t.Fatal(err)
	}
	router := provider.NewRouter(providers, provider.RouterConfig{
		Classes: map[provider.RouteClass]provider.Target{
			provider.ClassDefault: {Provider: "scripted", Model: "test"},
		},
	})
	caller := &RouterCaller{Router: router, Selection: provider.Selection{Provider: "scripted", Model: "test"}}

	plan := (&Planner{Decomposer: &ModelDecomposer{Caller: caller}}).Plan(context.Background(), "objective")
	if plan.Degraded {
		t.Fatalf("Degraded = true (%s)", plan.Reason)
	}
	if len(plan.Tasks) != 1 || plan.Tasks[0].ID != "a" {
		t.Errorf("tasks = %+v", plan.Tasks)
	}

	if _, err := (&RouterCaller{}).Complete(context.Background(), nil); err == nil {
		t.Error("Complete() without a router = nil, want an error")
	}
}
