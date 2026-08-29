package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kawaiipantsu/boop/internal/agent"
	"github.com/kawaiipantsu/boop/internal/app"
	"github.com/kawaiipantsu/boop/internal/session"
)

// Bounds on agent work started through the API (§10, §11).
//
// An objective is not a request: it can occupy the fleet for a long time, and
// nothing about HTTP bounds it. Every run therefore carries a deadline of its
// own, so a planner that never answers or a worker wedged on a command cannot
// hold agents, tool slots and model quota for the lifetime of the process.
const (
	// defaultAgentRunTimeout bounds one objective when the caller does not
	// choose. It is generous because real work is slow, and finite because
	// "forever" is not a policy.
	defaultAgentRunTimeout = 30 * time.Minute
	// maxAgentRunTimeout is the ceiling a caller may ask for.
	maxAgentRunTimeout = 4 * time.Hour
	// agentStopGrace bounds how long Shutdown waits for a cancelled fleet to
	// unwind before giving up on it (§58).
	agentStopGrace = 3 * time.Second
	// retainedRuns bounds how many finished runs are remembered for
	// GET /api/agents, so a long session cannot grow the list without limit.
	retainedRuns = 20
	// persistAgentsTimeout bounds the write-back of final agent state. It runs
	// after the server context is already cancelled during shutdown, so it
	// gets a deadline rather than inheriting one.
	persistAgentsTimeout = 5 * time.Second
)

// agentRun is one objective handed to the fleet.
//
// It exists because the coordinator tracks agents, not the request that asked
// for them: without a run the API could report "three agents failed" but never
// "the objective you submitted finished, and here is its report".
type agentRun struct {
	id        string
	sessionID string
	objective string
	startedAt time.Time

	finishedAt time.Time
	report     *agent.RunReport
	err        string
	done       bool

	cancel context.CancelFunc
}

// agentRunView is the JSON shape of a run.
type agentRunView struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Objective string `json:"objective"`
	Running   bool   `json:"running"`
	// StartedAt and FinishedAt bound the run; FinishedAt is absent while it
	// is still going.
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	// Error is the run-level failure, such as a cancellation or an
	// unschedulable plan. Per-task failures live in Report.
	Error string `json:"error,omitempty"`
	// Report is the aggregated outcome, present once the run has finished.
	Report *agent.RunReport `json:"report,omitempty"`
}

func (r *agentRun) view() agentRunView {
	return agentRunView{
		ID:         r.id,
		SessionID:  r.sessionID,
		Objective:  r.objective,
		Running:    !r.done,
		StartedAt:  r.startedAt,
		FinishedAt: r.finishedAt,
		Error:      r.err,
		Report:     r.report,
	}
}

// agentView is one agent as the WebUI renders it (§26).
//
// Live agents and persisted records are projected onto the same shape on
// purpose: the Agents tab should not have to know whether a row came from the
// running fleet or from the session store, and a row that changes shape when a
// run ends is a frontend bug waiting to happen.
type agentView struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Task     string `json:"task,omitempty"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Status   string `json:"status"`
	ParentID string `json:"parent_id,omitempty"`
	RootID   string `json:"root_id,omitempty"`
	Depth    int    `json:"depth,omitempty"`
	// Source is "live" for an agent the coordinator is tracking and "stored"
	// for one recovered from the session store.
	Source     string    `json:"source"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	DurationMS int64     `json:"duration_ms,omitempty"`
	Output     string    `json:"output,omitempty"`
	Error      string    `json:"error,omitempty"`
}

func agentViewFromInfo(info agent.AgentInfo) agentView {
	return agentView{
		ID:         info.ID,
		Name:       info.Name,
		Task:       info.Task,
		Provider:   info.Provider,
		Model:      info.Model,
		Status:     string(info.Status),
		ParentID:   info.ParentID,
		RootID:     info.RootID,
		Depth:      info.Depth,
		Source:     "live",
		StartedAt:  info.StartedAt,
		FinishedAt: info.FinishedAt,
		DurationMS: info.Duration.Milliseconds(),
		Output:     info.Output,
		Error:      info.Error,
	}
}

