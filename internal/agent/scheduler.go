package agent

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kawaiipantsu/boop/internal/provider"
)

// TaskStatus is the lifecycle state of a scheduled task.
type TaskStatus string

// Task statuses. Blocked and Cancelled are distinct from Failed on purpose: a
// task that never ran because a dependency died did not itself fail, and the
// difference matters when the run is reported back to the user.
const (
	TaskPending   TaskStatus = "pending"
	TaskRunning   TaskStatus = "running"
	TaskComplete  TaskStatus = "complete"
	TaskFailed    TaskStatus = "failed"
	TaskBlocked   TaskStatus = "blocked"
	TaskCancelled TaskStatus = "cancelled"
)

// Terminal reports whether the task has reached a final state.
func (s TaskStatus) Terminal() bool {
	switch s {
	case TaskComplete, TaskFailed, TaskBlocked, TaskCancelled:
		return true
	}
	return false
}

// Task is one unit of scheduled work (§11).
//
// The first five fields are the shape the specification fixes. The rest are
// additive and exist to serve two §10 requirements that the minimal shape
// cannot express: Writes/Reads let the scheduler keep two writers off the same
// path, and Requirements/Files/AllowedTools carry the isolated context a worker
// is given instead of a copy of the main conversation.
type Task struct {
	ID           string     `json:"id"`
	Description  string     `json:"description"`
	Dependencies []string   `json:"dependencies,omitempty"`
	AgentID      string     `json:"agent_id,omitempty"`
	Status       TaskStatus `json:"status,omitempty"`

	// Writes lists workspace-relative paths this task will modify. Two tasks
	// whose Writes overlap never run at the same time.
	Writes []string `json:"writes,omitempty"`
	// Reads lists paths the task needs to look at. It is context for the
	// worker, not a scheduling constraint.
	Reads []string `json:"reads,omitempty"`
	// AllowedTools restricts the worker's tool set. Empty derives a
	// conservative default from the task shape (see DefaultAllowedTools).
	AllowedTools []string `json:"allowed_tools,omitempty"`
	// Requirements are the user requirements that bear on this task.
	Requirements []string `json:"requirements,omitempty"`
	// Validation states how the task's completion is checked.
	Validation string `json:"validation,omitempty"`
}

