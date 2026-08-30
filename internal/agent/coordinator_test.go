package agent

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kawaiipantsu/boop/internal/app"
	"github.com/kawaiipantsu/boop/internal/provider"
)

// stubRunner reports a canned outcome and optionally blocks.
type stubRunner struct {
	mu      sync.Mutex
	briefs  map[string]Brief
	fail    map[string]error
	hold    chan struct{}
	started chan string
	calls   int32
}

func newStubRunner() *stubRunner {
	return &stubRunner{briefs: map[string]Brief{}, fail: map[string]error{}, started: make(chan string, 64)}
}

func (r *stubRunner) RunTask(ctx context.Context, a *Agent, brief Brief) (TaskOutcome, error) {
	atomic.AddInt32(&r.calls, 1)
	r.mu.Lock()
	r.briefs[a.Name] = brief
	err := r.fail[a.Name]
	hold := r.hold
	r.mu.Unlock()

	select {
	case r.started <- a.Name:
	default:
	}

	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return TaskOutcome{}, ctx.Err()
		}
	}
	if err != nil {
		return TaskOutcome{}, err
	}
	return TaskOutcome{
		Output:    "finished " + a.Name,
		Usage:     provider.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5},
		ToolCalls: 1,
	}, nil
}

func (r *stubRunner) brief(name string) Brief {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.briefs[name]
}

func newTestCoordinator(t *testing.T, runner TaskRunner, opts ...func(*CoordinatorOptions)) (*Coordinator, *collector) {
	t.Helper()
	bus, c := newBusWithCollector()
	o := CoordinatorOptions{Runner: runner, Bus: bus, SessionID: "s1", Max: 5}
	for _, fn := range opts {
		fn(&o)
	}
	return NewCoordinator(o), c
}

func TestCoordinatorRunAggregatesResults(t *testing.T) {
	runner := newStubRunner()
	runner.fail["tests"] = errors.New("assertion failed")
	c, events := newTestCoordinator(t, runner)

	report, err := c.Run(context.Background(), PlanRequest{
		Objective:    "add a provider",
		Requirements: []string{"no new dependencies"},
		Tasks: []Task{
			{ID: "inspect", Description: "inspect the architecture"},
			{ID: "implement", Description: "implement it", Dependencies: []string{"inspect"}, Writes: []string{"p.go"}},
			{ID: "tests", Description: "write tests", Dependencies: []string{"inspect"}, Writes: []string{"p_test.go"}},
			{ID: "review", Description: "review", Dependencies: []string{"implement", "tests"}},
		},
	})
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if report.Succeeded != 2 || report.Failed != 1 || report.Blocked != 1 {
		t.Errorf("counts = %d ok / %d failed / %d blocked, want 2/1/1", report.Succeeded, report.Failed, report.Blocked)
	}
	if report.OK() {
		t.Error("OK() = true with a failed task")
	}
	if report.Usage.TotalTokens != 10 {
		t.Errorf("Usage.TotalTokens = %d, want 10 from the two tasks that ran", report.Usage.TotalTokens)
	}
	if report.Duration < 0 {
		t.Error("Duration was negative")
	}
	if len(report.Agents) != 3 {
		t.Errorf("report lists %d agents, want the three that ran", len(report.Agents))
	}

	summary := report.Summary()
	for _, want := range []string{"add a provider", "finished inspect", "assertion failed", "blocked"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary is missing %q:\n%s", want, summary)
		}
	}

	// Every worker's brief carries the run's requirements, not the conversation.
	b := runner.brief("implement")
	if len(b.Requirements) != 1 || b.Requirements[0] != "no new dependencies" {
		t.Errorf("brief requirements = %v", b.Requirements)
	}
	if b.Objective != "add a provider" {
		t.Errorf("brief objective = %q", b.Objective)
	}

	if got := len(events.of(app.EventAgentCreated)); got != 3 {
		t.Errorf("got %d agent.created events, want 3", got)
	}
	if got := len(events.of(app.EventAgentStatusChanged)); got < 3 {
		t.Errorf("got %d agent.status.changed events, want at least one per agent", got)
	}
}

