package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kawaiipantsu/boop/internal/agent"
	"github.com/kawaiipantsu/boop/internal/app"
	"github.com/kawaiipantsu/boop/internal/config"
)

// fakeFleet builds a coordinator driven by runner instead of by a model.
//
// §41 forbids tests that need a provider, and an agent run is the one API path
// that would otherwise start one. Options.Agents exists for exactly this.
func fakeFleet(runner agent.TaskRunner) func(*app.App, string) *agent.Coordinator {
	return func(a *app.App, sessionID string) *agent.Coordinator {
		return agent.NewCoordinator(agent.CoordinatorOptions{
			Runner:    runner,
			Bus:       a.Bus,
			SessionID: sessionID,
			Provider:  a.Config().Provider,
			Model:     a.Config().Model,
			Max:       2,
		})
	}
}

// newAgentServer wires a server with a scripted fleet over a real store.
func newAgentServer(t *testing.T, runner agent.TaskRunner, mutate func(*config.Config)) (*Server, *app.App) {
	t.Helper()
	application := newTestApp(t)
	cfg := *application.Config()
	if mutate != nil {
		mutate(&cfg)
	}
	srv := newTestServer(t, func(o *Options) {
		o.App = application
		o.Config = &cfg
		o.Agents = fakeFleet(runner)
	})
	return srv, application
}

// blockingRunner reports each agent it starts and then waits to be released.
func blockingRunner(started chan<- string, release <-chan struct{}) agent.TaskRunnerFunc {
	return func(ctx context.Context, a *agent.Agent, _ agent.Brief) (agent.TaskOutcome, error) {
		select {
		case started <- a.ID:
		default:
		}
		select {
		case <-release:
			return agent.TaskOutcome{Output: "finished " + a.Name}, nil
		case <-ctx.Done():
			return agent.TaskOutcome{}, ctx.Err()
		}
	}
}

// getAgents fetches the agent listing.
func getAgents(t *testing.T, srv *Server) agentsResponse {
	t.Helper()
	rec, body := doJSON(t, srv, http.MethodGet, "/api/agents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/agents = %d (body %s)", rec.Code, body)
	}
	var resp agentsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

