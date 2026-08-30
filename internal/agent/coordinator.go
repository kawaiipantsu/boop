package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kawaiipantsu/boop/internal/app"
)

// Recursion and fleet bounds (§11: "the scheduler must prevent uncontrolled
// recursive spawning").
const (
	// DefaultMaxDepth is how deep agent trees may nest. Top-level agents are
	// depth 1, so the default allows a planner, its workers and one level of
	// helper below that.
	DefaultMaxDepth = 3
	// DefaultMaxAgents caps the agents in one tree — one objective. The cap is
	// per tree rather than per process so a long session is never quietly
	// starved by work it finished an hour ago.
	DefaultMaxAgents = 20
	// DefaultRetainedAgents bounds how many finished agents are remembered for
	// `/agents list` before the oldest are dropped.
	DefaultRetainedAgents = 200
)

// Coordinator errors.
var (
	// ErrDisabled reports that `/agents off` is in force.
	ErrDisabled = errors.New("agents are disabled")
	// ErrNotFound reports an unknown agent id.
	ErrNotFound = errors.New("no such agent")
	// ErrAmbiguousID reports an id prefix matching more than one agent.
	ErrAmbiguousID = errors.New("ambiguous agent id")
	// ErrMaxDepth reports a spawn that would nest too deeply.
	ErrMaxDepth = errors.New("agent spawn depth exceeded")
	// ErrAgentBudget reports a spawn that would exceed the per-objective
	// agent budget.
	ErrAgentBudget = errors.New("agent budget exhausted")
	// ErrNoRunner reports a coordinator with nothing to execute tasks.
	ErrNoRunner = errors.New("agent: no task runner is configured")
	// ErrNoTasks reports a run with an empty plan.
	ErrNoTasks = errors.New("agent: the plan contains no tasks")
)

// SpawnSpec describes an agent to create.
type SpawnSpec struct {
	Name     string
	Task     string
	ParentID string
	Provider string
	Model    string
}

// CoordinatorOptions configures a Coordinator.
type CoordinatorOptions struct {
	// Runner executes tasks. Required for Run; Spawn works without it.
	Runner TaskRunner
	// Planner decomposes objectives. Nil makes RunObjective a single task.
	Planner *Planner
	// Scope decides each worker's isolated context. Nil uses defaults.
	Scope *Scope
	// Bus receives agent lifecycle events.
	Bus *app.Bus
	// SessionID labels published events.
	SessionID string
	// Provider and Model are the defaults workers inherit.
	Provider string
	Model    string
	// Max is the concurrency limit; zero uses DefaultMaxConcurrency.
	Max int
	// MaxDepth and MaxAgents bound recursive spawning; zero uses the defaults.
	MaxDepth  int
	MaxAgents int
	// Retain bounds remembered finished agents; zero uses the default.
	Retain int
	// Disabled starts the coordinator in the `/agents off` state.
	Disabled bool
	// Now supplies timestamps; nil uses time.Now.
	Now func() time.Time
}

// Coordinator owns the agent fleet.
//
// It is the single object the frontends talk to: `/agents` reads Snapshot,
// `/agents list` reads List, `/agents stop <id>` calls Stop, `/agents max <n>`
// calls SetMax and `/agents on|off` calls SetEnabled. None of those methods
// parse anything — command parsing belongs to the frontends (§2.3).
type Coordinator struct {
	runner  TaskRunner
	planner *Planner
	scope   *Scope
	sched   *Scheduler
	bus     *app.Bus

	sessionID string
	provider  string
	model     string
	now       func() time.Time

	mu        sync.Mutex
	enabled   bool
	maxDepth  int
	maxAgents int
	retain    int
	agents    map[string]*record
	order     []string
}

// record is the coordinator's private handle on a live agent.
type record struct {
	agent  *Agent
	done   chan struct{}
	once   sync.Once
	cancel context.CancelFunc
}

func (r *record) finish() { r.once.Do(func() { close(r.done) }) }

