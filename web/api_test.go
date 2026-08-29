package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kawaiipantsu/boop/internal/app"
	"github.com/kawaiipantsu/boop/internal/config"
	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/session"
)

// doJSON performs a request against the handler and decodes the response.
func doJSON(t *testing.T, srv *Server, method, target string, body any) (*httptest.ResponseRecorder, []byte) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode request: %v", err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec, rec.Body.Bytes()
}

// TestConfigNeverDisclosesSecrets is the §45 test that must not regress: the
// WebUI learns the *name* of every credential variable and nothing else.
func TestConfigNeverDisclosesSecrets(t *testing.T) {
	const (
		apiKeyValue  = "sk-live-THIS-MUST-NEVER-APPEAR"
		headerSecret = "Bearer HEADER-SECRET-MUST-NOT-LEAK"
		webToken     = "WEB-TOKEN-MUST-NOT-LEAK"
	)
	env := map[string]string{
		"OPENAI_API_KEY": apiKeyValue,
		"BOOP_WEB_TOKEN": webToken,
	}

	cfg := config.Default()
	cfg.Web.Auth = config.AuthConfig{Enabled: true, TokenEnv: "BOOP_WEB_TOKEN"}
	pc := cfg.Providers["openai"]
	pc.APIKeyEnv = "OPENAI_API_KEY"
	pc.Headers = map[string]string{"Authorization": headerSecret}
	cfg.Providers["openai"] = pc

	srv := newTestServer(t, func(o *Options) {
		o.Config = cfg
		o.LookupEnv = envOf(env)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.Header.Set("Authorization", "Bearer "+webToken)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	raw := rec.Body.String()
	for _, secret := range []string{apiKeyValue, headerSecret, webToken, "HEADER-SECRET", "sk-live"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("GET /api/config leaked %q in:\n%s", secret, raw)
		}
	}

	var resp configResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := resp.Config.Providers["openai"].APIKeyEnv; got != "OPENAI_API_KEY" {
		t.Errorf("api_key_env = %q, want the variable name OPENAI_API_KEY", got)
	}
	if got := resp.Config.Providers["openai"].Headers["Authorization"]; got != redactedValue {
		t.Errorf("header value = %q, want %q", got, redactedValue)
	}

	want := map[string]bool{"providers.openai": true, "web.auth": true}
	for _, ref := range resp.Secrets {
		if !want[ref.Scope] {
			continue
		}
		delete(want, ref.Scope)
		if !ref.Set {
			t.Errorf("secret %s reported as unset, but the variable has a value", ref.Scope)
		}
		if ref.Env == "" {
			t.Errorf("secret %s has no environment variable name", ref.Scope)
		}
	}
	if len(want) != 0 {
		t.Errorf("missing secret references for %v", want)
	}
}

// TestConfigSecretRefReportsUnsetVariable: the settings UI needs to tell the
// difference between "configured" and "configured but the shell is missing it".
func TestConfigSecretRefReportsUnsetVariable(t *testing.T) {
	cfg := config.Default()
	pc := cfg.Providers["openai"]
	pc.APIKeyEnv = "OPENAI_API_KEY"
	cfg.Providers["openai"] = pc

	srv := newTestServer(t, func(o *Options) {
		o.Config = cfg
		o.LookupEnv = envOf(nil)
	})
	_, body := doJSON(t, srv, http.MethodGet, "/api/config", nil)

	var resp configResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, ref := range resp.Secrets {
		if ref.Scope == "providers.openai" && ref.Set {
			t.Error("an unset environment variable was reported as set")
		}
	}
}

// TestPutConfig covers validation, persistence and the redaction round trip.
func TestPutConfig(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*config.Config)
		persist    *bool
		wantStatus int
		wantSaved  bool
		wantDetail string
	}{
		{
			name:       "a valid change is accepted and saved",
			mutate:     func(c *config.Config) { c.Execution.MaxToolIterations = 12 },
			wantStatus: http.StatusOK,
			wantSaved:  true,
		},
		{
			name:       "validate without persisting",
			mutate:     func(c *config.Config) { c.Execution.MaxToolIterations = 12 },
			persist:    boolPtr(false),
			wantStatus: http.StatusOK,
		},
		{
			name:       "an out-of-range port is rejected",
			mutate:     func(c *config.Config) { c.Web.Port = 99999 },
			wantStatus: http.StatusBadRequest,
			wantDetail: "web.port",
		},
		{
			name:       "a bad listen address is rejected",
			mutate:     func(c *config.Config) { c.Web.Listen = "not-an-ip" },
			wantStatus: http.StatusBadRequest,
			wantDetail: "web.listen",
		},
		{
			name:       "an unknown active provider is rejected",
			mutate:     func(c *config.Config) { c.Provider = "nope" },
			wantStatus: http.StatusBadRequest,
			wantDetail: "provider",
		},
		{
			name: "a literal api key pasted into api_key_env is rejected",
			mutate: func(c *config.Config) {
				pc := c.Providers["openai"]
				pc.APIKeyEnv = "sk-abcdef0123456789"
				c.Providers["openai"] = pc
			},
			wantStatus: http.StatusBadRequest,
			wantDetail: "api_key_env",
		},
		{
			name:       "an invalid log level is rejected",
			mutate:     func(c *config.Config) { c.Logging.Level = "chatty" },
			wantStatus: http.StatusBadRequest,
			wantDetail: "logging.level",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var saved *config.Config
			srv := newTestServer(t, func(o *Options) {
				o.SaveConfig = func(c *config.Config) error { saved = c; return nil }
			})

			incoming := config.Default()
			tc.mutate(incoming)
			rec, body := doJSON(t, srv, http.MethodPut, "/api/config",
				configRequest{Config: incoming, Persist: tc.persist})

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, body)
			}
			if tc.wantStatus != http.StatusOK {
				var env errorEnvelope
				if err := json.Unmarshal(body, &env); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if env.Error.Code != codeInvalidConfig {
					t.Errorf("code = %q, want %q", env.Error.Code, codeInvalidConfig)
				}
				if !strings.Contains(strings.Join(env.Error.Details, "\n"), tc.wantDetail) {
					t.Errorf("details %v, want one mentioning %q", env.Error.Details, tc.wantDetail)
				}
				if saved != nil {
					t.Error("a rejected configuration was still written to disk")
				}
				return
			}
			if tc.wantSaved != (saved != nil) {
				t.Errorf("saved = %v, want %v", saved != nil, tc.wantSaved)
			}
		})
	}
}