func agentViewFromRecord(rec session.AgentRecord) agentView {
	v := agentView{
		ID:       rec.ID,
		Name:     rec.Name,
		Task:     rec.Task,
		Provider: rec.Provider,
		Model:    rec.Model,
		Status:   rec.Status,
		ParentID: rec.ParentID,
		Source:   "stored",
		Error:    rec.Error,
	}
	if rec.StartedAt != nil {
		v.StartedAt = *rec.StartedAt
	} else {
		v.StartedAt = rec.CreatedAt
	}
	if rec.FinishedAt != nil {
		v.FinishedAt = *rec.FinishedAt
		v.DurationMS = v.FinishedAt.Sub(v.StartedAt).Milliseconds()
	}
	return v
}

// agentsResponse is the GET /api/agents document.
type agentsResponse struct {
	SessionID string `json:"session_id"`
	// Enabled mirrors `/agents on|off` for the live fleet, falling back to the
	// configuration when there is no fleet.
	Enabled bool `json:"enabled"`
	// Available reports that a coordinator exists and can accept work.
	Available bool `json:"available"`
	// Unavailable explains a missing coordinator in words the UI can show.
	// An empty agent list with no explanation reads as "nothing is running",
	// which is a different and much more misleading statement than "agents
	// are switched off".
	Unavailable string `json:"unavailable,omitempty"`
	Max         int    `json:"max"`
	// Snapshot is the live fleet state, absent when there is no coordinator.
	Snapshot *agent.Snapshot `json:"snapshot,omitempty"`
	Agents   []agentView     `json:"agents"`
	Runs     []agentRunView  `json:"runs"`
}