// NewCoordinator assembles a coordinator.
func NewCoordinator(opts CoordinatorOptions) *Coordinator {
	c := &Coordinator{
		runner:    opts.Runner,
		planner:   opts.Planner,
		scope:     opts.Scope,
		bus:       opts.Bus,
		sessionID: opts.SessionID,
		provider:  opts.Provider,
		model:     opts.Model,
		now:       opts.Now,
		enabled:   !opts.Disabled,
		maxDepth:  opts.MaxDepth,
		maxAgents: opts.MaxAgents,
		retain:    opts.Retain,
		agents:    make(map[string]*record),
	}
	if c.scope == nil {
		c.scope = &Scope{}
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.maxDepth <= 0 {
		c.maxDepth = DefaultMaxDepth
	}
	if c.maxAgents <= 0 {
		c.maxAgents = DefaultMaxAgents
	}
	if c.retain <= 0 {
		c.retain = DefaultRetainedAgents
	}
	c.sched = NewScheduler(TaskExecutorFunc(c.executeTask), opts.Max)
	c.sched.Now = c.now
	return c
}

// Enabled reports whether agents may run (`/agents on|off`).
func (c *Coordinator) Enabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enabled
}

// SetEnabled turns the agent system on or off.
//
// Turning it off also stops everything in flight: a user typing `/agents off`
// while five workers are editing files means "stop", not "finish what you
// started and then stop accepting more".
func (c *Coordinator) SetEnabled(on bool) {
	c.mu.Lock()
	c.enabled = on
	c.mu.Unlock()
	if !on {
		c.StopAll()
	}
}

// Max returns the concurrency limit.
func (c *Coordinator) Max() int { return c.sched.Max() }

// SetMax changes the concurrency limit (`/agents max <int>`). It applies to
// work that has not started yet, including work in a run already under way.
func (c *Coordinator) SetMax(n int) error { return c.sched.SetMax(n) }

// MaxDepth returns the recursion depth cap.
func (c *Coordinator) MaxDepth() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maxDepth
}

// SetMaxDepth changes the recursion depth cap.
func (c *Coordinator) SetMaxDepth(n int) error {
	if n < 1 {
		return fmt.Errorf("agent depth must be at least 1, got %d", n)
	}
	c.mu.Lock()
	c.maxDepth = n
	c.mu.Unlock()
	return nil
}

// MaxAgents returns the per-objective agent budget.
func (c *Coordinator) MaxAgents() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maxAgents
}

// SetMaxAgents changes the per-objective agent budget.
func (c *Coordinator) SetMaxAgents(n int) error {
	if n < 1 {
		return fmt.Errorf("agent budget must be at least 1, got %d", n)
	}
	c.mu.Lock()
	c.maxAgents = n
	c.mu.Unlock()
	return nil
}

// Spawn creates an idle agent without running it.
//
// The caller owns the returned agent's lifecycle and must close it out with
// Finish or Stop. Internal task execution uses the same path but starts the
// agent working immediately, so a running agent is never observable as idle.
func (c *Coordinator) Spawn(spec SpawnSpec) (*Agent, error) {
	return c.spawn(spec, StatusIdle, nil)
}

// spawn registers an agent together with the cancel function that stops it.
//
// The two happen under one lock deliberately: registering first and attaching
// the canceller afterwards would leave a window in which `/agents stop` marks
// an agent cancelled while its worker keeps running, unaware.
func (c *Coordinator) spawn(spec SpawnSpec, initial AgentStatus, cancel context.CancelFunc) (*Agent, error) {
	c.mu.Lock()

	if !c.enabled {
		c.mu.Unlock()
		return nil, ErrDisabled
	}

	depth := 1
	root := ""
	if spec.ParentID != "" {
		parent, ok := c.agents[spec.ParentID]
		if !ok {
			c.mu.Unlock()
			return nil, fmt.Errorf("%w: parent %s", ErrNotFound, shortID(spec.ParentID))
		}
		depth = parent.agent.Depth() + 1
		root = parent.agent.RootID()
	}
	if depth > c.maxDepth {
		limit := c.maxDepth
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: %s would be depth %d, the limit is %d",
			ErrMaxDepth, firstLine(spec.Task), depth, limit)
	}
	if root != "" {
		if n := c.treeSizeLocked(root); n >= c.maxAgents {
			limit := c.maxAgents
			c.mu.Unlock()
			return nil, fmt.Errorf("%w: this objective has already used %d of %d agents",
				ErrAgentBudget, n, limit)
		}
	}

	provider := spec.Provider
	if provider == "" {
		provider = c.provider
	}
	model := spec.Model
	if model == "" {
		model = c.model
	}

	a := NewAgent(AgentSpec{
		Name:      spec.Name,
		Task:      spec.Task,
		Provider:  provider,
		Model:     model,
		ParentID:  spec.ParentID,
		Depth:     depth,
		RootID:    root,
		Status:    initial,
		Bus:       c.bus,
		SessionID: c.sessionID,
		Now:       c.now,
		Silent:    true,
	})
	c.agents[a.ID] = &record{agent: a, done: make(chan struct{}), cancel: cancel}
	c.order = append(c.order, a.ID)
	c.trimLocked()
	c.mu.Unlock()

	// Published after the lock is released: Bus.Publish is synchronous, and a
	// subscriber that reads the fleet back would otherwise deadlock.
	a.Announce()
	return a, nil
}