// TestPutConfigKeepsRedactedHeaders: a frontend that GETs then PUTs must not
// overwrite a credential with the redaction placeholder it was shown.
func TestPutConfigKeepsRedactedHeaders(t *testing.T) {
	// A plain value: config.Validate rejects credential-shaped header values
	// outright, and this test is about the redaction round trip, not that rule.
	const headerSecret = "original-value"

	cfg := config.Default()
	pc := cfg.Providers["ollama"]
	pc.Headers = map[string]string{"X-Auth": headerSecret, "X-Trace": "on"}
	cfg.Providers["ollama"] = pc

	var saved *config.Config
	srv := newTestServer(t, func(o *Options) {
		o.Config = cfg
		o.SaveConfig = func(c *config.Config) error { saved = c; return nil }
	})

	_, body := doJSON(t, srv, http.MethodGet, "/api/config", nil)
	var got configResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Change something unrelated and send the redacted document straight back.
	got.Config.Providers["ollama"].Headers["X-Trace"] = "off"
	rec, putBody := doJSON(t, srv, http.MethodPut, "/api/config", configRequest{Config: got.Config})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, putBody)
	}
	if saved == nil {
		t.Fatal("nothing was saved")
	}
	if v := saved.Providers["ollama"].Headers["X-Auth"]; v != headerSecret {
		t.Errorf("saved X-Auth = %q, want the original secret to be preserved", v)
	}
	if v := saved.Providers["ollama"].Headers["X-Trace"]; v != "off" {
		t.Errorf("saved X-Trace = %q, want the edit to be applied", v)
	}
	if strings.Contains(string(putBody), headerSecret) {
		t.Error("the PUT response echoed the secret back")
	}
}