func TestCoordinatorRunRejectsBadPlans(t *testing.T) {
	tests := []struct {
		name string
		req  PlanRequest
		want error
		set  func(*Coordinator)
	}{
		{"no tasks", PlanRequest{Objective: "x"}, ErrNoTasks, nil},
		{
			name: "cycle",
			req:  PlanRequest{Tasks: []Task{{ID: "a", Dependencies: []string{"b"}}, {ID: "b", Dependencies: []string{"a"}}}},
			want: ErrInvalidGraph,
		},
		{
			name: "disabled",
			req:  PlanRequest{Tasks: []Task{{ID: "a", Description: "a"}}},
			want: ErrDisabled,
			set:  func(c *Coordinator) { c.SetEnabled(false) },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := newStubRunner()
			c, _ := newTestCoordinator(t, runner)
			if tc.set != nil {
				tc.set(c)
			}
			report, err := c.Run(context.Background(), tc.req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Run() = %v, want %v", err, tc.want)
			}
			if report == nil || report.Error == "" {
				t.Error("want a report carrying the error")
			}
			if atomic.LoadInt32(&runner.calls) != 0 {
				t.Error("a rejected plan must not spawn agents")
			}
		})
	}
}

func TestCoordinatorRunsWithoutARunner(t *testing.T) {
	c := NewCoordinator(CoordinatorOptions{})
	_, err := c.Run(context.Background(), PlanRequest{Tasks: []Task{{ID: "a", Description: "a"}}})
	if !errors.Is(err, ErrNoRunner) {
		t.Fatalf("Run() = %v, want ErrNoRunner", err)
	}
}

func TestCoordinatorBoundsConcurrency(t *testing.T) {
	var mu sync.Mutex
	running, peak := 0, 0
	runner := TaskRunnerFunc(func(ctx context.Context, a *Agent, _ Brief) (TaskOutcome, error) {
		mu.Lock()
		running++
		if running > peak {
			peak = running
		}
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		mu.Lock()
		running--
		mu.Unlock()
		return TaskOutcome{Output: "ok"}, nil
	})
	c, _ := newTestCoordinator(t, runner, func(o *CoordinatorOptions) { o.Max = 2 })

	tasks := make([]Task, 8)
	for i := range tasks {
		tasks[i] = Task{ID: fmt.Sprintf("t%d", i), Description: "work"}
	}
	if _, err := c.Run(context.Background(), PlanRequest{Tasks: tasks}); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if peak > 2 {
		t.Errorf("peak concurrency = %d, want at most the configured 2", peak)
	}
}

func TestCoordinatorDefaultsMatchTheSpec(t *testing.T) {
	c := NewCoordinator(CoordinatorOptions{})
	if got := c.Max(); got != 5 {
		t.Errorf("Max() = %d, want the §10 default of 5", got)
	}
	if got := c.MaxDepth(); got != DefaultMaxDepth {
		t.Errorf("MaxDepth() = %d, want %d", got, DefaultMaxDepth)
	}
	if got := c.MaxAgents(); got != DefaultMaxAgents {
		t.Errorf("MaxAgents() = %d, want %d", got, DefaultMaxAgents)
	}
	if !c.Enabled() {
		t.Error("Enabled() = false, want agents on by default")
	}
}