// agentRequest asks the fleet to work on an objective.
type agentRequest struct {
	SessionID string `json:"session_id,omitempty"`
	// Name labels the first task; the coordinator names spawned workers after
	// the task they run.
	Name string `json:"name,omitempty"`
	// Task is the objective. Required.
	Task string `json:"task"`
	// Provider and Model must match the session's, if given: the coordinator
	// pins its workers to one provider/model pair when it is built, and
	// silently ignoring an override would be worse than refusing it.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	// ParentID attributes the work to an existing agent, which is what makes
	// the depth and per-objective budget caps apply to nested requests (§11).
	ParentID string `json:"parent_id,omitempty"`
	// Plan decomposes the objective into a dependency graph before running
	// it. Absent means plan; false runs it as a single agent.
	Plan *bool `json:"plan,omitempty"`
	// TimeoutSeconds bounds the run. Zero uses defaultAgentRunTimeout.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// agentStartResponse acknowledges accepted work.
type agentStartResponse struct {
	Accepted  bool   `json:"accepted"`
	RunID     string `json:"run_id"`
	SessionID string `json:"session_id"`
	Objective string `json:"objective"`
	// Planned reports that the objective will be decomposed before running.
	Planned   bool  `json:"planned"`
	TimeoutMS int64 `json:"timeout_ms"`
}

// agentStopRequest stops agents. Exactly one of ID, RunID and All is required.
type agentStopRequest struct {
	SessionID string `json:"session_id,omitempty"`
	// ID is an agent id or a unique prefix of one.
	ID string `json:"id,omitempty"`
	// RunID cancels an objective, including work that has not spawned an
	// agent yet — a run still in the planning phase has nothing to stop
	// otherwise.
	RunID string `json:"run_id,omitempty"`
	// All stops every agent and every run for the session.
	All bool `json:"all,omitempty"`
}

// agentStopResponse reports what was actually stopped.
type agentStopResponse struct {
	SessionID string `json:"session_id"`
	// Stopped lists the agent ids that were cancelled.
	Stopped []string `json:"stopped"`
	// RunsCancelled lists the run ids that were cancelled.
	RunsCancelled []string `json:"runs_cancelled"`
}

// ---------------------------------------------------------------------------
// Coordinator lifecycle
// ---------------------------------------------------------------------------

// fleet returns the coordinator for a session, building it on first use.
//
// Coordinators are per session because agent.NewFromApp binds the session id
// at construction: it labels every event a worker publishes, and the WebUI
// filters on it. A shared coordinator would have to lie about which
// conversation a worker belongs to.
//
// The second result is a human explanation of a nil coordinator, never an
// error: "agents are off" is a state the UI renders, not a failure.
func (s *Server) fleet(sessionID string) (*agent.Coordinator, string) {
	if s.app == nil {
		return nil, "this server was started without a Boop runtime attached"
	}
	if !s.cfg.Agents.Enabled {
		return nil, "agents are disabled; set agents.enabled in the configuration or run /agents on"
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, "no session is selected; POST /api/session first or pass session_id"
	}

	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	if s.fleetClosed {
		return nil, "the server is shutting down"
	}
	if coord, built := s.fleets[sessionID]; built {
		if coord == nil {
			return nil, "the agent runtime reported itself disabled"
		}
		return coord, ""
	}
	coord := s.newFleet(s.app, sessionID)
	s.fleets[sessionID] = coord
	if coord == nil {
		return nil, "the agent runtime reported itself disabled"
	}
	// The server's own configuration wins over the runtime's, because
	// Options.Config may deliberately differ from App.Config.
	if s.cfg.Agents.Max > 0 {
		if err := coord.SetMax(s.cfg.Agents.Max); err != nil {
			s.log.Printf("web: cannot apply agents.max=%d: %v", s.cfg.Agents.Max, err)
		}
	}
	return coord, ""
}

// stopFleets cancels every run and every agent, then waits briefly for the
// workers to unwind.
//
// It runs before the WebSocket hub closes, so the cancelled status of each
// agent still reaches connected clients: a browser that sees agents frozen in
// "working" after a shutdown has been told something untrue (§58).
func (s *Server) stopFleets(ctx context.Context) {
	s.agentMu.Lock()
	s.fleetClosed = true
	fleets := make([]*agent.Coordinator, 0, len(s.fleets))
	for _, coord := range s.fleets {
		if coord != nil {
			fleets = append(fleets, coord)
		}
	}
	cancels := make([]context.CancelFunc, 0, len(s.agentRuns))
	for _, run := range s.agentRuns {
		if !run.done && run.cancel != nil {
			cancels = append(cancels, run.cancel)
		}
	}
	s.agentMu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
	for _, coord := range fleets {
		coord.StopAll()
	}
	if len(fleets) == 0 {
		return
	}
	waitCtx, cancel := context.WithTimeout(ctx, agentStopGrace)
	defer cancel()
	for _, coord := range fleets {
		_ = coord.Wait(waitCtx)
	}
}

// ---------------------------------------------------------------------------
// GET|POST /api/agents
// ---------------------------------------------------------------------------

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listAgents(w, r)
	case http.MethodPost:
		s.createAgent(w, r)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

// handleAgentByID serves everything under /api/agents/.
//
// Two spellings of "stop" exist deliberately: DELETE /api/agents/{id} is the
// obvious REST shape, and POST /api/agents/stop is what a form or a client
// that cannot send DELETE can reach, and is the only one that can express
// "stop this run" or "stop everything".
func (s *Server) handleAgentByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/agents/"), "/")
	switch {
	case rest == "stop":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		var req agentStopRequest
		if r.ContentLength != 0 && !decodeBody(w, r, &req) {
			return
		}
		s.stopAgents(w, r, req)
	case rest == "" || strings.Contains(rest, "/"):
		writeError(w, http.StatusNotFound, codeNotFound, "no such API endpoint: "+r.URL.Path)
	default:
		if r.Method != http.MethodDelete {
			methodNotAllowed(w, http.MethodDelete)
			return
		}
		s.stopAgents(w, r, agentStopRequest{
			ID:        rest,
			SessionID: strings.TrimSpace(r.URL.Query().Get("session_id")),
		})
	}
}

// listAgents reports the fleet, live where possible and from the store where
// not (§26).
func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	sessionID := strings.TrimSpace(r.URL.Query().Get("session_id"))
	if sessionID == "" {
		sessionID = s.CurrentSession()
	}

	coord, reason := s.fleet(sessionID)
	resp := agentsResponse{
		SessionID:   sessionID,
		Enabled:     s.cfg.Agents.Enabled,
		Available:   coord != nil,
		Unavailable: reason,
		Max:         s.cfg.Agents.Max,
		Agents:      []agentView{},
		Runs:        s.runViews(sessionID),
	}

	seen := make(map[string]struct{})
	if coord != nil {
		snap := coord.Snapshot()
		resp.Snapshot = &snap
		resp.Enabled = snap.Enabled
		resp.Max = snap.Max
		for _, info := range snap.Agents {
			seen[info.ID] = struct{}{}
			resp.Agents = append(resp.Agents, agentViewFromInfo(info))
		}
	}

	// Persisted records fill in the history the coordinator has already
	// forgotten, and are the only source at all when agents are switched off.
	if sessionID != "" {
		stored, err := s.app.Sessions.Agents(r.Context(), sessionID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "cannot list agents: "+err.Error())
			return
		}
		for _, rec := range stored {
			if _, live := seen[rec.ID]; live {
				continue
			}
			resp.Agents = append(resp.Agents, agentViewFromRecord(rec))
		}
	}
	sort.SliceStable(resp.Agents, func(i, j int) bool {
		if !resp.Agents[i].StartedAt.Equal(resp.Agents[j].StartedAt) {
			return resp.Agents[i].StartedAt.Before(resp.Agents[j].StartedAt)
		}
		return resp.Agents[i].ID < resp.Agents[j].ID
	})
	writeJSON(w, http.StatusOK, resp)
}