// TestStatus checks the §54 document, including that it never carries a token.
func TestStatus(t *testing.T) {
	srv := newTestServer(t, func(o *Options) {
		o.App = newTestApp(t)
		o.Config = o.App.Config
		o.Broker = permissions.NewBroker()
	})

	rec, body := doJSON(t, srv, http.MethodGet, "/api/status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, body)
	}
	var resp statusResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Version.Version == "" {
		t.Error("no version reported")
	}
	if resp.Listen == "" {
		t.Error("no listen address reported")
	}
	if resp.AuthEnabled {
		t.Error("auth reported as enabled on a default config")
	}
	if resp.ProjectPath == "" {
		t.Error("no project path reported")
	}
	if len(resp.Tools) == 0 {
		t.Error("no tools reported")
	}
}

// TestMethodNotAllowed keeps the error envelope consistent for wrong verbs.
func TestMethodNotAllowed(t *testing.T) {
	srv := newTestServer(t, nil)
	tests := []struct{ method, path string }{
		{http.MethodPost, "/api/status"},
		{http.MethodDelete, "/api/config"},
		{http.MethodGet, "/api/message"},
		{http.MethodGet, "/api/session"},
		{http.MethodPut, "/api/approval"},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec, body := doJSON(t, srv, tc.method, tc.path, nil)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405 (body %s)", rec.Code, body)
			}
			if rec.Header().Get("Allow") == "" {
				t.Error("no Allow header")
			}
			var env errorEnvelope
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Error.Code != codeMethodNotAllowed {
				t.Errorf("code = %q, want %q", env.Error.Code, codeMethodNotAllowed)
			}
		})
	}
}

// TestEndpointsWithoutRuntime: a server with no App must degrade politely
// rather than panic.
func TestEndpointsWithoutRuntime(t *testing.T) {
	srv := newTestServer(t, nil)
	for _, path := range []string{"/api/models", "/api/agents", "/api/sessions", "/api/tools"} {
		rec, body := doJSON(t, srv, http.MethodGet, path, nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s = %d, want 503 (body %s)", path, rec.Code, body)
		}
	}
}

// TestApprovalRoundTrip drives a real broker through the API, which is what
// §50 means by approvals being actionable from the WebUI.
func TestApprovalRoundTrip(t *testing.T) {
	broker := permissions.NewBroker()
	t.Cleanup(broker.Close)
	srv := newTestServer(t, func(o *Options) { o.Broker = broker })

	action := permissions.Action{
		Category: permissions.CatShellExecute,
		Risk:     permissions.RiskMedium,
		Tool:     "run",
		Summary:  "run `go test ./...`",
		Detail:   "go test ./...",
	}
	answered := make(chan bool, 1)
	go func() {
		ok, err := broker.RequestDecision(t.Context(), action,
			permissions.Decision{Outcome: permissions.OutcomeConfirm, Reason: "shell execution requires confirmation"})
		if err != nil {
			answered <- false
			return
		}
		answered <- ok
	}()

	var pending []permissions.PendingApproval
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pending = broker.Pending(); len(pending) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(pending) == 0 {
		t.Fatal("the approval never reached the broker")
	}

	// The queue is visible over the API, with the same detail the TUI shows.
	rec, body := doJSON(t, srv, http.MethodGet, "/api/approval", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/approval = %d (body %s)", rec.Code, body)
	}
	var list approvalsResponse
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Pending) != 1 || list.Pending[0].Action.Detail != action.Detail {
		t.Fatalf("pending = %+v, want the queued action", list.Pending)
	}
	if list.Pending[0].Decision.Reason == "" {
		t.Error("the approval carries no reason; the WebUI would be less explicit than the TUI")
	}

	rec, body = doJSON(t, srv, http.MethodPost, "/api/approval",
		approvalRequest{ID: pending[0].ID, Approved: true, Scope: "once"})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/approval = %d (body %s)", rec.Code, body)
	}
	select {
	case ok := <-answered:
		if !ok {
			t.Error("the core was told the action was denied")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the blocked approval was never released")
	}
}