// treeSizeLocked counts the agents belonging to one objective's tree.
func (c *Coordinator) treeSizeLocked(root string) int {
	n := 0
	for _, rec := range c.agents {
		if rec.agent.RootID() == root {
			n++
		}
	}
	return n
}

// trimLocked drops the oldest finished agents once the retention bound is hit,
// so a long session cannot grow the fleet list without limit.
func (c *Coordinator) trimLocked() {
	for len(c.order) > c.retain {
		dropped := false
		for i, id := range c.order {
			rec, ok := c.agents[id]
			if !ok || rec.agent.State().Terminal() {
				delete(c.agents, id)
				c.order = append(c.order[:i], c.order[i+1:]...)
				dropped = true
				break
			}
		}
		if !dropped {
			return // everything still live; keep them all
		}
	}
}

// Finish closes out an agent created with Spawn.
func (c *Coordinator) Finish(id, output string, cause error) error {
	rec, err := c.lookup(id)
	if err != nil {
		return err
	}
	if cause != nil {
		_ = rec.agent.Fail(cause)
	} else {
		_ = rec.agent.Complete(output)
	}
	rec.finish()
	return nil
}

// Agent returns a snapshot of one agent, resolving a short id prefix.
func (c *Coordinator) Agent(id string) (AgentInfo, error) {
	rec, err := c.lookup(id)
	if err != nil {
		return AgentInfo{}, err
	}
	return rec.agent.Snapshot(), nil
}

// List returns the fleet in creation order, backing `/agents list`.
func (c *Coordinator) List() []AgentInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]AgentInfo, 0, len(c.order))
	for _, id := range c.order {
		if rec, ok := c.agents[id]; ok {
			out = append(out, rec.agent.Snapshot())
		}
	}
	return out
}

// Active returns the agents currently occupying a concurrency slot.
func (c *Coordinator) Active() []AgentInfo {
	var out []AgentInfo
	for _, info := range c.List() {
		if info.Status.Active() {
			out = append(out, info)
		}
	}
	return out
}

// Stop cancels one agent, backing `/agents stop <id>`. The id may be a unique
// prefix, because nobody wants to retype a UUID.
func (c *Coordinator) Stop(id string) error {
	rec, err := c.lookup(id)
	if err != nil {
		return err
	}
	c.stopRecord(rec)
	return nil
}

// StopAll cancels every live agent.
func (c *Coordinator) StopAll() {
	c.mu.Lock()
	recs := make([]*record, 0, len(c.agents))
	for _, rec := range c.agents {
		recs = append(recs, rec)
	}
	c.mu.Unlock()
	for _, rec := range recs {
		c.stopRecord(rec)
	}
}

func (c *Coordinator) stopRecord(rec *record) {
	if rec.agent.State().Terminal() {
		return
	}
	// Cancel first so the worker unwinds, then record the status: an agent
	// reported cancelled while its goroutine still runs would be a lie.
	c.mu.Lock()
	cancel := rec.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	_ = rec.agent.Cancel()
	if cancel == nil {
		// Nothing is running this agent, so nothing else will close it out.
		rec.finish()
	}
}