// writePaths returns the task's normalised write set, dropping empties.
func (t Task) writePaths() []string {
	out := make([]string, 0, len(t.Writes))
	for _, p := range t.Writes {
		if n := NormalizePath(p); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// ConflictsWith reports whether running t and other concurrently would let two
// agents write the same place (§10: "agents should not compete blindly for
// writes").
func (t Task) ConflictsWith(other Task) bool {
	for _, a := range t.writePaths() {
		for _, b := range other.writePaths() {
			if PathsConflict(a, b) {
				return true
			}
		}
	}
	return false
}

// NormalizePath puts a declared path into the comparable form the conflict
// check uses: slash-separated, cleaned, without a trailing separator. An empty
// or whitespace-only path normalises to the empty string and is ignored.
//
// Backslashes count as separators on every platform, not just Windows: a plan
// may be authored anywhere, and the only cost of reading "internal\agent" as a
// directory on Linux is that two tasks are serialised when they need not have
// been. Missing a genuine conflict costs a corrupted file.
func NormalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = path.Clean(strings.ReplaceAll(filepath.ToSlash(p), `\`, "/"))
	if p != "/" {
		p = strings.TrimSuffix(p, "/")
	}
	return p
}

// PathsConflict reports whether writing both paths could touch the same file.
//
// Containment counts: a task rewriting "internal/agent" conflicts with one
// editing "internal/agent/scheduler.go", because the directory writer may
// replace or delete the file underneath the other agent. "." is the workspace
// root and therefore conflicts with everything.
func PathsConflict(a, b string) bool {
	a, b = NormalizePath(a), NormalizePath(b)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	if a == "." || b == "." {
		return true
	}
	return strings.HasPrefix(b, a+"/") || strings.HasPrefix(a, b+"/")
}

// TaskOutcome is what an executor reports for one successful task.
type TaskOutcome struct {
	// AgentID is the agent that carried out the task.
	AgentID string `json:"agent_id,omitempty"`
	// Output is the worker's report, which becomes part of the aggregate.
	Output string `json:"output,omitempty"`
	// Usage is the task's token cost.
	Usage provider.Usage `json:"usage,omitempty"`
	// ToolCalls counts tools the worker actually executed.
	ToolCalls int `json:"tool_calls,omitempty"`
	// Truncated reports that the worker hit its iteration limit, so the
	// output may be incomplete even though nothing errored.
	Truncated bool `json:"truncated,omitempty"`
}

// TaskResult is the scheduler's record of one task, run or not.
type TaskResult struct {
	TaskID     string         `json:"task_id"`
	AgentID    string         `json:"agent_id,omitempty"`
	Status     TaskStatus     `json:"status"`
	Output     string         `json:"output,omitempty"`
	Error      string         `json:"error,omitempty"`
	Usage      provider.Usage `json:"usage,omitempty"`
	ToolCalls  int            `json:"tool_calls,omitempty"`
	Truncated  bool           `json:"truncated,omitempty"`
	StartedAt  time.Time      `json:"started_at,omitempty"`
	FinishedAt time.Time      `json:"finished_at,omitempty"`
	Duration   time.Duration  `json:"duration,omitempty"`

	// Err is the underlying failure, kept out of JSON so callers can inspect
	// it with errors.Is while frontends see the message in Error.
	Err error `json:"-"`
}

// TaskExecutor runs a single scheduled task to completion.
//
// It is an interface so the scheduler can be exercised without a model,
// a provider or the network. The coordinator's implementation spawns an agent
// and runs one app.Loop over an isolated context.
type TaskExecutor interface {
	ExecuteTask(ctx context.Context, task Task) (TaskOutcome, error)
}

// TaskExecutorFunc adapts a function to TaskExecutor.
type TaskExecutorFunc func(ctx context.Context, task Task) (TaskOutcome, error)

// ExecuteTask implements TaskExecutor.
func (f TaskExecutorFunc) ExecuteTask(ctx context.Context, task Task) (TaskOutcome, error) {
	return f(ctx, task)
}

// Scheduler errors.
var (
	// ErrNoExecutor means the scheduler was run without anything to run tasks.
	ErrNoExecutor = errors.New("scheduler: an executor is required")
	// ErrInvalidGraph covers duplicate, empty and unknown task identifiers.
	ErrInvalidGraph = errors.New("scheduler: invalid task graph")
	// ErrNoProgress means the scheduler could dispatch nothing while work
	// remained. It should be unreachable — cycles are rejected up front — and
	// exists so an unforeseen bug surfaces as an error instead of a hang.
	ErrNoProgress = errors.New("scheduler: no task can make progress")
)

// CycleError reports a dependency cycle, naming the loop it found.
//
// Refusing the graph is the point: a cycle scheduled naively is a deadlock, and
// a deadlock in an agent runtime looks to the user like Boop hanging.
type CycleError struct {
	// Path is the cycle, starting and ending on the same task.
	Path []string
}

// Error implements error.
func (e *CycleError) Error() string {
	return "scheduler: dependency cycle: " + strings.Join(e.Path, " -> ")
}

// Is lets errors.Is(err, ErrInvalidGraph) match a cycle, so callers that only
// care that the plan was unusable do not have to enumerate reasons.
func (e *CycleError) Is(target error) bool { return target == ErrInvalidGraph }

// Validate checks that tasks form a schedulable graph.
//
// It rejects empty and duplicate identifiers, dependencies on tasks that are
// not present, self-dependencies, and cycles.
func Validate(tasks []Task) error {
	byID := make(map[string]Task, len(tasks))
	for i, t := range tasks {
		id := strings.TrimSpace(t.ID)
		if id == "" {
			return fmt.Errorf("%w: task %d has no id", ErrInvalidGraph, i)
		}
		if _, dup := byID[id]; dup {
			return fmt.Errorf("%w: duplicate task id %q", ErrInvalidGraph, id)
		}
		byID[id] = t
	}
	for _, t := range tasks {
		for _, dep := range t.Dependencies {
			dep = strings.TrimSpace(dep)
			if dep == t.ID {
				return fmt.Errorf("%w: task %q depends on itself", ErrInvalidGraph, t.ID)
			}
			if _, ok := byID[dep]; !ok {
				return fmt.Errorf("%w: task %q depends on unknown task %q", ErrInvalidGraph, t.ID, dep)
			}
		}
	}
	return findCycle(tasks, byID)
}

// findCycle runs an iterative-friendly depth-first search and reports the first
// back edge it finds, together with the path that closes the loop.
func findCycle(tasks []Task, byID map[string]Task) error {
	const (
		white = 0 // unvisited
		grey  = 1 // on the current path
		black = 2 // fully explored
	)
	colour := make(map[string]int, len(tasks))
	var stack []string

	var visit func(id string) error
	visit = func(id string) error {
		colour[id] = grey
		stack = append(stack, id)
		for _, dep := range byID[id].Dependencies {
			dep = strings.TrimSpace(dep)
			switch colour[dep] {
			case grey:
				// Trim the stack to the start of the cycle so the message
				// names only the loop, not how we got there.
				start := 0
				for i, s := range stack {
					if s == dep {
						start = i
						break
					}
				}
				return &CycleError{Path: append(append([]string(nil), stack[start:]...), dep)}
			case white:
				if err := visit(dep); err != nil {
					return err
				}
			}
		}
		stack = stack[:len(stack)-1]
		colour[id] = black
		return nil
	}

	for _, t := range tasks {
		if colour[t.ID] == white {
			if err := visit(t.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// DefaultMaxConcurrency is the §10 default for simultaneously running agents.
const DefaultMaxConcurrency = 5

// Scheduler runs a task graph with bounded concurrency.
//
// Three constraints are enforced at dispatch time and nowhere else, which is
// what keeps the rest of the runtime free of scheduling logic: dependencies
// must be complete, the global concurrency limit must not be exceeded, and no
// two concurrently running tasks may write conflicting paths.
type Scheduler struct {
	// Executor runs individual tasks. Required.
	Executor TaskExecutor
	// Now supplies timestamps; nil uses time.Now.
	Now func() time.Time

	mu  sync.Mutex
	sem *semaphore
}

// NewScheduler returns a scheduler bounded to max concurrent tasks. A
// non-positive max uses DefaultMaxConcurrency.
func NewScheduler(exec TaskExecutor, max int) *Scheduler {
	return &Scheduler{Executor: exec, sem: newSemaphore(max)}
}

// slots returns the shared concurrency pool, creating it for a zero-value
// Scheduler so the type is usable without its constructor.
func (s *Scheduler) slots() *semaphore {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sem == nil {
		s.sem = newSemaphore(DefaultMaxConcurrency)
	}
	return s.sem
}

// Max returns the concurrency limit.
func (s *Scheduler) Max() int { return s.slots().limitOf() }

// SetMax changes the concurrency limit, backing `/agents max <int>`.
//
// It takes effect on the next dispatch decision, so raising the limit during a
// run releases more work immediately. Lowering it never interrupts tasks that
// are already running; it only stops new ones starting.
func (s *Scheduler) SetMax(n int) error {
	if n < 1 {
		return fmt.Errorf("agents max must be at least 1, got %d", n)
	}
	s.slots().setLimit(n)
	return nil
}

func (s *Scheduler) clock() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Run executes the graph and returns one result per task, in input order.
//
// A task that fails does not stop the run: its dependents are marked blocked
// and everything independent of it keeps going. Run returns an error only when
// the graph itself is unusable or the context is cancelled — in the latter case
// the partial results are still returned, and no goroutine outlives the call.
//
// Concurrency slots are shared with every other Run of the same scheduler, so
// the limit is global. A Run started from inside a task — a worker that plans
// and delegates further — gives up its own slot for the duration, because a
// parent that holds a slot while waiting for its children is the classic way to
// deadlock a nested scheduler.
func (s *Scheduler) Run(ctx context.Context, tasks []Task) ([]TaskResult, error) {
	if s.Executor == nil {
		return nil, ErrNoExecutor
	}
	if len(tasks) == 0 {
		return nil, nil
	}
	if err := Validate(tasks); err != nil {
		return nil, err
	}

	slots := s.slots()
	if holdsPermit(ctx) {
		slots.release()
		defer slots.reclaim()
	}

	st := newRunState(tasks)
	done := make(chan TaskResult, len(tasks))
	held := make(map[string]string, len(tasks))
	var wg sync.WaitGroup
	running := 0
	cancelled := ctx.Err() != nil
	taskCtx := withPermit(ctx)

	for !cancelled {
		for {
			t := st.nextReady(held)
			if t == nil {
				break
			}
			if !slots.tryAcquire() {
				if running > 0 {
					break // wait for one of our own tasks to free a slot
				}
				// Nothing of ours is running, so waiting on done would hang.
				// Block for a slot instead, which another run will free.
				if err := slots.acquire(ctx); err != nil {
					cancelled = true
					break
				}
			}
			st.status[t.ID] = TaskRunning
			for _, w := range t.writePaths() {
				held[w] = t.ID
			}
			running++
			wg.Add(1)
			task := *t
			go func() {
				defer wg.Done()
				defer slots.release()
				done <- s.execute(taskCtx, task)
			}()
		}
		if cancelled {
			break
		}

		if running == 0 {
			if st.pending() == 0 {
				break
			}
			// Unreachable with a validated graph; reported rather than hung.
			return st.collect(), fmt.Errorf("%w: %s", ErrNoProgress, strings.Join(st.pendingIDs(), ", "))
		}

		select {
		case res := <-done:
			running--
			st.apply(res, held)
		case <-ctx.Done():
			cancelled = true
		}
	}

	// Every dispatched goroutine is joined before returning, so a cancelled
	// run leaves nothing behind.
	wg.Wait()
	close(done)
	for res := range done {
		if st.status[res.TaskID] == TaskRunning {
			st.apply(res, held)
		}
	}

	if err := ctx.Err(); err != nil {
		st.cancelRemaining(s.clock())
		return st.collect(), err
	}
	return st.collect(), nil
}

// execute runs one task, converting a panic into a failed result so a broken
// tool or executor cannot take the process down with it.
func (s *Scheduler) execute(ctx context.Context, task Task) (res TaskResult) {
	started := s.clock()
	res = TaskResult{TaskID: task.ID, StartedAt: started}
	defer func() {
		if r := recover(); r != nil {
			res.Err = fmt.Errorf("task %s panicked: %v", task.ID, r)
		}
		res.FinishedAt = s.clock()
		res.Duration = res.FinishedAt.Sub(started)
	}()

	outcome, err := s.Executor.ExecuteTask(ctx, task)
	res.AgentID = outcome.AgentID
	res.Output = outcome.Output
	res.Usage = outcome.Usage
	res.ToolCalls = outcome.ToolCalls
	res.Truncated = outcome.Truncated
	res.Err = err
	return res
}

// runState is the scheduler's private bookkeeping for one Run.
type runState struct {
	order      []string
	byID       map[string]*Task
	status     map[string]TaskStatus
	results    map[string]TaskResult
	dependents map[string][]string
}

func newRunState(tasks []Task) *runState {
	st := &runState{
		order:      make([]string, 0, len(tasks)),
		byID:       make(map[string]*Task, len(tasks)),
		status:     make(map[string]TaskStatus, len(tasks)),
		results:    make(map[string]TaskResult, len(tasks)),
		dependents: make(map[string][]string, len(tasks)),
	}
	for i := range tasks {
		t := tasks[i]
		t.Status = TaskPending
		st.order = append(st.order, t.ID)
		st.byID[t.ID] = &t
		st.status[t.ID] = TaskPending
	}
	for _, t := range st.byID {
		for _, dep := range t.Dependencies {
			dep = strings.TrimSpace(dep)
			st.dependents[dep] = append(st.dependents[dep], t.ID)
		}
	}
	for dep := range st.dependents {
		sort.Strings(st.dependents[dep])
	}
	return st
}

// nextReady picks the first pending task whose dependencies are complete and
// whose writes do not collide with a task already running.
func (st *runState) nextReady(held map[string]string) *Task {
	for _, id := range st.order {
		if st.status[id] != TaskPending {
			continue
		}
		t := st.byID[id]
		if !st.depsComplete(t) {
			continue
		}
		if conflicts(t, held) {
			continue
		}
		return t
	}
	return nil
}

func (st *runState) depsComplete(t *Task) bool {
	for _, dep := range t.Dependencies {
		if st.status[strings.TrimSpace(dep)] != TaskComplete {
			return false
		}
	}
	return true
}

func conflicts(t *Task, held map[string]string) bool {
	for _, w := range t.writePaths() {
		for owned := range held {
			if PathsConflict(w, owned) {
				return true
			}
		}
	}
	return false
}

// apply records a finished task, frees its write claims and cascades failure.
func (st *runState) apply(res TaskResult, held map[string]string) {
	for p, owner := range held {
		if owner == res.TaskID {
			delete(held, p)
		}
	}

	switch {
	case res.Err == nil:
		res.Status = TaskComplete
	case errors.Is(res.Err, context.Canceled), errors.Is(res.Err, context.DeadlineExceeded):
		res.Status = TaskCancelled
		res.Error = res.Err.Error()
	default:
		res.Status = TaskFailed
		res.Error = res.Err.Error()
	}
	st.status[res.TaskID] = res.Status
	st.results[res.TaskID] = res

	if res.Status == TaskComplete {
		return
	}
	st.failDependents(res.TaskID, res.Status, res.FinishedAt)
}

// failDependents propagates a dead task's outcome to everything downstream.
//
// This is the defined failure policy: dependents never run on the output of a
// task that did not produce any, and they are reported as blocked rather than
// failed, so the user can see which task was the actual cause. A cancelled task
// propagates cancellation instead, because "blocked" would imply the run failed
// when in fact the user stopped it.
func (st *runState) failDependents(id string, cause TaskStatus, at time.Time) {
	downstream := TaskBlocked
	if cause == TaskCancelled {
		downstream = TaskCancelled
	}
	queue := append([]string(nil), st.dependents[id]...)
	seen := map[string]bool{id: true}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		if seen[next] {
			continue
		}
		seen[next] = true
		if st.status[next] != TaskPending {
			continue
		}
		st.status[next] = downstream
		st.results[next] = TaskResult{
			TaskID:     next,
			Status:     downstream,
			Error:      fmt.Sprintf("dependency %s %s", id, cause),
			FinishedAt: at,
		}
		queue = append(queue, st.dependents[next]...)
	}
}

// cancelRemaining marks everything that never started as cancelled.
func (st *runState) cancelRemaining(at time.Time) {
	for _, id := range st.order {
		if st.status[id] == TaskPending || st.status[id] == TaskRunning {
			st.status[id] = TaskCancelled
			st.results[id] = TaskResult{
				TaskID:     id,
				Status:     TaskCancelled,
				Error:      context.Canceled.Error(),
				Err:        context.Canceled,
				FinishedAt: at,
			}
		}
	}
}

func (st *runState) pending() int {
	n := 0
	for _, id := range st.order {
		if st.status[id] == TaskPending {
			n++
		}
	}
	return n
}

func (st *runState) pendingIDs() []string {
	var ids []string
	for _, id := range st.order {
		if st.status[id] == TaskPending {
			ids = append(ids, id)
		}
	}
	return ids
}

// collect returns results in input order, inventing a record for any task that
// somehow has none so the caller always gets one result per task.
func (st *runState) collect() []TaskResult {
	out := make([]TaskResult, 0, len(st.order))
	for _, id := range st.order {
		res, ok := st.results[id]
		if !ok {
			res = TaskResult{TaskID: id, Status: st.status[id]}
		}
		out = append(out, res)
	}
	return out
}