// TestApprovalErrors covers the rejection paths.
func TestApprovalErrors(t *testing.T) {
	tests := []struct {
		name       string
		broker     bool
		req        approvalRequest
		wantStatus int
		wantCode   string
	}{
		{name: "no broker attached", req: approvalRequest{ID: "x"}, wantStatus: http.StatusServiceUnavailable, wantCode: codeUnavailable},
		{name: "missing id", broker: true, wantStatus: http.StatusBadRequest, wantCode: codeBadRequest},
		{name: "unknown id", broker: true, req: approvalRequest{ID: "nope"}, wantStatus: http.StatusNotFound, wantCode: codeNotFound},
		{name: "bad scope", broker: true, req: approvalRequest{ID: "x", Scope: "forever"}, wantStatus: http.StatusBadRequest, wantCode: codeBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t, func(o *Options) {
				if tc.broker {
					b := permissions.NewBroker()
					t.Cleanup(b.Close)
					o.Broker = b
				}
			})
			rec, body := doJSON(t, srv, http.MethodPost, "/api/approval", tc.req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, body)
			}
			var env errorEnvelope
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Error.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", env.Error.Code, tc.wantCode)
			}
		})
	}
}

// TestSessionLifecycle exercises POST /api/session and GET /api/sessions
// against a real store.
func TestSessionLifecycle(t *testing.T) {
	application := newTestApp(t)
	srv := newTestServer(t, func(o *Options) {
		o.App = application
		o.Config = application.Config
	})

	rec, body := doJSON(t, srv, http.MethodPost, "/api/session", sessionRequest{Title: "first"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/session = %d (body %s)", rec.Code, body)
	}
	var created sessionDetail
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Session.ID == "" {
		t.Fatal("no session id returned")
	}
	if srv.CurrentSession() != created.Session.ID {
		t.Errorf("current session = %q, want %q", srv.CurrentSession(), created.Session.ID)
	}

	rec, body = doJSON(t, srv, http.MethodGet, "/api/sessions", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/sessions = %d (body %s)", rec.Code, body)
	}
	var list sessionsResponse
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].ID != created.Session.ID {
		t.Fatalf("sessions = %+v, want the created one", list.Sessions)
	}
	if list.Current != created.Session.ID {
		t.Errorf("current = %q, want %q", list.Current, created.Session.ID)
	}

	// Resuming selects the same session again.
	rec, body = doJSON(t, srv, http.MethodPost, "/api/session", sessionRequest{Resume: created.Session.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("resume = %d (body %s)", rec.Code, body)
	}

	rec, body = doJSON(t, srv, http.MethodPost, "/api/session", sessionRequest{Resume: "does-not-exist"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("resuming an unknown session = %d, want 404 (body %s)", rec.Code, body)
	}
}

// TestCreateAgent records a requested agent against the session and announces
// it, so every frontend sees the same queue (§26).
func TestCreateAgent(t *testing.T) {
	application := newTestApp(t)
	srv := newTestServer(t, func(o *Options) {
		o.App = application
		o.Config = application.Config
	})

	created := make(chan string, 4)
	unsubscribe := application.Bus.Subscribe(func(ev app.Event) {
		select {
		case created <- ev.AgentID:
		default:
		}
	}, app.EventAgentCreated)
	t.Cleanup(unsubscribe)

	rec, body := doJSON(t, srv, http.MethodPost, "/api/session", sessionRequest{})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create session: %d %s", rec.Code, body)
	}

	rec, body = doJSON(t, srv, http.MethodPost, "/api/agents", agentRequest{Name: "research", Task: "read the spec"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/agents = %d (body %s)", rec.Code, body)
	}
	select {
	case id := <-created:
		if id == "" {
			t.Error("agent.created carried no agent id")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no agent.created event was published")
	}

	rec, body = doJSON(t, srv, http.MethodGet, "/api/agents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/agents = %d (body %s)", rec.Code, body)
	}
	var list agentsResponse
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Agents) != 1 {
		t.Fatalf("agents = %+v, want one", list.Agents)
	}
	if list.Agents[0].Task != "read the spec" {
		t.Errorf("task = %q", list.Agents[0].Task)
	}

	rec, body = doJSON(t, srv, http.MethodPost, "/api/agents", agentRequest{Name: "empty"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an agent without a task = %d, want 400 (body %s)", rec.Code, body)
	}
}

// TestAgentsDisabled: the configuration switch must actually gate the API.
func TestAgentsDisabled(t *testing.T) {
	application := newTestApp(t)
	cfg := *application.Config
	cfg.Agents.Enabled = false
	srv := newTestServer(t, func(o *Options) {
		o.App = application
		o.Config = &cfg
	})
	srv.setCurrentSession("some-session")
	rec, body := doJSON(t, srv, http.MethodPost, "/api/agents", agentRequest{Task: "x"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, body)
	}
}

// TestMessageRequiresContent guards the cheapest input validation.
func TestMessageRequiresContent(t *testing.T) {
	application := newTestApp(t)
	srv := newTestServer(t, func(o *Options) {
		o.App = application
		o.Config = application.Config
	})
	rec, body := doJSON(t, srv, http.MethodPost, "/api/message", messageRequest{Content: "   "})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, body)
	}
}