// createAgent starts real work and returns as soon as it has started.
//
// An objective can run for many minutes; holding the connection open for it
// would be the same mistake POST /api/message avoids with `async`. Progress
// arrives as agent.created and agent.status.changed events on the WebSocket
// (§25), and the aggregated report is readable from GET /api/agents once the
// run finishes.
func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	var req agentRequest
	if !decodeBody(w, r, &req) {
		return
	}
	objective := strings.TrimSpace(req.Task)
	if objective == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "an agent needs a `task`")
		return
	}
	if !s.cfg.Agents.Enabled {
		writeError(w, http.StatusConflict, codeConflict,
			"agents are disabled; set agents.enabled in the configuration")
		return
	}
	if err := checkAgentSelection(req, s.cfg.Provider, s.cfg.Model); err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	timeout, err := agentRunTimeout(req.TimeoutSeconds)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}

	sessionID, err := s.resolveSession(r.Context(), req.SessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	coord, reason := s.fleet(sessionID)
	if coord == nil {
		writeError(w, http.StatusConflict, codeConflict, reason)
		return
	}
	if !coord.Enabled() {
		writeError(w, http.StatusConflict, codeConflict, "the agent fleet is switched off (/agents off)")
		return
	}

	plan := req.Plan == nil || *req.Plan
	run := s.startAgentRun(coord, agentRunSpec{
		sessionID: sessionID,
		objective: objective,
		taskName:  strings.TrimSpace(req.Name),
		parentID:  strings.TrimSpace(req.ParentID),
		plan:      plan,
		timeout:   timeout,
	})
	writeJSON(w, http.StatusAccepted, agentStartResponse{
		Accepted:  true,
		RunID:     run.id,
		SessionID: sessionID,
		Objective: objective,
		Planned:   plan,
		TimeoutMS: timeout.Milliseconds(),
	})
}

// checkAgentSelection refuses a per-agent provider or model the coordinator
// cannot honour. See agentRequest for why this is refused rather than ignored.
func checkAgentSelection(req agentRequest, wantProvider, wantModel string) error {
	if p := strings.TrimSpace(req.Provider); p != "" && p != wantProvider {
		return fmt.Errorf("agents inherit the session's provider (%s); "+
			"change the provider first rather than passing one per agent", wantProvider)
	}
	if m := strings.TrimSpace(req.Model); m != "" && m != wantModel {
		return fmt.Errorf("agents inherit the session's model (%s); "+
			"change the model first rather than passing one per agent", wantModel)
	}
	return nil
}

// agentRunTimeout validates the requested bound.
func agentRunTimeout(seconds int) (time.Duration, error) {
	switch {
	case seconds == 0:
		return defaultAgentRunTimeout, nil
	case seconds < 0:
		return 0, errors.New("timeout_seconds must be positive")
	}
	d := time.Duration(seconds) * time.Second
	if d > maxAgentRunTimeout {
		return 0, fmt.Errorf("timeout_seconds must be at most %d", int(maxAgentRunTimeout/time.Second))
	}
	return d, nil
}

// newRunID mints an identifier for one objective run. Runs are server-local
// bookkeeping, not stored objects, so a UUID is all they need.
func newRunID() string { return uuid.NewString() }

// agentRunSpec is the internal description of work to start.
type agentRunSpec struct {
	sessionID string
	objective string
	taskName  string
	parentID  string
	plan      bool
	timeout   time.Duration
}