// TestAgentRunLifecycle is the whole contract in one pass: POST starts real
// work and returns immediately, GET reports it live, DELETE stops it, and the
// final state reaches the session store.
func TestAgentRunLifecycle(t *testing.T) {
	started := make(chan string, 4)
	release := make(chan struct{})
	defer close(release)

	srv, application := newAgentServer(t, blockingRunner(started, release), nil)

	created := make(chan string, 8)
	unsubscribe := application.Bus.Subscribe(func(ev app.Event) {
		select {
		case created <- ev.AgentID:
		default:
		}
	}, app.EventAgentCreated)
	t.Cleanup(unsubscribe)

	rec, body := doJSON(t, srv, http.MethodPost, "/api/agents",
		agentRequest{Name: "research", Task: "read the spec"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /api/agents = %d, want 202 (body %s)", rec.Code, body)
	}
	var accepted agentStartResponse
	if err := json.Unmarshal(body, &accepted); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !accepted.Accepted || accepted.RunID == "" || accepted.SessionID == "" {
		t.Fatalf("acknowledgement = %+v, want an accepted run", accepted)
	}
	if accepted.TimeoutMS == 0 {
		t.Error("the run was accepted with no time bound")
	}

	var agentID string
	select {
	case agentID = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("POST /api/agents never started any work")
	}
	select {
	case id := <-created:
		if id == "" {
			t.Error("agent.created carried no agent id")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no agent.created event reached the bus")
	}

	// The listing is live, not the persisted queue.
	list := getAgents(t, srv)
	if !list.Available || list.Unavailable != "" {
		t.Fatalf("available = %v, unavailable = %q, want a live fleet", list.Available, list.Unavailable)
	}
	if list.Snapshot == nil || list.Snapshot.Active != 1 {
		t.Fatalf("snapshot = %+v, want one active agent", list.Snapshot)
	}
	found := false
	for _, a := range list.Agents {
		if a.ID != agentID {
			continue
		}
		found = true
		if a.Source != "live" {
			t.Errorf("source = %q, want live", a.Source)
		}
		if a.Status != string(agent.StatusWorking) {
			t.Errorf("status = %q, want working", a.Status)
		}
		if a.Task != "read the spec" {
			t.Errorf("task = %q", a.Task)
		}
	}
	if !found {
		t.Fatalf("agent %s missing from %+v", agentID, list.Agents)
	}
	if len(list.Runs) != 1 || !list.Runs[0].Running {
		t.Fatalf("runs = %+v, want one running", list.Runs)
	}

	// A short id prefix is enough, as it is for `/agents stop`.
	rec, body = doJSON(t, srv, http.MethodDelete, "/api/agents/"+agentID[:8], nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /api/agents/{id} = %d (body %s)", rec.Code, body)
	}
	var stopped agentStopResponse
	if err := json.Unmarshal(body, &stopped); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(stopped.Stopped) != 1 || stopped.Stopped[0] != agentID {
		t.Fatalf("stopped = %v, want [%s]", stopped.Stopped, agentID)
	}

	waitFor(t, func() bool { // the run finishes
		runs := getAgents(t, srv).Runs
		return len(runs) == 1 && !runs[0].Running
	})

	final := getAgents(t, srv)
	if final.Runs[0].Report == nil {
		t.Fatal("a finished run carries no report")
	}
	for _, a := range final.Agents {
		if a.ID == agentID && a.Status != string(agent.StatusCancelled) {
			t.Errorf("status = %q, want cancelled", a.Status)
		}
	}

	// The terminal state is written back, so the session remembers what ran.
	waitFor(t, func() bool { // the agent record reaches the store
		records, err := application.Sessions.Agents(context.Background(), accepted.SessionID)
		if err != nil {
			t.Fatalf("list stored agents: %v", err)
		}
		for _, rec := range records {
			if rec.ID == agentID && rec.Status == string(agent.StatusCancelled) {
				return true
			}
		}
		return false
	})
}

// TestAgentsDisabledSaysSo: an empty list with no explanation reads as "no
// agents are running", which is not what "agents are switched off" means.
func TestAgentsDisabledSaysSo(t *testing.T) {
	srv, _ := newAgentServer(t, blockingRunner(nil, nil), func(c *config.Config) {
		c.Agents.Enabled = false
	})

	list := getAgents(t, srv)
	if list.Available {
		t.Error("a disabled runtime reported an available fleet")
	}
	if list.Enabled {
		t.Error("enabled = true with agents.enabled = false")
	}
	if list.Unavailable == "" {
		t.Fatal("the listing gave no reason for the empty fleet")
	}
	if !strings.Contains(list.Unavailable, "disabled") {
		t.Errorf("unavailable = %q, want it to say agents are disabled", list.Unavailable)
	}
	if list.Snapshot != nil {
		t.Error("a disabled runtime produced a fleet snapshot")
	}

	tests := []struct {
		name   string
		method string
		target string
		body   any
	}{
		{"start", http.MethodPost, "/api/agents", agentRequest{Task: "x"}},
		{"stop one", http.MethodPost, "/api/agents/stop", agentStopRequest{ID: "abc"}},
		{"stop all", http.MethodPost, "/api/agents/stop", agentStopRequest{All: true}},
		{"delete", http.MethodDelete, "/api/agents/abc", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec, body := doJSON(t, srv, tc.method, tc.target, tc.body)
			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409 (body %s)", rec.Code, body)
			}
			var env errorEnvelope
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !strings.Contains(env.Error.Message, "disabled") {
				t.Errorf("message = %q, want it to name the disabled switch", env.Error.Message)
			}
		})
	}
}

// TestAgentRequestValidation covers the rejections that never reach the fleet.
func TestAgentRequestValidation(t *testing.T) {
	tests := []struct {
		name       string
		req        agentRequest
		wantStatus int
		wantIn     string
	}{
		{
			name:       "a task is required",
			req:        agentRequest{Name: "empty"},
			wantStatus: http.StatusBadRequest,
			wantIn:     "task",
		},
		{
			name:       "a foreign provider is refused rather than ignored",
			req:        agentRequest{Task: "x", Provider: "somewhere-else"},
			wantStatus: http.StatusBadRequest,
			wantIn:     "provider",
		},
		{
			name:       "a foreign model is refused rather than ignored",
			req:        agentRequest{Task: "x", Model: "some-other-model"},
			wantStatus: http.StatusBadRequest,
			wantIn:     "model",
		},
		{
			name:       "a negative timeout is refused",
			req:        agentRequest{Task: "x", TimeoutSeconds: -1},
			wantStatus: http.StatusBadRequest,
			wantIn:     "timeout_seconds",
		},
		{
			name:       "an absurd timeout is refused",
			req:        agentRequest{Task: "x", TimeoutSeconds: 1 << 30},
			wantStatus: http.StatusBadRequest,
			wantIn:     "timeout_seconds",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			defer close(release)
			srv, _ := newAgentServer(t, blockingRunner(nil, release), nil)

			rec, body := doJSON(t, srv, http.MethodPost, "/api/agents", tc.req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, body)
			}
			var env errorEnvelope
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !strings.Contains(env.Error.Message, tc.wantIn) {
				t.Errorf("message = %q, want it to mention %q", env.Error.Message, tc.wantIn)
			}
		})
	}
}

