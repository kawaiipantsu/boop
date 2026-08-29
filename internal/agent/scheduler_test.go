package agent

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// tracker observes an executor's concurrency and ordering.
type tracker struct {
	mu       sync.Mutex
	running  int
	peak     int
	started  []string
	finished []string
	active   map[string]Task
	conflict string
}

func newTracker() *tracker { return &tracker{active: map[string]Task{}} }

func (tr *tracker) enter(task Task) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, other := range tr.active {
		if task.ConflictsWith(other) {
			tr.conflict = fmt.Sprintf("%s ran while %s held the same write path", task.ID, other.ID)
		}
	}
	tr.active[task.ID] = task
	tr.running++
	if tr.running > tr.peak {
		tr.peak = tr.running
	}
	tr.started = append(tr.started, task.ID)
}

func (tr *tracker) exit(task Task) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	delete(tr.active, task.ID)
	tr.running--
	tr.finished = append(tr.finished, task.ID)
}

func (tr *tracker) snapshot() (peak int, started, finished []string, conflict string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.peak, append([]string(nil), tr.started...), append([]string(nil), tr.finished...), tr.conflict
}

// hold makes an executor that pauses for d, long enough for genuine overlap.
func (tr *tracker) exec(d time.Duration, fail map[string]error) TaskExecutorFunc {
	return func(ctx context.Context, task Task) (TaskOutcome, error) {
		tr.enter(task)
		defer tr.exit(task)
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return TaskOutcome{}, ctx.Err()
		}
		if err, ok := fail[task.ID]; ok {
			return TaskOutcome{}, err
		}
		return TaskOutcome{Output: "did " + task.ID, ToolCalls: 1}, nil
	}
}

func resultsByID(results []TaskResult) map[string]TaskResult {
	out := make(map[string]TaskResult, len(results))
	for _, r := range results {
		out[r.TaskID] = r
	}
	return out
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		tasks   []Task
		wantErr bool
		cycle   []string
	}{
		{
			name:  "independent tasks",
			tasks: []Task{{ID: "a", Description: "a"}, {ID: "b", Description: "b"}},
		},
		{
			name: "linear chain",
			tasks: []Task{
				{ID: "a", Description: "a"},
				{ID: "b", Description: "b", Dependencies: []string{"a"}},
				{ID: "c", Description: "c", Dependencies: []string{"b"}},
			},
		},
		{
			name:  "diamond",
			tasks: []Task{{ID: "a"}, {ID: "b", Dependencies: []string{"a"}}, {ID: "c", Dependencies: []string{"a"}}, {ID: "d", Dependencies: []string{"b", "c"}}},
		},
		{
			name:    "empty id",
			tasks:   []Task{{ID: "", Description: "x"}},
			wantErr: true,
		},
		{
			name:    "duplicate id",
			tasks:   []Task{{ID: "a"}, {ID: "a"}},
			wantErr: true,
		},
		{
			name:    "unknown dependency",
			tasks:   []Task{{ID: "a", Dependencies: []string{"ghost"}}},
			wantErr: true,
		},
		{
			name:    "self dependency",
			tasks:   []Task{{ID: "a", Dependencies: []string{"a"}}},
			wantErr: true,
		},
		{
			name:    "two-task cycle",
			tasks:   []Task{{ID: "a", Dependencies: []string{"b"}}, {ID: "b", Dependencies: []string{"a"}}},
			wantErr: true,
			cycle:   []string{"a", "b", "a"},
		},
		{
			name: "three-task cycle",
			tasks: []Task{
				{ID: "a", Dependencies: []string{"c"}},
				{ID: "b", Dependencies: []string{"a"}},
				{ID: "c", Dependencies: []string{"b"}},
			},
			wantErr: true,
			cycle:   []string{"a", "c", "b", "a"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.tasks)
			if tc.wantErr != (err != nil) {
				t.Fatalf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil {
				return
			}
			if !errors.Is(err, ErrInvalidGraph) {
				t.Errorf("error %v does not match ErrInvalidGraph", err)
			}
			if tc.cycle == nil {
				return
			}
			var ce *CycleError
			if !errors.As(err, &ce) {
				t.Fatalf("error %v is not a *CycleError", err)
			}
			if strings.Join(ce.Path, ",") != strings.Join(tc.cycle, ",") {
				t.Errorf("cycle = %v, want %v", ce.Path, tc.cycle)
			}
		})
	}
}