// startAgentRun schedules an objective in the background.
//
// The run hangs off the server context, not the request context, for the same
// reason an async turn does: the client is entitled to hang up, and the work
// should stop when the server stops rather than when the browser tab closes.
func (s *Server) startAgentRun(coord *agent.Coordinator, spec agentRunSpec) *agentRun {
	ctx, cancel := context.WithTimeout(s.baseCtx, spec.timeout)
	run := &agentRun{
		id:        newRunID(),
		sessionID: spec.sessionID,
		objective: spec.objective,
		startedAt: s.now().UTC(),
		cancel:    cancel,
	}

	s.agentMu.Lock()
	s.agentRuns[run.id] = run
	s.agentMu.Unlock()

	go func() {
		defer cancel()
		report, err := s.execAgentRun(ctx, coord, spec)
		s.finishAgentRun(run, report, err)
		s.persistAgents(spec.sessionID, report)
		s.publishAgentRunError(spec.sessionID, run.id, spec.objective, err)
	}()
	return run
}

// execAgentRun plans (when asked) and runs the objective.
//
// Planning is inside the goroutine because it is itself a model call: doing it
// in the handler would put an unbounded round trip in front of the response
// the caller is waiting for.
func (s *Server) execAgentRun(ctx context.Context, coord *agent.Coordinator, spec agentRunSpec) (*agent.RunReport, error) {
	if spec.plan {
		result := coord.Plan(ctx, spec.objective)
		return coord.Run(ctx, agent.PlanRequest{
			Objective: result.Objective,
			Tasks:     result.Tasks,
			ParentID:  spec.parentID,
			Degraded:  result.Degraded,
			Reason:    result.Reason,
		})
	}
	id := spec.taskName
	if id == "" {
		id = "task-1"
	}
	return coord.Run(ctx, agent.PlanRequest{
		Objective: spec.objective,
		Tasks:     []agent.Task{{ID: id, Description: spec.objective}},
		ParentID:  spec.parentID,
	})
}

// finishAgentRun records the outcome and trims the remembered history.
func (s *Server) finishAgentRun(run *agentRun, report *agent.RunReport, err error) {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	run.done = true
	run.finishedAt = s.now().UTC()
	run.report = report
	if err != nil {
		run.err = err.Error()
	}
	s.trimRunsLocked()
}

// trimRunsLocked drops the oldest finished runs beyond retainedRuns.
func (s *Server) trimRunsLocked() {
	finished := make([]*agentRun, 0, len(s.agentRuns))
	for _, run := range s.agentRuns {
		if run.done {
			finished = append(finished, run)
		}
	}
	if len(finished) <= retainedRuns {
		return
	}
	sort.Slice(finished, func(i, j int) bool { return finished[i].finishedAt.Before(finished[j].finishedAt) })
	for _, run := range finished[:len(finished)-retainedRuns] {
		delete(s.agentRuns, run.id)
	}
}