// TestBadJSONRejected checks the decoder's guard rails.
func TestBadJSONRejected(t *testing.T) {
	srv := newTestServer(t, nil)

	req := httptest.NewRequest(http.MethodPut, "/api/config", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON = %d, want 400", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPut, "/api/config", strings.NewReader(`{"config":{},"nope":1}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d, want 400", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPut, "/api/config", strings.NewReader("hello"))
	req.Header.Set("Content-Type", "text/plain")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("wrong content type = %d, want 415", rec.Code)
	}
}

// TestStatsWithoutTracker: the endpoint still answers usefully when no stats
// tracker is wired in.
func TestStatsWithoutTracker(t *testing.T) {
	application := newTestApp(t)
	srv := newTestServer(t, func(o *Options) {
		o.App = application
		o.Config = application.Config
	})
	sess, err := application.Sessions.Create(t.Context(), session.CreateOptions{ProjectPath: application.Workspace.Root()})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	srv.setCurrentSession(sess.ID)

	rec, body := doJSON(t, srv, http.MethodGet, "/api/stats", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, body)
	}
	var resp statsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.SessionID != sess.ID {
		t.Errorf("session_id = %q, want %q", resp.SessionID, sess.ID)
	}
}

func boolPtr(b bool) *bool { return &b }

// TestTurnConflict: two turns on one session would interleave tool calls and
// transcript writes, so the second is refused rather than queued silently.
func TestTurnConflict(t *testing.T) {
	application := newTestApp(t)
	srv := newTestServer(t, func(o *Options) {
		o.App = application
		o.Config = application.Config
	})

	sess, err := application.Sessions.Create(t.Context(), session.CreateOptions{
		ProjectPath: application.Workspace.Root(),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	// Pretend a turn is already running for this session.
	if !srv.beginTurn(sess.ID, func() {}) {
		t.Fatal("beginTurn refused the first turn")
	}
	t.Cleanup(func() { srv.endTurn(sess.ID) })

	rec, body := doJSON(t, srv, http.MethodPost, "/api/message",
		messageRequest{SessionID: sess.ID, Content: "hello"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, body)
	}
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error.Code != codeConflict {
		t.Errorf("code = %q, want %q", env.Error.Code, codeConflict)
	}
}

// TestCancelTurnInterrupts covers §51: a running turn can be stopped, and
// cancelling a session that is idle reports that plainly.
func TestCancelTurnInterrupts(t *testing.T) {
	srv := newTestServer(t, nil)

	cancelled := make(chan struct{})
	if !srv.beginTurn("s1", func() { close(cancelled) }) {
		t.Fatal("beginTurn failed")
	}
	if !srv.cancelTurn("s1") {
		t.Fatal("cancelTurn reported no running turn")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("the turn's cancel function was never called")
	}
	srv.endTurn("s1")
	if srv.cancelTurn("s1") {
		t.Error("cancelTurn reported success for an idle session")
	}
}

// TestShutdownCancelsRunningTurns: §58 requires the model loop to stop, not to
// be abandoned mid-flight.
func TestShutdownCancelsRunningTurns(t *testing.T) {
	srv, err := New(Options{Config: config.Default(), Logger: discardLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cancelled := make(chan struct{})
	if !srv.beginTurn("s1", func() { close(cancelled) }) {
		t.Fatal("beginTurn failed")
	}
	if err := srv.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("shutdown left a turn running")
	}
}