// Wait blocks until no agent is working, or ctx ends.
//
// Idle agents are ignored: an agent that was spawned but never started is not
// work in progress, and waiting on one would hang forever.
func (c *Coordinator) Wait(ctx context.Context) error {
	for {
		c.mu.Lock()
		var wait chan struct{}
		for _, id := range c.order {
			rec, ok := c.agents[id]
			if ok && rec.agent.State().Active() {
				wait = rec.done
				break
			}
		}
		c.mu.Unlock()
		if wait == nil {
			return nil
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Prune forgets finished agents and returns how many were dropped. It also
// frees their share of the per-objective budget.
func (c *Coordinator) Prune() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := c.order[:0]
	dropped := 0
	for _, id := range c.order {
		rec, ok := c.agents[id]
		if ok && rec.agent.State().Terminal() {
			delete(c.agents, id)
			dropped++
			continue
		}
		kept = append(kept, id)
	}
	c.order = kept
	return dropped
}

// Snapshot reports the fleet for `/agents`, the TUI header and the WebUI.
func (c *Coordinator) Snapshot() Snapshot {
	agents := c.List()
	c.mu.Lock()
	snap := Snapshot{
		Enabled:   c.enabled,
		MaxDepth:  c.maxDepth,
		MaxAgents: c.maxAgents,
		At:        c.now(),
	}
	c.mu.Unlock()
	snap.Max = c.sched.Max()
	snap.Agents = agents
	snap.Total = len(agents)
	for _, a := range agents {
		switch {
		case a.Status == StatusIdle:
			snap.Idle++
		case a.Status == StatusComplete:
			snap.Complete++
		case a.Status == StatusError:
			snap.Failed++
		case a.Status == StatusCancelled:
			snap.Cancelled++
		default:
			snap.Active++
		}
	}
	return snap
}

// lookup resolves a full id or a unique prefix.
func (c *Coordinator) lookup(id string) (*record, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, ErrNotFound
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if rec, ok := c.agents[id]; ok {
		return rec, nil
	}
	var found *record
	for _, candidate := range c.order {
		if !strings.HasPrefix(candidate, id) {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("%w: %q matches more than one agent", ErrAmbiguousID, id)
		}
		found = c.agents[candidate]
	}
	if found == nil {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	return found, nil
}

// runContext carries the ambient facts about the objective being executed.
//
// It travels in the context rather than in a Coordinator field because nested
// spawning has to attribute a child to the agent that asked for it, and a field
// could not tell two concurrent runs apart.
type runContext struct {
	parentID     string
	objective    string
	requirements []string
}

type runContextKey struct{}

func withRunContext(ctx context.Context, rc *runContext) context.Context {
	return context.WithValue(ctx, runContextKey{}, rc)
}

func runContextFrom(ctx context.Context) *runContext {
	if rc, ok := ctx.Value(runContextKey{}).(*runContext); ok && rc != nil {
		return rc
	}
	return &runContext{}
}

// PlanRequest is one objective's worth of work to schedule.
type PlanRequest struct {
	// Objective is the wider goal, shown to every worker.
	Objective string
	// Tasks is the plan. Required.
	Tasks []Task
	// ParentID attributes the spawned agents to an existing agent, which is
	// what makes the depth and budget caps apply to recursive planning.
	ParentID string
	// Requirements apply to every task in the plan.
	Requirements []string
	// Degraded and Reason are carried through to the report when the plan came
	// from a planner that had to fall back.
	Degraded bool
	Reason   string
}

// Run schedules a plan and aggregates the results.
//
// It returns a report in every case that produced one, including cancellation:
// the user is entitled to see what the fleet managed before it stopped.
func (c *Coordinator) Run(ctx context.Context, req PlanRequest) (*RunReport, error) {
	report := &RunReport{
		Objective: req.Objective,
		Degraded:  req.Degraded,
		Reason:    req.Reason,
		StartedAt: c.now(),
	}
	finish := func(err error) (*RunReport, error) {
		if err != nil && report.Error == "" {
			report.Error = err.Error()
		}
		report.tally()
		report.FinishedAt = c.now()
		report.Duration = report.FinishedAt.Sub(report.StartedAt)
		return report, err
	}

	if !c.Enabled() {
		return finish(ErrDisabled)
	}
	if c.runner == nil {
		return finish(ErrNoRunner)
	}
	if len(req.Tasks) == 0 {
		return finish(ErrNoTasks)
	}
	// Validate before spawning anything, so an unschedulable plan costs no
	// agents and reports a cycle rather than deadlocking (§11).
	if err := Validate(req.Tasks); err != nil {
		return finish(err)
	}

	rctx := withRunContext(ctx, &runContext{
		parentID:     req.ParentID,
		objective:    req.Objective,
		requirements: req.Requirements,
	})
	results, err := c.sched.Run(rctx, req.Tasks)
	report.Tasks = results
	report.Agents = c.agentsFor(results)
	return finish(err)
}

// RunObjective plans an objective and runs the resulting tasks.
func (c *Coordinator) RunObjective(ctx context.Context, objective string) (*RunReport, error) {
	plan := c.planner.Plan(ctx, objective)
	return c.Run(ctx, PlanRequest{
		Objective: plan.Objective,
		Tasks:     plan.Tasks,
		Degraded:  plan.Degraded,
		Reason:    plan.Reason,
	})
}

// Plan exposes decomposition on its own, so a frontend can show a plan before
// committing agents to it.
func (c *Coordinator) Plan(ctx context.Context, objective string) PlanResult {
	return c.planner.Plan(ctx, objective)
}

// agentsFor collects snapshots of the agents that ran a set of results.
func (c *Coordinator) agentsFor(results []TaskResult) []AgentInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []AgentInfo
	for _, res := range results {
		if res.AgentID == "" {
			continue
		}
		if rec, ok := c.agents[res.AgentID]; ok {
			out = append(out, rec.agent.Snapshot())
		}
	}
	return out
}

// executeTask is the scheduler's executor: one task, one agent, one isolated
// context, one loop.
func (c *Coordinator) executeTask(ctx context.Context, task Task) (TaskOutcome, error) {
	if c.runner == nil {
		return TaskOutcome{}, ErrNoRunner
	}
	rc := runContextFrom(ctx)

	actx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Spawning already working means the agent is never visible as idle while
	// a goroutine is driving it, which Wait relies on.
	a, err := c.spawn(SpawnSpec{
		Name:     taskName(task),
		Task:     task.Description,
		ParentID: rc.parentID,
		Provider: task.Provider,
		Model:    task.Model,
	}, StatusWorking, cancel)
	if err != nil {
		// Depth, budget and disabled all land here as ordinary task failures:
		// the scheduler records them, blocks the dependents and moves on. An
		// exhausted budget must never look like a hang (§11).
		return TaskOutcome{}, err
	}

	rec, lookupErr := c.lookup(a.ID)
	if lookupErr != nil {
		return TaskOutcome{}, lookupErr
	}
	defer rec.finish()

	// Anything this worker spawns is its child, which is how the depth cap
	// reaches recursive spawning.
	actx = withRunContext(actx, &runContext{
		parentID:     a.ID,
		objective:    rc.objective,
		requirements: rc.requirements,
	})

	brief := c.scope.Brief(BriefRequest{
		Task:         task,
		Objective:    rc.objective,
		Requirements: rc.requirements,
	})

	if c.bus != nil {
		c.bus.Publish(app.Event{
			Type:      app.EventTaskStarted,
			SessionID: c.sessionID,
			AgentID:   a.ID,
			Payload:   task,
			At:        c.now(),
		})
	}

	outcome, runErr := c.runner.RunTask(actx, a, brief)
	outcome.AgentID = a.ID

	if c.bus != nil {
		c.bus.Publish(app.Event{
			Type:      app.EventTaskCompleted,
			SessionID: c.sessionID,
			AgentID:   a.ID,
			Payload:   outcome,
			At:        c.now(),
		})
	}

	switch {
	case runErr == nil:
		_ = a.Complete(outcome.Output)
	case errors.Is(runErr, context.Canceled), errors.Is(runErr, context.DeadlineExceeded):
		_ = a.Cancel()
	default:
		_ = a.Fail(runErr)
	}
	return outcome, runErr
}

// taskName is the short label a worker agent is listed under.
func taskName(task Task) string {
	if id := strings.TrimSpace(task.ID); id != "" {
		return id
	}
	return firstLine(task.Description)
}