// TestAgentStopErrors covers the stop endpoint's refusals.
func TestAgentStopErrors(t *testing.T) {
	tests := []struct {
		name       string
		req        agentStopRequest
		wantStatus int
	}{
		{name: "no target", req: agentStopRequest{}, wantStatus: http.StatusBadRequest},
		{name: "two targets", req: agentStopRequest{ID: "a", All: true}, wantStatus: http.StatusBadRequest},
		{name: "unknown agent", req: agentStopRequest{ID: "nope"}, wantStatus: http.StatusNotFound},
		{name: "unknown run", req: agentStopRequest{RunID: "nope"}, wantStatus: http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			started := make(chan string, 1)
			release := make(chan struct{})
			defer close(release)
			srv, _ := newAgentServer(t, blockingRunner(started, release), nil)

			// A session and a live run, so the failures are about the target
			// and not about there being no fleet at all.
			rec, body := doJSON(t, srv, http.MethodPost, "/api/agents", agentRequest{Task: "work"})
			if rec.Code != http.StatusAccepted {
				t.Fatalf("start: %d %s", rec.Code, body)
			}
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("the run never started")
			}

			rec, body = doJSON(t, srv, http.MethodPost, "/api/agents/stop", tc.req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, body)
			}
		})
	}
}

// TestAgentStopAll stops the run as well as the workers: a run still deciding
// what to do has no agent to cancel, and would otherwise carry on.
func TestAgentStopAll(t *testing.T) {
	started := make(chan string, 4)
	release := make(chan struct{})
	defer close(release)
	srv, _ := newAgentServer(t, blockingRunner(started, release), nil)

	rec, body := doJSON(t, srv, http.MethodPost, "/api/agents", agentRequest{Task: "work"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", rec.Code, body)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the run never started")
	}

	rec, body = doJSON(t, srv, http.MethodPost, "/api/agents/stop", agentStopRequest{All: true})
	if rec.Code != http.StatusOK {
		t.Fatalf("stop all = %d (body %s)", rec.Code, body)
	}
	var stopped agentStopResponse
	if err := json.Unmarshal(body, &stopped); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(stopped.Stopped) == 0 {
		t.Error("stop all cancelled no agents")
	}
	if len(stopped.RunsCancelled) != 1 {
		t.Errorf("runs_cancelled = %v, want one", stopped.RunsCancelled)
	}
	waitFor(t, func() bool { // the run finishes
		runs := getAgents(t, srv).Runs
		return len(runs) == 1 && !runs[0].Running
	})
}

// TestAgentRunIsTimeBounded: an objective that never finishes must not hold
// the fleet forever.
func TestAgentRunIsTimeBounded(t *testing.T) {
	started := make(chan string, 1)
	never := make(chan struct{})
	defer close(never)
	srv, _ := newAgentServer(t, blockingRunner(started, never), nil)

	rec, body := doJSON(t, srv, http.MethodPost, "/api/agents",
		agentRequest{Task: "run forever", TimeoutSeconds: 1})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", rec.Code, body)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the run never started")
	}

	waitFor(t, func() bool { // the run hits its deadline
		runs := getAgents(t, srv).Runs
		return len(runs) == 1 && !runs[0].Running
	})
	runs := getAgents(t, srv).Runs
	if runs[0].Error == "" {
		t.Error("a run stopped by its deadline reported no error")
	}
}

// TestShutdownStopsTheFleet: §58 requires workers to be told to stop, not
// abandoned mid-command.
func TestShutdownStopsTheFleet(t *testing.T) {
	started := make(chan string, 1)
	cancelled := make(chan struct{})
	runner := agent.TaskRunnerFunc(func(ctx context.Context, a *agent.Agent, _ agent.Brief) (agent.TaskOutcome, error) {
		started <- a.ID
		<-ctx.Done()
		close(cancelled)
		return agent.TaskOutcome{}, ctx.Err()
	})
	srv, _ := newAgentServer(t, runner, nil)

	rec, body := doJSON(t, srv, http.MethodPost, "/api/agents", agentRequest{Task: "work"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", rec.Code, body)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the run never started")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown left a worker running")
	}
}