func TestCoordinatorSetMaxValidation(t *testing.T) {
	c := NewCoordinator(CoordinatorOptions{Max: 3})
	tests := []struct {
		name    string
		set     func() error
		wantErr bool
	}{
		{"max zero", func() error { return c.SetMax(0) }, true},
		{"max negative", func() error { return c.SetMax(-2) }, true},
		{"max valid", func() error { return c.SetMax(7) }, false},
		{"depth zero", func() error { return c.SetMaxDepth(0) }, true},
		{"depth valid", func() error { return c.SetMaxDepth(2) }, false},
		{"budget zero", func() error { return c.SetMaxAgents(0) }, true},
		{"budget valid", func() error { return c.SetMaxAgents(4) }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.set(); tc.wantErr != (err != nil) {
				t.Errorf("error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
	if c.Max() != 7 || c.MaxDepth() != 2 || c.MaxAgents() != 4 {
		t.Errorf("limits = %d/%d/%d, want 7/2/4", c.Max(), c.MaxDepth(), c.MaxAgents())
	}
}

func TestCoordinatorSpawnDepthCap(t *testing.T) {
	c := NewCoordinator(CoordinatorOptions{MaxDepth: 2})
	root, err := c.Spawn(SpawnSpec{Name: "root", Task: "root"})
	if err != nil {
		t.Fatalf("Spawn() = %v", err)
	}
	if root.Depth() != 1 {
		t.Errorf("Depth() = %d, want 1", root.Depth())
	}
	child, err := c.Spawn(SpawnSpec{Name: "child", Task: "child", ParentID: root.ID})
	if err != nil {
		t.Fatalf("Spawn(child) = %v", err)
	}
	if child.RootID() != root.ID {
		t.Errorf("RootID() = %q, want the root's id", child.RootID())
	}
	_, err = c.Spawn(SpawnSpec{Name: "grandchild", Task: "deep", ParentID: child.ID})
	if !errors.Is(err, ErrMaxDepth) {
		t.Fatalf("Spawn(grandchild) = %v, want ErrMaxDepth", err)
	}
	if !strings.Contains(err.Error(), "limit is 2") {
		t.Errorf("error = %q, want it to name the limit", err)
	}
	if _, err := c.Spawn(SpawnSpec{Name: "orphan", ParentID: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Spawn with an unknown parent = %v, want ErrNotFound", err)
	}
}

func TestCoordinatorAgentBudgetIsPerObjective(t *testing.T) {
	c := NewCoordinator(CoordinatorOptions{MaxAgents: 3, MaxDepth: 9})
	root, err := c.Spawn(SpawnSpec{Name: "root"})
	if err != nil {
		t.Fatalf("Spawn() = %v", err)
	}
	parent := root
	for i := 0; i < 2; i++ {
		parent, err = c.Spawn(SpawnSpec{Name: fmt.Sprintf("c%d", i), ParentID: parent.ID})
		if err != nil {
			t.Fatalf("Spawn(%d) = %v", i, err)
		}
	}
	_, err = c.Spawn(SpawnSpec{Name: "one too many", ParentID: parent.ID})
	if !errors.Is(err, ErrAgentBudget) {
		t.Fatalf("Spawn() = %v, want ErrAgentBudget", err)
	}

	// A different objective starts with a fresh budget.
	if _, err := c.Spawn(SpawnSpec{Name: "another root"}); err != nil {
		t.Errorf("a new tree = %v, want a fresh budget", err)
	}
}

// TestCoordinatorRecursiveSpawningTerminates is the §11 requirement that
// uncontrolled recursion is impossible: a worker that always delegates must hit
// a clear error, not run forever and not hang.
func TestCoordinatorRecursiveSpawningTerminates(t *testing.T) {
	before := runtime.NumGoroutine()
	var c *Coordinator
	var depth int32

	runner := TaskRunnerFunc(func(ctx context.Context, a *Agent, _ Brief) (TaskOutcome, error) {
		if d := int32(a.Depth()); d > atomic.LoadInt32(&depth) {
			atomic.StoreInt32(&depth, d)
		}
		// Always delegate, forever, if it were allowed.
		report, err := c.Run(ctx, PlanRequest{
			Objective: "deeper",
			ParentID:  a.ID,
			Tasks: []Task{
				{ID: a.ID + "-x", Description: "deeper x"},
				{ID: a.ID + "-y", Description: "deeper y"},
			},
		})
		if err != nil {
			return TaskOutcome{Output: "delegation refused"}, nil
		}
		return TaskOutcome{Output: report.Summary()}, nil
	})
	c = NewCoordinator(CoordinatorOptions{Runner: runner, Max: 3, MaxDepth: 3, MaxAgents: 12})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	report, err := c.Run(ctx, PlanRequest{Objective: "root", Tasks: []Task{{ID: "root", Description: "root"}}})
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("recursion did not terminate on its own")
	}
	if got := atomic.LoadInt32(&depth); got > 3 {
		t.Errorf("reached depth %d, want at most the configured 3", got)
	}
	if got := len(c.List()); got > 12 {
		t.Errorf("created %d agents, want at most the budget of 12", got)
	}
	if len(report.Tasks) != 1 {
		t.Fatalf("got %d results, want 1", len(report.Tasks))
	}
	if report.Tasks[0].Status != TaskComplete {
		t.Errorf("root = %s, want complete", report.Tasks[0].Status)
	}
	assertNoLeak(t, before)
}

func TestCoordinatorStopByPrefix(t *testing.T) {
	runner := newStubRunner()
	hold := make(chan struct{})
	runner.hold = hold
	c, _ := newTestCoordinator(t, runner)

	done := make(chan *RunReport, 1)
	go func() {
		report, _ := c.Run(context.Background(), PlanRequest{Tasks: []Task{{ID: "slow", Description: "slow"}}})
		done <- report
	}()

	<-runner.started
	var target AgentInfo
	for _, a := range c.List() {
		target = a
	}
	if err := c.Stop(target.ShortID()); err != nil {
		t.Fatalf("Stop(%q) = %v", target.ShortID(), err)
	}

	select {
	case report := <-done:
		if report.Cancelled != 1 {
			t.Errorf("Cancelled = %d, want 1", report.Cancelled)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after the agent was stopped")
	}
	close(hold)

	info, err := c.Agent(target.ID)
	if err != nil {
		t.Fatalf("Agent() = %v", err)
	}
	if info.Status != StatusCancelled {
		t.Errorf("Status = %q, want cancelled", info.Status)
	}

	if err := c.Stop("ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stop(ghost) = %v, want ErrNotFound", err)
	}
	if err := c.Stop(""); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stop(empty) = %v, want ErrNotFound", err)
	}
}

func TestCoordinatorLookupIsAmbiguousOnSharedPrefix(t *testing.T) {
	c := NewCoordinator(CoordinatorOptions{})
	a, err := c.Spawn(SpawnSpec{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Spawn(SpawnSpec{Name: "b"}); err != nil {
		t.Fatal(err)
	}
	// Every UUID starts with a hex digit, so a one-character prefix that
	// matches the first agent might match the second too. Use the empty-ish
	// common prefix of both: the test only asserts the error when it happens.
	if _, err := c.Agent(a.ID[:1]); err != nil && !errors.Is(err, ErrAmbiguousID) && !errors.Is(err, ErrNotFound) {
		t.Errorf("Agent(prefix) = %v, want a resolved agent or a clear ambiguity", err)
	}
	if _, err := c.Agent(a.ID); err != nil {
		t.Errorf("Agent(full id) = %v", err)
	}
}

func TestCoordinatorStopAllAndDisable(t *testing.T) {
	runner := newStubRunner()
	runner.hold = make(chan struct{})
	c, _ := newTestCoordinator(t, runner, func(o *CoordinatorOptions) { o.Max = 4 })

	done := make(chan *RunReport, 1)
	go func() {
		report, _ := c.Run(context.Background(), PlanRequest{Tasks: []Task{
			{ID: "a", Description: "a"}, {ID: "b", Description: "b"},
		}})
		done <- report
	}()
	<-runner.started
	<-runner.started

	// `/agents off` stops what is in flight.
	c.SetEnabled(false)

	select {
	case report := <-done:
		if report.Cancelled != 2 {
			t.Errorf("Cancelled = %d, want both tasks", report.Cancelled)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after /agents off")
	}
	if c.Enabled() {
		t.Error("Enabled() = true after SetEnabled(false)")
	}
	if _, err := c.Spawn(SpawnSpec{Name: "nope"}); !errors.Is(err, ErrDisabled) {
		t.Errorf("Spawn while disabled = %v, want ErrDisabled", err)
	}
	c.SetEnabled(true)
	if _, err := c.Spawn(SpawnSpec{Name: "yes"}); err != nil {
		t.Errorf("Spawn after /agents on = %v", err)
	}
}

func TestCoordinatorWait(t *testing.T) {
	runner := newStubRunner()
	hold := make(chan struct{})
	runner.hold = hold
	c, _ := newTestCoordinator(t, runner)

	go func() {
		_, _ = c.Run(context.Background(), PlanRequest{Tasks: []Task{{ID: "a", Description: "a"}}})
	}()
	<-runner.started

	// Wait must respect its own context rather than hanging.
	short, cancelShort := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelShort()
	if err := c.Wait(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait() = %v, want the deadline to be honoured", err)
	}

	close(hold)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Wait(ctx); err != nil {
		t.Fatalf("Wait() = %v", err)
	}
	for _, a := range c.List() {
		if a.Status.Active() {
			t.Errorf("agent %s is still %s after Wait", a.ShortID(), a.Status)
		}
	}
}

func TestCoordinatorWaitIgnoresIdleAgents(t *testing.T) {
	c := NewCoordinator(CoordinatorOptions{})
	if _, err := c.Spawn(SpawnSpec{Name: "never started"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Wait(ctx); err != nil {
		t.Fatalf("Wait() = %v, want idle agents to be ignored", err)
	}
}

func TestCoordinatorCancellationDuringRun(t *testing.T) {
	before := runtime.NumGoroutine()
	runner := newStubRunner()
	runner.hold = make(chan struct{})
	c, _ := newTestCoordinator(t, runner, func(o *CoordinatorOptions) { o.Max = 2 })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var report *RunReport
	var err error
	go func() {
		defer close(done)
		report, err = c.Run(ctx, PlanRequest{Tasks: []Task{
			{ID: "a", Description: "a"}, {ID: "b", Description: "b"},
			{ID: "c", Description: "c", Dependencies: []string{"a"}},
		}})
	}()
	<-runner.started
	<-runner.started
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run() = %v, want context.Canceled", err)
	}
	if report.Cancelled != 3 {
		t.Errorf("Cancelled = %d, want every task", report.Cancelled)
	}
	assertNoLeak(t, before)
}

func TestCoordinatorSnapshotAndPrune(t *testing.T) {
	runner := newStubRunner()
	runner.fail["b"] = errors.New("nope")
	c, _ := newTestCoordinator(t, runner)

	if _, err := c.Run(context.Background(), PlanRequest{Tasks: []Task{
		{ID: "a", Description: "a"}, {ID: "b", Description: "b"},
	}}); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if _, err := c.Spawn(SpawnSpec{Name: "idler"}); err != nil {
		t.Fatal(err)
	}

	snap := c.Snapshot()
	if snap.Total != 3 || snap.Complete != 1 || snap.Failed != 1 || snap.Idle != 1 {
		t.Errorf("snapshot = %+v, want 3 total / 1 complete / 1 failed / 1 idle", snap)
	}
	if snap.Max != 5 || !snap.Enabled {
		t.Errorf("snapshot limits = max %d enabled %v", snap.Max, snap.Enabled)
	}
	if !strings.Contains(snap.Summary(), "agents on") {
		t.Errorf("Summary() = %q", snap.Summary())
	}
	if !strings.Contains(snap.String(), "complete") {
		t.Errorf("String() = %q", snap.String())
	}

	if dropped := c.Prune(); dropped != 2 {
		t.Errorf("Prune() dropped %d, want the 2 finished agents", dropped)
	}
	if got := len(c.List()); got != 1 {
		t.Errorf("List() has %d agents after pruning, want only the idle one", got)
	}
}

func TestCoordinatorFinish(t *testing.T) {
	c := NewCoordinator(CoordinatorOptions{})
	a, err := c.Spawn(SpawnSpec{Name: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Finish(a.ID, "all done", nil); err != nil {
		t.Fatalf("Finish() = %v", err)
	}
	if a.State() != StatusComplete || a.Output() != "all done" {
		t.Errorf("agent = %s / %q", a.State(), a.Output())
	}

	b, err := c.Spawn(SpawnSpec{Name: "failing"})
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	if err := c.Finish(b.ID, "", boom); err != nil {
		t.Fatalf("Finish() = %v", err)
	}
	if b.State() != StatusError || !errors.Is(b.Err(), boom) {
		t.Errorf("agent = %s / %v", b.State(), b.Err())
	}
	if err := c.Finish("ghost", "", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("Finish(ghost) = %v, want ErrNotFound", err)
	}
}

func TestCoordinatorRunObjectiveUsesThePlanner(t *testing.T) {
	runner := newStubRunner()
	planner := &Planner{Decomposer: DecomposerFunc(func(_ context.Context, objective string) ([]Task, error) {
		return []Task{
			{ID: "one", Description: "first half of " + objective},
			{ID: "two", Description: "second half", Dependencies: []string{"one"}},
		}, nil
	})}
	c, _ := newTestCoordinator(t, runner, func(o *CoordinatorOptions) { o.Planner = planner })

	report, err := c.RunObjective(context.Background(), "split the work")
	if err != nil {
		t.Fatalf("RunObjective() = %v", err)
	}
	if report.Succeeded != 2 {
		t.Errorf("Succeeded = %d, want 2", report.Succeeded)
	}
	if report.Degraded {
		t.Errorf("Degraded = true (%s)", report.Reason)
	}
}

func TestCoordinatorRunObjectiveDegradesWhenPlanningFails(t *testing.T) {
	runner := newStubRunner()
	planner := &Planner{Decomposer: DecomposerFunc(func(context.Context, string) ([]Task, error) {
		return nil, errors.New("the planner model is down")
	})}
	c, _ := newTestCoordinator(t, runner, func(o *CoordinatorOptions) { o.Planner = planner })

	report, err := c.RunObjective(context.Background(), "do the thing anyway")
	if err != nil {
		t.Fatalf("RunObjective() = %v, want the request to survive a planner failure", err)
	}
	if !report.Degraded || report.Succeeded != 1 {
		t.Errorf("report = degraded %v, succeeded %d; want a single-task fallback that ran", report.Degraded, report.Succeeded)
	}
	if !strings.Contains(report.Summary(), "degraded") {
		t.Errorf("summary does not explain the degradation:\n%s", report.Summary())
	}
}

func TestCoordinatorPlanWithoutAPlanner(t *testing.T) {
	c := NewCoordinator(CoordinatorOptions{})
	plan := c.Plan(context.Background(), "an objective")
	if len(plan.Tasks) != 1 || !plan.Degraded {
		t.Errorf("plan = %+v, want a single degraded task", plan)
	}
}

// TestCoordinatorConcurrentCommands is the -race guard for the frontend
// surface: `/agents list`, `/agents max`, `/agents stop` and `/agents off` can
// all arrive while a run is in flight.
func TestCoordinatorConcurrentCommands(t *testing.T) {
	runner := TaskRunnerFunc(func(ctx context.Context, a *Agent, _ Brief) (TaskOutcome, error) {
		select {
		case <-time.After(2 * time.Millisecond):
		case <-ctx.Done():
			return TaskOutcome{}, ctx.Err()
		}
		return TaskOutcome{Output: "ok"}, nil
	})
	c, _ := newTestCoordinator(t, runner, func(o *CoordinatorOptions) { o.Max = 4 })

	tasks := make([]Task, 20)
	for i := range tasks {
		tasks[i] = Task{ID: fmt.Sprintf("t%d", i), Description: "work"}
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = c.Run(context.Background(), PlanRequest{Objective: "load", Tasks: tasks})
	}()

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = c.List()
				_ = c.Snapshot()
				_ = c.Active()
				_ = c.SetMax(1 + (i+j)%5)
				if agents := c.List(); len(agents) > 0 {
					_ = c.Stop(agents[0].ID)
				}
			}
		}(i)
	}
	wg.Wait()

	if snap := c.Snapshot(); snap.Total == 0 {
		t.Error("no agents were recorded")
	}
}

func TestCoordinatorRetainsBoundedHistory(t *testing.T) {
	c := NewCoordinator(CoordinatorOptions{Retain: 5, MaxAgents: 100})
	for i := 0; i < 20; i++ {
		a, err := c.Spawn(SpawnSpec{Name: fmt.Sprintf("a%d", i)})
		if err != nil {
			t.Fatalf("Spawn(%d) = %v", i, err)
		}
		if err := c.Finish(a.ID, "done", nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(c.List()); got > 5 {
		t.Errorf("List() has %d agents, want the retention bound of 5", got)
	}
}