// runViews returns the runs for a session, newest last.
func (s *Server) runViews(sessionID string) []agentRunView {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	out := make([]agentRunView, 0, len(s.agentRuns))
	for _, run := range s.agentRuns {
		if sessionID != "" && run.sessionID != sessionID {
			continue
		}
		out = append(out, run.view())
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.Before(out[j].StartedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// publishAgentRunError reports a background failure on the bus, because the
// caller of an accepted run has no response left to read it from (§25).
//
// A deliberate cancellation is not an error; a timeout is, and says so.
func (s *Server) publishAgentRunError(sessionID, runID, objective string, err error) {
	if err == nil || errors.Is(err, context.Canceled) || s.app == nil || s.app.Bus == nil {
		return
	}
	message := err.Error()
	if errors.Is(err, context.DeadlineExceeded) {
		message = "the agent run exceeded its time limit and was stopped: " + message
	}
	s.app.Bus.Publish(app.Event{
		Type:      app.EventError,
		SessionID: sessionID,
		Payload: map[string]string{
			"error":     message,
			"run_id":    runID,
			"objective": objective,
		},
	})
}

// persistAgents writes the fleet's final state to the session store (§46).
//
// It happens once, when the run ends, rather than on every status change: a
// worker changes status many times a minute, and the live state is already on
// the event stream. What the store is for is the answer to "what did the fleet
// do in this session", which only the terminal state can give.
func (s *Server) persistAgents(sessionID string, report *agent.RunReport) {
	if s.app == nil || report == nil || len(report.Agents) == 0 {
		return
	}
	// A cancelled run means the server context is probably already dead, so
	// the write gets an independent deadline rather than inheriting one.
	ctx, cancel := context.WithTimeout(context.Background(), persistAgentsTimeout)
	defer cancel()

	now := s.now().UTC()
	for _, info := range report.Agents {
		rec := &session.AgentRecord{
			ID:        info.ID,
			SessionID: sessionID,
			ParentID:  info.ParentID,
			Name:      info.Name,
			Task:      info.Task,
			Provider:  info.Provider,
			Model:     info.Model,
			Status:    string(info.Status),
			Error:     info.Error,
			CreatedAt: info.StartedAt,
			UpdatedAt: now,
		}
		if !info.StartedAt.IsZero() {
			started := info.StartedAt
			rec.StartedAt = &started
		}
		if !info.FinishedAt.IsZero() {
			finished := info.FinishedAt
			rec.FinishedAt = &finished
		}
		if err := s.app.Sessions.SaveAgent(ctx, rec); err != nil {
			s.log.Printf("web: cannot record agent %s: %v", info.ID, err)
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Stopping
// ---------------------------------------------------------------------------

// stopAgents implements DELETE /api/agents/{id} and POST /api/agents/stop.
func (s *Server) stopAgents(w http.ResponseWriter, r *http.Request, req agentStopRequest) {
	if !s.requireApp(w) {
		return
	}
	targets := 0
	for _, set := range []bool{strings.TrimSpace(req.ID) != "", strings.TrimSpace(req.RunID) != "", req.All} {
		if set {
			targets++
		}
	}
	if targets != 1 {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"send exactly one of `id`, `run_id` or `all`")
		return
	}

	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		sessionID = s.CurrentSession()
	}
	coord, reason := s.fleet(sessionID)
	if coord == nil {
		// Nothing can be running without a coordinator, so the honest answer
		// is why there is none, not an empty success.
		writeError(w, http.StatusConflict, codeConflict, reason)
		return
	}

	resp := agentStopResponse{SessionID: sessionID, Stopped: []string{}, RunsCancelled: []string{}}
	switch {
	case strings.TrimSpace(req.RunID) != "":
		id := strings.TrimSpace(req.RunID)
		if !s.cancelRun(id, sessionID) {
			writeError(w, http.StatusNotFound, codeNotFound,
				"no run "+id+" is active for this session")
			return
		}
		resp.RunsCancelled = append(resp.RunsCancelled, id)

	case req.All:
		resp.RunsCancelled = s.cancelSessionRuns(sessionID)
		for _, info := range coord.List() {
			if !info.Status.Terminal() {
				resp.Stopped = append(resp.Stopped, info.ID)
			}
		}
		coord.StopAll()

	default:
		id := strings.TrimSpace(req.ID)
		info, err := coord.Agent(id)
		if err != nil {
			writeError(w, statusForAgentError(err), codeForAgentError(err), err.Error())
			return
		}
		if err := coord.Stop(id); err != nil {
			writeError(w, statusForAgentError(err), codeForAgentError(err), err.Error())
			return
		}
		resp.Stopped = append(resp.Stopped, info.ID)
	}
	writeJSON(w, http.StatusOK, resp)
}

// cancelRun cancels one run belonging to a session.
func (s *Server) cancelRun(runID, sessionID string) bool {
	s.agentMu.Lock()
	run, ok := s.agentRuns[runID]
	if !ok || run.done || run.sessionID != sessionID {
		s.agentMu.Unlock()
		return false
	}
	cancel := run.cancel
	s.agentMu.Unlock()
	if cancel != nil {
		cancel()
	}
	return true
}

// cancelSessionRuns cancels every live run for a session.
func (s *Server) cancelSessionRuns(sessionID string) []string {
	s.agentMu.Lock()
	var (
		ids     []string
		cancels []context.CancelFunc
	)
	for id, run := range s.agentRuns {
		if run.done || run.sessionID != sessionID {
			continue
		}
		ids = append(ids, id)
		if run.cancel != nil {
			cancels = append(cancels, run.cancel)
		}
	}
	s.agentMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	sort.Strings(ids)
	if ids == nil {
		return []string{}
	}
	return ids
}

func statusForAgentError(err error) int {
	switch {
	case errors.Is(err, agent.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, agent.ErrAmbiguousID), errors.Is(err, agent.ErrDisabled):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func codeForAgentError(err error) string {
	switch {
	case errors.Is(err, agent.ErrNotFound):
		return codeNotFound
	case errors.Is(err, agent.ErrAmbiguousID), errors.Is(err, agent.ErrDisabled):
		return codeConflict
	default:
		return codeInternal
	}
}