func TestPathsConflict(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"internal/agent/scheduler.go", "internal/agent/scheduler.go", true},
		{"internal/agent/scheduler.go", "internal/agent/agent.go", false},
		{"internal/agent", "internal/agent/agent.go", true},
		{"internal/agent/agent.go", "internal/agent", true},
		{"internal/agent", "internal/agents", false},
		{"./internal/agent/", "internal/agent", true},
		{"internal\\agent", "internal/agent", true},
		{".", "anything/at/all.go", true},
		{"", "internal/agent", false},
		{"internal/agent", "", false},
		{"a/b/c", "a/b/c/d/e", true},
		{"a/b/c", "a/b/d", false},
	}
	for _, tc := range tests {
		t.Run(tc.a+" vs "+tc.b, func(t *testing.T) {
			if got := PathsConflict(tc.a, tc.b); got != tc.want {
				t.Errorf("PathsConflict(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestTaskConflictsWith(t *testing.T) {
	tests := []struct {
		name string
		a, b Task
		want bool
	}{
		{"no writes", Task{ID: "a"}, Task{ID: "b"}, false},
		{"disjoint writes", Task{ID: "a", Writes: []string{"x.go"}}, Task{ID: "b", Writes: []string{"y.go"}}, false},
		{"same write", Task{ID: "a", Writes: []string{"x.go"}}, Task{ID: "b", Writes: []string{"./x.go"}}, true},
		{"directory contains file", Task{ID: "a", Writes: []string{"pkg"}}, Task{ID: "b", Writes: []string{"pkg/x.go"}}, true},
		{"reads do not conflict", Task{ID: "a", Reads: []string{"x.go"}}, Task{ID: "b", Writes: []string{"x.go"}}, false},
		{"one of several overlaps", Task{ID: "a", Writes: []string{"a.go", "b.go"}}, Task{ID: "b", Writes: []string{"c.go", "b.go"}}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.ConflictsWith(tc.b); got != tc.want {
				t.Errorf("ConflictsWith() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSchedulerRunsIndependentTasksConcurrently(t *testing.T) {
	tr := newTracker()
	s := NewScheduler(tr.exec(20*time.Millisecond, nil), 5)

	tasks := []Task{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}
	results, err := s.Run(context.Background(), tasks)
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("got %d results, want 4", len(results))
	}
	peak, _, _, conflict := tr.snapshot()
	if peak != 4 {
		t.Errorf("peak concurrency = %d, want 4 — independent tasks must overlap", peak)
	}
	if conflict != "" {
		t.Errorf("unexpected conflict: %s", conflict)
	}
	for _, r := range results {
		if r.Status != TaskComplete {
			t.Errorf("task %s = %s, want complete", r.TaskID, r.Status)
		}
		if r.Output != "did "+r.TaskID {
			t.Errorf("task %s output = %q", r.TaskID, r.Output)
		}
	}
}

func TestSchedulerBoundsConcurrency(t *testing.T) {
	for _, max := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("max=%d", max), func(t *testing.T) {
			tr := newTracker()
			s := NewScheduler(tr.exec(10*time.Millisecond, nil), max)
			tasks := make([]Task, 9)
			for i := range tasks {
				tasks[i] = Task{ID: fmt.Sprintf("t%d", i)}
			}
			if _, err := s.Run(context.Background(), tasks); err != nil {
				t.Fatalf("Run() = %v", err)
			}
			peak, started, _, _ := tr.snapshot()
			if peak > max {
				t.Errorf("peak concurrency = %d, want at most %d", peak, max)
			}
			if len(started) != len(tasks) {
				t.Errorf("started %d tasks, want %d", len(started), len(tasks))
			}
		})
	}
}

func TestSchedulerDefaultsToFiveConcurrentAgents(t *testing.T) {
	if got := NewScheduler(nil, 0).Max(); got != DefaultMaxConcurrency {
		t.Errorf("Max() = %d, want the §10 default of %d", got, DefaultMaxConcurrency)
	}
	if DefaultMaxConcurrency != 5 {
		t.Errorf("DefaultMaxConcurrency = %d, want 5", DefaultMaxConcurrency)
	}
}

func TestSchedulerSetMax(t *testing.T) {
	s := NewScheduler(nil, 3)
	if err := s.SetMax(0); err == nil {
		t.Error("SetMax(0) = nil, want an error")
	}
	if err := s.SetMax(-1); err == nil {
		t.Error("SetMax(-1) = nil, want an error")
	}
	if s.Max() != 3 {
		t.Errorf("Max() = %d, want the rejected change to leave it at 3", s.Max())
	}
	if err := s.SetMax(9); err != nil {
		t.Fatalf("SetMax(9) = %v", err)
	}
	if s.Max() != 9 {
		t.Errorf("Max() = %d, want 9", s.Max())
	}
}

func TestSchedulerHonoursDependencies(t *testing.T) {
	tr := newTracker()
	s := NewScheduler(tr.exec(5*time.Millisecond, nil), 5)

	tasks := []Task{
		{ID: "inspect"},
		{ID: "implement", Dependencies: []string{"inspect"}},
		{ID: "tests", Dependencies: []string{"inspect"}},
		{ID: "review", Dependencies: []string{"implement", "tests"}},
	}
	if _, err := s.Run(context.Background(), tasks); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	_, started, finished, _ := tr.snapshot()

	index := func(list []string, id string) int {
		for i, v := range list {
			if v == id {
				return i
			}
		}
		return -1
	}
	if index(started, "implement") < index(finished, "inspect") {
		t.Error("implement started before inspect finished")
	}
	if index(started, "review") < index(finished, "implement") {
		t.Error("review started before implement finished")
	}
	if index(started, "review") < index(finished, "tests") {
		t.Error("review started before tests finished")
	}
}

// TestSchedulerSerialisesConflictingWriters is the §10 guarantee that agents do
// not compete blindly for writes.
func TestSchedulerSerialisesConflictingWriters(t *testing.T) {
	tr := newTracker()
	s := NewScheduler(tr.exec(15*time.Millisecond, nil), 5)

	tasks := []Task{
		{ID: "w1", Writes: []string{"internal/agent/scheduler.go"}},
		{ID: "w2", Writes: []string{"internal/agent/scheduler.go"}},
		{ID: "w3", Writes: []string{"internal/agent"}},         // directory contains the file
		{ID: "free", Writes: []string{"internal/tui/view.go"}}, // must still overlap
	}
	results, err := s.Run(context.Background(), tasks)
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	peak, _, _, conflict := tr.snapshot()
	if conflict != "" {
		t.Fatalf("conflicting writers overlapped: %s", conflict)
	}
	if peak < 2 {
		t.Errorf("peak concurrency = %d, want the non-conflicting task to overlap", peak)
	}
	if peak > 2 {
		t.Errorf("peak concurrency = %d, want at most 2 with three mutually conflicting writers", peak)
	}
	for _, r := range results {
		if r.Status != TaskComplete {
			t.Errorf("task %s = %s, want complete", r.TaskID, r.Status)
		}
	}
}

func TestSchedulerFailureBlocksDependentsAndSparesTheRest(t *testing.T) {
	tr := newTracker()
	boom := errors.New("compile error")
	s := NewScheduler(tr.exec(time.Millisecond, map[string]error{"implement": boom}), 5)

	tasks := []Task{
		{ID: "implement"},
		{ID: "tests", Dependencies: []string{"implement"}},
		{ID: "review", Dependencies: []string{"tests"}},
		{ID: "docs"},
	}
	results, err := s.Run(context.Background(), tasks)
	if err != nil {
		t.Fatalf("Run() = %v, want a failing task not to fail the run", err)
	}
	byID := resultsByID(results)

	if byID["implement"].Status != TaskFailed {
		t.Errorf("implement = %s, want failed", byID["implement"].Status)
	}
	if !errors.Is(byID["implement"].Err, boom) {
		t.Errorf("implement Err = %v, want %v", byID["implement"].Err, boom)
	}
	for _, id := range []string{"tests", "review"} {
		if byID[id].Status != TaskBlocked {
			t.Errorf("%s = %s, want blocked (transitively)", id, byID[id].Status)
		}
		if byID[id].Error == "" {
			t.Errorf("%s has no explanation of why it was blocked", id)
		}
	}
	if byID["docs"].Status != TaskComplete {
		t.Errorf("docs = %s, want complete — an unrelated failure must not stop it", byID["docs"].Status)
	}
	_, started, _, _ := tr.snapshot()
	sort.Strings(started)
	if strings.Join(started, ",") != "docs,implement" {
		t.Errorf("started = %v, want only docs and implement to have run", started)
	}
}

func TestSchedulerFailureFreesWriteClaims(t *testing.T) {
	tr := newTracker()
	s := NewScheduler(tr.exec(time.Millisecond, map[string]error{"a": errors.New("nope")}), 5)
	tasks := []Task{
		{ID: "a", Writes: []string{"shared.go"}},
		{ID: "b", Writes: []string{"shared.go"}},
	}
	results, err := s.Run(context.Background(), tasks)
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	byID := resultsByID(results)
	if byID["b"].Status != TaskComplete {
		t.Errorf("b = %s, want complete — a's failure must release its write claim", byID["b"].Status)
	}
}

func TestSchedulerRecoversFromAPanickingTask(t *testing.T) {
	s := NewScheduler(TaskExecutorFunc(func(_ context.Context, task Task) (TaskOutcome, error) {
		if task.ID == "bad" {
			panic("tool exploded")
		}
		return TaskOutcome{Output: "fine"}, nil
	}), 2)

	results, err := s.Run(context.Background(), []Task{{ID: "bad"}, {ID: "good"}})
	if err != nil {
		t.Fatalf("Run() = %v, want the panic contained in a result", err)
	}
	byID := resultsByID(results)
	if byID["bad"].Status != TaskFailed {
		t.Errorf("bad = %s, want failed", byID["bad"].Status)
	}
	if !strings.Contains(byID["bad"].Error, "panicked") {
		t.Errorf("bad error = %q, want it to mention the panic", byID["bad"].Error)
	}
	if byID["good"].Status != TaskComplete {
		t.Errorf("good = %s, want complete", byID["good"].Status)
	}
}

func TestSchedulerCancellationStopsEverything(t *testing.T) {
	before := runtime.NumGoroutine()

	started := make(chan struct{}, 8)
	s := NewScheduler(TaskExecutorFunc(func(ctx context.Context, task Task) (TaskOutcome, error) {
		started <- struct{}{}
		<-ctx.Done() // never finishes on its own
		return TaskOutcome{}, ctx.Err()
	}), 2)

	tasks := []Task{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var results []TaskResult
	var err error
	go func() {
		defer close(done)
		results, err = s.Run(ctx, tasks)
	}()

	<-started
	<-started
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after cancellation")
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run() = %v, want context.Canceled", err)
	}
	if len(results) != len(tasks) {
		t.Fatalf("got %d results, want one per task", len(results))
	}
	for _, r := range results {
		if r.Status != TaskCancelled {
			t.Errorf("task %s = %s, want cancelled", r.TaskID, r.Status)
		}
	}
	assertNoLeak(t, before)
}

func TestSchedulerCancelledBeforeStart(t *testing.T) {
	ran := false
	s := NewScheduler(TaskExecutorFunc(func(context.Context, Task) (TaskOutcome, error) {
		ran = true
		return TaskOutcome{}, nil
	}), 2)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	results, err := s.Run(ctx, []Task{{ID: "a"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v, want context.Canceled", err)
	}
	if ran {
		t.Error("a task ran even though the context was already cancelled")
	}
	if len(results) != 1 || results[0].Status != TaskCancelled {
		t.Errorf("results = %+v, want one cancelled task", results)
	}
}

func TestSchedulerRejectsBadInput(t *testing.T) {
	tests := []struct {
		name  string
		sched *Scheduler
		tasks []Task
		want  error
	}{
		{"no executor", &Scheduler{}, []Task{{ID: "a"}}, ErrNoExecutor},
		{
			name:  "cycle",
			sched: NewScheduler(TaskExecutorFunc(func(context.Context, Task) (TaskOutcome, error) { return TaskOutcome{}, nil }), 2),
			tasks: []Task{{ID: "a", Dependencies: []string{"b"}}, {ID: "b", Dependencies: []string{"a"}}},
			want:  ErrInvalidGraph,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.sched.Run(context.Background(), tc.tasks)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Run() = %v, want %v", err, tc.want)
			}
		})
	}

	s := NewScheduler(TaskExecutorFunc(func(context.Context, Task) (TaskOutcome, error) { return TaskOutcome{}, nil }), 2)
	results, err := s.Run(context.Background(), nil)
	if err != nil || results != nil {
		t.Errorf("Run(nil) = %v, %v, want no results and no error", results, err)
	}
}

// TestSchedulerRunLeavesNoGoroutines covers the ordinary path as well as the
// cancelled one: a scheduler that leaks a goroutine per task would slowly eat
// a long session.
func TestSchedulerRunLeavesNoGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()
	tr := newTracker()
	s := NewScheduler(tr.exec(time.Millisecond, map[string]error{"c": errors.New("x")}), 3)

	tasks := []Task{{ID: "a"}, {ID: "b", Dependencies: []string{"a"}}, {ID: "c"}, {ID: "d", Dependencies: []string{"c"}}}
	if _, err := s.Run(context.Background(), tasks); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	assertNoLeak(t, before)
}

// assertNoLeak polls, because a goroutine that has been asked to stop takes a
// moment to actually be gone.
func assertNoLeak(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := runtime.NumGoroutine()
		if got <= before {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("goroutines = %d, want at most the %d there were before", got, before)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
