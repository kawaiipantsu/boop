package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kawaiipantsu/boop/internal/app"
	"github.com/kawaiipantsu/boop/internal/config"
	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/provider"
	"github.com/kawaiipantsu/boop/internal/session"
	"github.com/kawaiipantsu/boop/internal/version"
)

// Error codes returned in the JSON error envelope. They are stable strings so
// a frontend can branch on them without parsing prose.
const (
	codeBadRequest       = "bad_request"
	codeUnauthorized     = "unauthorized"
	codeForbidden        = "forbidden"
	codeNotFound         = "not_found"
	codeMethodNotAllowed = "method_not_allowed"
	codeConflict         = "conflict"
	codeUnavailable      = "unavailable"
	codeInvalidConfig    = "invalid_config"
	codeInternal         = "internal"
)

// maxRequestBytes bounds a request body. The API takes prompts and config
// documents, not uploads, so a megabyte is generous.
const maxRequestBytes = 1 << 20

// listModelsTimeout bounds the per-provider probe behind GET /api/models, so
// one unreachable backend cannot hang the whole listing.
const listModelsTimeout = 10 * time.Second

// historyWindow bounds how many stored turns are replayed into a new request.
//
// §47 forbids blind-sending an entire session. Until the context manager is
// wired into the loop this is the honest, bounded approximation: the most
// recent turns, oldest first.
const historyWindow = 40

// redactedValue replaces any configured header value in API output.
//
// Header values are free-form and users do paste credentials into them, so the
// WebUI never sees one (§45). A PUT that echoes this sentinel back keeps the
// stored value rather than overwriting a secret with the word "[redacted]".
const redactedValue = "[redacted]"

// errorEnvelope is the single error shape used by every endpoint.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

// errorBody carries a machine-readable code, a human message and, for
// validation failures, the individual problems.
type errorBody struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Details []string `json:"details,omitempty"`
}

// writeJSON writes a successful JSON response.
func writeJSON(w http.ResponseWriter, status int, body any) {
	data, err := json.Marshal(body)
	if err != nil {
		// Encoding our own response types cannot fail in practice; if it does,
		// say so rather than emitting a half-written body.
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"response encoding failed"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// writeError writes the error envelope.
func writeError(w http.ResponseWriter, status int, code, message string, details ...string) {
	writeJSON(w, status, errorEnvelope{Error: errorBody{Code: code, Message: message, Details: details}})
}

// decodeBody reads a bounded, strictly-typed JSON request body.
//
// Unknown fields are rejected: a typo in a field name that silently does
// nothing is a bug report waiting to happen.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if base, _, _ := strings.Cut(ct, ";"); strings.TrimSpace(base) != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, codeBadRequest, "the request body must be application/json")
			return false
		}
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "the request body is not valid JSON: "+err.Error())
		return false
	}
	return true
}

// methodNotAllowed answers a request with the wrong verb.
func methodNotAllowed(w http.ResponseWriter, allowed ...string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed,
		"this endpoint accepts "+strings.Join(allowed, ", "))
}

// requireApp reports whether a runtime is attached, answering the request when
// it is not.
func (s *Server) requireApp(w http.ResponseWriter) bool {
	if s.app == nil {
		writeError(w, http.StatusServiceUnavailable, codeUnavailable,
			"this server was started without a Boop runtime attached")
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// GET /api/status
// ---------------------------------------------------------------------------

// statusResponse is the §54 health document.
type statusResponse struct {
	Version          version.Info     `json:"version"`
	StartedAt        time.Time        `json:"started_at"`
	UptimeMS         int64            `json:"uptime_ms"`
	Listen           string           `json:"listen"`
	AuthEnabled      bool             `json:"auth_enabled"`
	TrustedProxy     bool             `json:"trusted_proxy_headers"`
	AllowedOrigins   []string         `json:"allowed_origins"`
	Provider         string           `json:"provider"`
	Model            string           `json:"model"`
	Mode             string           `json:"mode"`
	AgentsEnabled    bool             `json:"agents_enabled"`
	NetworkEnabled   bool             `json:"network_enabled"`
	ProjectPath      string           `json:"project_path,omitempty"`
	HasMemory        bool             `json:"has_project_memory"`
	BundleEmbedded   bool             `json:"bundle_embedded"`
	CurrentSession   string           `json:"current_session,omitempty"`
	Clients          int              `json:"clients"`
	PendingApprovals int              `json:"pending_approvals"`
	Providers        []providerHealth `json:"providers,omitempty"`
	Tools            []string         `json:"tools,omitempty"`
	Warnings         []string         `json:"warnings,omitempty"`
	RestartRequired  bool             `json:"restart_required"`
}

// providerHealth is the cached view of one backend's reachability.
type providerHealth struct {
	Name    string `json:"name"`
	Healthy bool   `json:"healthy"`
	Error   string `json:"error,omitempty"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	_, bundled := bundleFS()
	s.mu.Lock()
	restart := s.restart
	current := s.currentSession
	s.mu.Unlock()

	resp := statusResponse{
		Version:         version.Get(),
		StartedAt:       s.startedAt,
		UptimeMS:        s.now().Sub(s.startedAt).Milliseconds(),
		Listen:          s.Addr(),
		AuthEnabled:     s.auth.Enabled(),
		TrustedProxy:    s.web.TrustedProxyHeaders,
		AllowedOrigins:  append([]string(nil), s.web.AllowedOrigins...),
		Provider:        s.cfg.Provider,
		Model:           s.cfg.Model,
		Mode:            string(s.cfg.Execution.Mode),
		AgentsEnabled:   s.cfg.Agents.Enabled,
		NetworkEnabled:  s.cfg.Network.Enabled,
		BundleEmbedded:  bundled,
		CurrentSession:  current,
		Clients:         s.hub.count(),
		RestartRequired: restart,
	}
	if resp.AllowedOrigins == nil {
		resp.AllowedOrigins = []string{}
	}
	if s.broker != nil {
		resp.PendingApprovals = len(s.broker.Pending())
	}
	if s.app != nil {
		resp.Warnings = s.app.Warnings
		resp.HasMemory = s.app.Memory != nil
		if s.app.Workspace != nil {
			resp.ProjectPath = s.app.Workspace.Root()
		}
		if s.app.Tools != nil {
			resp.Tools = s.app.Tools.Names()
		}
		if s.app.Router != nil {
			for name, err := range s.app.Router.HealthSnapshot() {
				ph := providerHealth{Name: name, Healthy: err == nil}
				if err != nil {
					ph.Error = err.Error()
				}
				resp.Providers = append(resp.Providers, ph)
			}
			sort.Slice(resp.Providers, func(i, j int) bool { return resp.Providers[i].Name < resp.Providers[j].Name })
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// GET|PUT /api/config
// ---------------------------------------------------------------------------

// configResponse is the redacted configuration document.
type configResponse struct {
	Config *config.Config `json:"config"`
	// Secrets lists where credentials come from, by environment variable
	// name. Values are never included (§45); Set only reports whether the
	// variable currently resolves to something non-empty.
	Secrets []secretRef `json:"secrets"`
	// Path is the file a PUT would write to.
	Path string `json:"path,omitempty"`
	// Warnings are config.Validate's non-fatal findings.
	Warnings []string `json:"warnings,omitempty"`
	// RestartRequired reports that a saved change is not yet in effect.
	RestartRequired bool `json:"restart_required"`
}

// secretRef names one credential source without disclosing it.
type secretRef struct {
	// Scope is the config location, such as "providers.openai" or "web.auth".
	Scope string `json:"scope"`
	// Env is the environment variable name the value is read from.
	Env string `json:"env"`
	// Set reports whether that variable currently holds a value.
	Set bool `json:"set"`
}

// configRequest is the PUT body. The envelope mirrors the GET response so a
// frontend can round-trip the document it was given.
type configRequest struct {
	Config *config.Config `json:"config"`
	// Persist writes the accepted configuration to disk. The default is true;
	// send false to validate without saving.
	Persist *bool `json:"persist,omitempty"`
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.getConfig(w)
	case http.MethodPut:
		s.putConfig(w, r)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPut)
	}
}

func (s *Server) getConfig(w http.ResponseWriter) {
	warnings, _ := s.cfg.Validate()
	s.mu.Lock()
	restart := s.restart
	s.mu.Unlock()
	path, _ := config.ConfigFile()
	writeJSON(w, http.StatusOK, configResponse{
		Config:          redactConfig(s.cfg),
		Secrets:         s.secretRefs(s.cfg),
		Path:            path,
		Warnings:        warnings,
		RestartRequired: restart,
	})
}

// putConfig validates and persists a replacement configuration.
//
// The running process is not mutated. The runtime reads *config.Config from
// many goroutines without synchronisation, so rewriting it underneath them
// would be a data race, and a half-applied configuration is worse than one
// that takes effect on restart. The response says so explicitly.
func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	var req configRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Config == nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "the request body must contain a `config` object")
		return
	}
	incoming := req.Config
	restoreRedactedHeaders(incoming, s.cfg)

	warnings, err := incoming.Validate()
	if err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidConfig,
			"the configuration was rejected and nothing was changed", splitValidationErrors(err)...)
		return
	}

	persist := true
	if req.Persist != nil {
		persist = *req.Persist
	}
	if persist {
		if err := s.saveConfig(incoming); err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "the configuration is valid but could not be saved: "+err.Error())
			return
		}
		s.mu.Lock()
		s.restart = true
		s.mu.Unlock()
	}

	path, _ := config.ConfigFile()
	writeJSON(w, http.StatusOK, configResponse{
		Config:          redactConfig(incoming),
		Secrets:         s.secretRefs(incoming),
		Path:            path,
		Warnings:        warnings,
		RestartRequired: persist,
	})
}

// splitValidationErrors turns joined validation errors into one detail per
// problem, so a settings form can show them next to the offending field.
func splitValidationErrors(err error) []string {
	if err == nil {
		return nil
	}
	lines := strings.Split(err.Error(), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// redactConfig copies a configuration with every free-form secret removed.
//
// The struct itself is secret-free by design — credentials are named, not
// stored (§45) — so the only thing to scrub is the per-provider header map,
// which users do fill with literal tokens.
func redactConfig(c *config.Config) *config.Config {
	out := *c
	if c.Providers != nil {
		providers := make(map[string]config.ProviderConfig, len(c.Providers))
		for name, pc := range c.Providers {
			if len(pc.Headers) > 0 {
				headers := make(map[string]string, len(pc.Headers))
				for k := range pc.Headers {
					headers[k] = redactedValue
				}
				pc.Headers = headers
			}
			providers[name] = pc
		}
		out.Providers = providers
	}
	return &out
}

// restoreRedactedHeaders puts back header values the client never saw, so a
// straight round-trip of GET then PUT does not overwrite a credential with the
// redaction sentinel.
func restoreRedactedHeaders(incoming, current *config.Config) {
	for name, pc := range incoming.Providers {
		existing, ok := current.Providers[name]
		if !ok || len(pc.Headers) == 0 {
			continue
		}
		changed := false
		headers := make(map[string]string, len(pc.Headers))
		for k, v := range pc.Headers {
			if v == redactedValue {
				if old, had := existing.Headers[k]; had {
					headers[k] = old
					changed = true
					continue
				}
			}
			headers[k] = v
		}
		if changed {
			pc.Headers = headers
			incoming.Providers[name] = pc
		}
	}
}

// secretRefs reports which environment variables supply credentials and
// whether they are currently set. It never reads a value into the response.
func (s *Server) secretRefs(c *config.Config) []secretRef {
	refs := make([]secretRef, 0, len(c.Providers)+1)
	names := make([]string, 0, len(c.Providers))
	for name := range c.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		env := strings.TrimSpace(c.Providers[name].APIKeyEnv)
		if env == "" {
			continue
		}
		refs = append(refs, secretRef{Scope: "providers." + name, Env: env, Set: s.envSet(env)})
	}
	if env := strings.TrimSpace(c.Web.Auth.TokenEnv); env != "" {
		refs = append(refs, secretRef{Scope: "web.auth", Env: env, Set: s.envSet(env)})
	}
	return refs
}

// envSet reports whether an environment variable holds a non-empty value.
func (s *Server) envSet(name string) bool {
	v, ok := s.lookupEnv(name)
	return ok && strings.TrimSpace(v) != ""
}

// ---------------------------------------------------------------------------
// GET /api/models
// ---------------------------------------------------------------------------

// modelsResponse lists every model the configured providers offer.
type modelsResponse struct {
	Models []provider.Model `json:"models"`
	// Errors reports providers that could not be listed. A local backend that
	// is not running is normal, so it degrades the listing rather than
	// failing it (§8).
	Errors []providerHealth `json:"errors,omitempty"`
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if !s.requireApp(w) {
		return
	}
	registry := s.app.Router.Registry()
	names := registry.Names()
	if want := strings.TrimSpace(r.URL.Query().Get("provider")); want != "" {
		if _, ok := registry.Get(want); !ok {
			writeError(w, http.StatusNotFound, codeNotFound, "no provider named "+strconv.Quote(want)+" is configured")
			return
		}
		names = []string{want}
	}

	ctx, cancel := context.WithTimeout(r.Context(), listModelsTimeout)
	defer cancel()

	var (
		mu   sync.Mutex
		resp modelsResponse
		wg   sync.WaitGroup
	)
	for _, name := range names {
		p, ok := registry.Get(name)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(name string, p provider.Provider) {
			defer wg.Done()
			models, err := p.ListModels(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				resp.Errors = append(resp.Errors, providerHealth{Name: name, Error: err.Error()})
				return
			}
			for _, m := range models {
				if m.Provider == "" {
					m.Provider = name
				}
				resp.Models = append(resp.Models, m)
			}
		}(name, p)
	}
	wg.Wait()

	sort.Slice(resp.Models, func(i, j int) bool {
		if resp.Models[i].Provider != resp.Models[j].Provider {
			return resp.Models[i].Provider < resp.Models[j].Provider
		}
		return resp.Models[i].ID < resp.Models[j].ID
	})
	sort.Slice(resp.Errors, func(i, j int) bool { return resp.Errors[i].Name < resp.Errors[j].Name })
	if resp.Models == nil {
		resp.Models = []provider.Model{}
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// GET /api/providers
// ---------------------------------------------------------------------------

// providerInfo describes one configured backend. It carries the credential
// variable's name and whether it resolves, never its value (§45).
type providerInfo struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	BaseURL    string `json:"base_url,omitempty"`
	APIKeyEnv  string `json:"api_key_env,omitempty"`
	APIKeySet  bool   `json:"api_key_set"`
	Disabled   bool   `json:"disabled"`
	Active     bool   `json:"active"`
	Registered bool   `json:"registered"`
	Healthy    bool   `json:"healthy"`
	Error      string `json:"error,omitempty"`
}

// providersResponse is the /api/providers document.
type providersResponse struct {
	Active    string         `json:"active"`
	Providers []providerInfo `json:"providers"`
	// Warnings explains providers that are configured but could not be built.
	Warnings []string `json:"warnings,omitempty"`
}

func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	// A live probe touches the network, so it happens only when asked for.
	live := r.URL.Query().Get("check") == "true"

	names := make([]string, 0, len(s.cfg.Providers))
	for name := range s.cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)

	resp := providersResponse{Active: s.cfg.Provider, Providers: make([]providerInfo, 0, len(names))}
	var health map[string]error
	if s.app != nil && s.app.Router != nil {
		health = s.app.Router.HealthSnapshot()
		resp.Warnings = s.app.Warnings
	}
	for _, name := range names {
		pc := s.cfg.Providers[name]
		info := providerInfo{
			Name:      name,
			Type:      pc.Type,
			BaseURL:   pc.BaseURL,
			APIKeyEnv: pc.APIKeyEnv,
			APIKeySet: pc.APIKeyEnv != "" && s.envSet(pc.APIKeyEnv),
			Disabled:  pc.Disabled,
			Active:    name == s.cfg.Provider,
			Healthy:   true,
		}
		if s.app != nil && s.app.Router != nil {
			_, info.Registered = s.app.Router.Registry().Get(name)
		}
		if err, known := health[name]; known && err != nil {
			info.Healthy, info.Error = false, err.Error()
		}
		if live && info.Registered {
			ctx, cancel := context.WithTimeout(r.Context(), listModelsTimeout)
			err := s.app.Router.Health(ctx, name)
			cancel()
			info.Healthy = err == nil
			if err != nil {
				info.Error = err.Error()
			} else {
				info.Error = ""
			}
		}
		resp.Providers = append(resp.Providers, info)
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// GET /api/tools
// ---------------------------------------------------------------------------

// toolsResponse lists the registered tools and their argument schemas, which
// is what the WebUI's Tools view renders (§26).
type toolsResponse struct {
	Tools []provider.ToolDefinition `json:"tools"`
	Mode  string                    `json:"mode"`
}

func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if !s.requireApp(w) {
		return
	}
	defs := s.app.Tools.Definitions(nil)
	if defs == nil {
		defs = []provider.ToolDefinition{}
	}
	writeJSON(w, http.StatusOK, toolsResponse{Tools: defs, Mode: string(s.cfg.Execution.Mode)})
}

// ---------------------------------------------------------------------------
// GET|POST /api/agents
// ---------------------------------------------------------------------------

// agentsResponse lists the agents recorded for a session (§26).
type agentsResponse struct {
	SessionID string                `json:"session_id"`
	Agents    []session.AgentRecord `json:"agents"`
	Max       int                   `json:"max"`
	Enabled   bool                  `json:"enabled"`
}

// agentRequest asks for a new agent.
type agentRequest struct {
	SessionID string `json:"session_id,omitempty"`
	Name      string `json:"name"`
	Task      string `json:"task"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	ParentID  string `json:"parent_id,omitempty"`
}

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

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	sessionID := strings.TrimSpace(r.URL.Query().Get("session_id"))
	if sessionID == "" {
		sessionID = s.CurrentSession()
	}
	resp := agentsResponse{
		SessionID: sessionID,
		Agents:    []session.AgentRecord{},
		Max:       s.cfg.Agents.Max,
		Enabled:   s.cfg.Agents.Enabled,
	}
	if sessionID != "" {
		agents, err := s.app.Sessions.Agents(r.Context(), sessionID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "cannot list agents: "+err.Error())
			return
		}
		if agents != nil {
			resp.Agents = agents
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// createAgent records a requested agent and announces it on the bus.
//
// The scheduler that actually runs agents is a separate milestone (§11); this
// endpoint owns the part that belongs to the API — validating the request,
// persisting it against the session, and emitting agent.created so every
// attached frontend sees the same queue. The record's status is "queued", not
// "running", because claiming otherwise would be a lie the UI would repeat.
func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	var req agentRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Task) == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "an agent needs a `task`")
		return
	}
	if !s.cfg.Agents.Enabled {
		writeError(w, http.StatusConflict, codeConflict, "agents are disabled; set agents.enabled in the configuration")
		return
	}

	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		sessionID = s.CurrentSession()
	}
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "no session is selected; POST /api/session first or pass session_id")
		return
	}
	existing, err := s.app.Sessions.Agents(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "cannot read the agent list: "+err.Error())
		return
	}
	if s.cfg.Agents.Max > 0 && countActiveAgents(existing) >= s.cfg.Agents.Max {
		writeError(w, http.StatusConflict, codeConflict,
			fmt.Sprintf("the agent limit of %d is already reached (agents.max)", s.cfg.Agents.Max))
		return
	}

	now := s.now().UTC()
	rec := &session.AgentRecord{
		SessionID: sessionID,
		ParentID:  req.ParentID,
		Name:      strings.TrimSpace(req.Name),
		Task:      strings.TrimSpace(req.Task),
		Provider:  firstNonEmpty(req.Provider, s.cfg.Provider),
		Model:     firstNonEmpty(req.Model, s.cfg.Model),
		Status:    "queued",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.app.Sessions.SaveAgent(r.Context(), rec); err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "cannot record the agent: "+err.Error())
		return
	}
	s.app.Bus.Publish(app.Event{
		Type:      app.EventAgentCreated,
		SessionID: sessionID,
		AgentID:   rec.ID,
		Payload:   rec,
	})
	writeJSON(w, http.StatusCreated, map[string]any{"agent": rec})
}

// countActiveAgents counts agents that have not finished, which is what
// agents.max bounds.
func countActiveAgents(recs []session.AgentRecord) int {
	n := 0
	for _, rec := range recs {
		switch rec.Status {
		case "complete", "completed", "failed", "error", "cancelled":
		default:
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// GET /api/sessions
// ---------------------------------------------------------------------------

// sessionsResponse lists session headers.
type sessionsResponse struct {
	Sessions []*session.Session `json:"sessions"`
	Current  string             `json:"current,omitempty"`
}

// sessionDetail is one session with its transcript.
type sessionDetail struct {
	Session  *session.Session   `json:"session"`
	Messages []provider.Message `json:"messages"`
	Usage    any                `json:"usage,omitempty"`
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if !s.requireApp(w) {
		return
	}
	if id := strings.TrimSpace(r.URL.Query().Get("id")); id != "" {
		s.sessionDetail(w, r, id)
		return
	}
	limit := 0
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, codeBadRequest, "limit must be a non-negative integer")
			return
		}
		limit = n
	}
	sessions, err := s.app.Sessions.List(r.Context(), session.ListOptions{
		ProjectPath: strings.TrimSpace(r.URL.Query().Get("project")),
		Limit:       limit,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "cannot list sessions: "+err.Error())
		return
	}
	if sessions == nil {
		sessions = []*session.Session{}
	}
	writeJSON(w, http.StatusOK, sessionsResponse{Sessions: sessions, Current: s.CurrentSession()})
}

func (s *Server) sessionDetail(w http.ResponseWriter, r *http.Request, id string) {
	sess, err := s.app.Sessions.Load(r.Context(), id)
	if err != nil {
		if errors.Is(err, session.ErrNotFound) {
			writeError(w, http.StatusNotFound, codeNotFound, "no session "+strconv.Quote(id))
			return
		}
		writeError(w, http.StatusInternalServerError, codeInternal, "cannot load the session: "+err.Error())
		return
	}
	messages, err := s.app.Sessions.History().Messages(r.Context(), id, session.TranscriptOptions{})
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "cannot read the transcript: "+err.Error())
		return
	}
	if messages == nil {
		messages = []provider.Message{}
	}
	detail := sessionDetail{Session: sess, Messages: messages}
	if usage, err := s.app.Sessions.Usage(r.Context(), id); err == nil {
		detail.Usage = usage
	}
	writeJSON(w, http.StatusOK, detail)
}

// ---------------------------------------------------------------------------
// POST /api/session
// ---------------------------------------------------------------------------

// sessionRequest creates or resumes a session and selects it for the WebUI.
type sessionRequest struct {
	// Resume names an existing session to attach to instead of creating one.
	Resume   string `json:"resume,omitempty"`
	Title    string `json:"title,omitempty"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !s.requireApp(w) {
		return
	}
	var req sessionRequest
	if r.ContentLength != 0 && !decodeBody(w, r, &req) {
		return
	}

	projectPath := ""
	if s.app.Workspace != nil {
		projectPath = s.app.Workspace.Root()
	}

	var (
		sess *session.Session
		err  error
	)
	if id := strings.TrimSpace(req.Resume); id != "" {
		sess, err = s.app.Sessions.Resume(r.Context(), id, projectPath)
		if err != nil {
			switch {
			case errors.Is(err, session.ErrNotFound):
				writeError(w, http.StatusNotFound, codeNotFound, "no session "+strconv.Quote(id))
			case errors.Is(err, session.ErrProjectMismatch):
				writeError(w, http.StatusConflict, codeConflict, err.Error())
			default:
				writeError(w, http.StatusInternalServerError, codeInternal, "cannot resume the session: "+err.Error())
			}
			return
		}
	} else {
		sess, err = s.app.Sessions.Create(r.Context(), session.CreateOptions{
			ProjectPath: projectPath,
			Provider:    firstNonEmpty(req.Provider, s.cfg.Provider),
			Model:       firstNonEmpty(req.Model, s.cfg.Model),
			Title:       req.Title,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "cannot create a session: "+err.Error())
			return
		}
		s.app.Bus.Publish(app.Event{Type: app.EventSessionStarted, SessionID: sess.ID, Payload: sess})
	}
	s.setCurrentSession(sess.ID)

	messages, err := s.app.Sessions.History().Messages(r.Context(), sess.ID, session.TranscriptOptions{})
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "cannot read the transcript: "+err.Error())
		return
	}
	if messages == nil {
		messages = []provider.Message{}
	}
	status := http.StatusCreated
	if req.Resume != "" {
		status = http.StatusOK
	}
	writeJSON(w, status, sessionDetail{Session: sess, Messages: messages})
}

// ---------------------------------------------------------------------------
// GET /api/stats
// ---------------------------------------------------------------------------

// statsResponse is the §28 statistics document.
type statsResponse struct {
	UptimeMS int64 `json:"uptime_ms"`
	// Snapshot is the live tracker state; null when no tracker is attached,
	// in which case Usage carries the persisted totals instead.
	Snapshot  any    `json:"snapshot"`
	SessionID string `json:"session_id,omitempty"`
	Usage     any    `json:"usage,omitempty"`
	Messages  int64  `json:"messages"`
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	resp := statsResponse{UptimeMS: s.now().Sub(s.startedAt).Milliseconds()}
	if s.stats != nil {
		snap := s.stats.Snapshot()
		resp.Snapshot = snap
	}
	sessionID := strings.TrimSpace(r.URL.Query().Get("session_id"))
	if sessionID == "" {
		sessionID = s.CurrentSession()
	}
	if s.app != nil && sessionID != "" {
		resp.SessionID = sessionID
		if usage, err := s.app.Sessions.Usage(r.Context(), sessionID); err == nil {
			resp.Usage = usage
		}
		if n, err := s.app.Sessions.History().Count(r.Context(), sessionID); err == nil {
			resp.Messages = n
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// POST /api/approval
// ---------------------------------------------------------------------------

// approvalsResponse is the pending queue plus the standing session grants, so
// the WebUI can show exactly what the TUI shows (§50).
type approvalsResponse struct {
	Pending []permissions.PendingApproval `json:"pending"`
	Grants  []permissions.Grant           `json:"grants"`
	Mode    string                        `json:"mode"`
}

// approvalRequest answers one pending approval.
type approvalRequest struct {
	ID       string `json:"id"`
	Approved bool   `json:"approved"`
	// Scope is "once" (default), "session.category" or "session.command".
	// The broker may narrow it; the response reports what was applied.
	Scope string `json:"scope,omitempty"`
}

// approvalResult reports the applied decision.
type approvalResult struct {
	ID       string `json:"id"`
	Approved bool   `json:"approved"`
	Scope    string `json:"scope"`
}

func (s *Server) handleApproval(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listApprovals(w)
	case http.MethodPost:
		s.resolveApproval(w, r)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (s *Server) listApprovals(w http.ResponseWriter) {
	if s.broker == nil {
		writeError(w, http.StatusServiceUnavailable, codeUnavailable, "no approval broker is attached to this server")
		return
	}
	resp := approvalsResponse{
		Pending: s.broker.Pending(),
		Grants:  s.broker.SessionGrants(),
		Mode:    string(s.cfg.Execution.Mode),
	}
	if resp.Pending == nil {
		resp.Pending = []permissions.PendingApproval{}
	}
	if resp.Grants == nil {
		resp.Grants = []permissions.Grant{}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) resolveApproval(w http.ResponseWriter, r *http.Request) {
	var req approvalRequest
	if !decodeBody(w, r, &req) {
		return
	}
	result, status, err := s.applyApproval(req)
	if err != nil {
		writeError(w, status, codeForApprovalError(status), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// applyApproval resolves one approval. It is shared with the WebSocket path so
// both transports go through the same broker and cannot diverge (§50).
func (s *Server) applyApproval(req approvalRequest) (approvalResult, int, error) {
	if s.broker == nil {
		return approvalResult{}, http.StatusServiceUnavailable, errors.New("no approval broker is attached to this server")
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		return approvalResult{}, http.StatusBadRequest, errors.New("an approval `id` is required")
	}
	scope, err := parseScope(req.Scope)
	if err != nil {
		return approvalResult{}, http.StatusBadRequest, err
	}
	if err := s.broker.ResolveWithScope(id, req.Approved, scope); err != nil {
		if errors.Is(err, permissions.ErrNoSuchApproval) {
			return approvalResult{}, http.StatusNotFound,
				fmt.Errorf("approval %s is not pending; it may have been answered elsewhere or timed out", id)
		}
		return approvalResult{}, http.StatusInternalServerError, err
	}
	return approvalResult{ID: id, Approved: req.Approved, Scope: string(scope)}, http.StatusOK, nil
}

// parseScope validates the requested grant scope.
func parseScope(raw string) (permissions.GrantScope, error) {
	switch permissions.GrantScope(strings.TrimSpace(raw)) {
	case "", permissions.ScopeOnce:
		return permissions.ScopeOnce, nil
	case permissions.ScopeSessionCategory:
		return permissions.ScopeSessionCategory, nil
	case permissions.ScopeSessionCommand:
		return permissions.ScopeSessionCommand, nil
	default:
		return "", fmt.Errorf("scope %q is not one of %q, %q, %q",
			raw, permissions.ScopeOnce, permissions.ScopeSessionCategory, permissions.ScopeSessionCommand)
	}
}

// codeForApprovalError maps an HTTP status to the envelope's error code.
func codeForApprovalError(status int) string {
	switch status {
	case http.StatusBadRequest:
		return codeBadRequest
	case http.StatusNotFound:
		return codeNotFound
	case http.StatusServiceUnavailable:
		return codeUnavailable
	default:
		return codeInternal
	}
}

// ---------------------------------------------------------------------------
// POST /api/message
// ---------------------------------------------------------------------------

// messageRequest submits a user turn.
type messageRequest struct {
	SessionID string `json:"session_id,omitempty"`
	Content   string `json:"content"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	// Async returns as soon as the turn starts. The result then arrives only
	// as bus events on the WebSocket, which is the right shape for a long run
	// that would otherwise sit on an idle HTTP connection.
	Async bool `json:"async,omitempty"`
}

// turnResponse is the completed turn.
type turnResponse struct {
	SessionID      string             `json:"session_id"`
	Text           string             `json:"text"`
	Messages       []provider.Message `json:"messages"`
	Usage          provider.Usage     `json:"usage"`
	ToolCalls      int                `json:"tool_calls"`
	Iterations     int                `json:"iterations"`
	StoppedAtLimit bool               `json:"stopped_at_limit"`
	Provider       string             `json:"provider"`
	Model          string             `json:"model"`
}

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !s.requireApp(w) {
		return
	}
	var req messageRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "`content` must not be empty")
		return
	}

	sessionID, err := s.resolveSession(r.Context(), req.SessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}

	if req.Async {
		// The turn outlives the request, so it runs under the server context
		// and is cancelled by Shutdown rather than by the client hanging up.
		started, err := s.startTurn(s.baseCtx, sessionID, req)
		if err != nil {
			writeError(w, statusForTurnError(err), codeForTurnError(err), err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"session_id": started, "accepted": true})
		return
	}

	turn, err := s.runTurn(r.Context(), sessionID, req)
	if err != nil {
		writeError(w, statusForTurnError(err), codeForTurnError(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, turn)
}

// errTurnBusy reports a second turn attempted on a session that is still
// working. Interleaving two turns on one transcript corrupts it.
var errTurnBusy = errors.New("this session is already running a turn; cancel it or wait for it to finish")

func statusForTurnError(err error) int {
	switch {
	case errors.Is(err, errTurnBusy):
		return http.StatusConflict
	case errors.Is(err, context.Canceled):
		return http.StatusRequestTimeout
	default:
		return http.StatusInternalServerError
	}
}

func codeForTurnError(err error) string {
	if errors.Is(err, errTurnBusy) {
		return codeConflict
	}
	return codeInternal
}

// resolveSession returns the session to use, creating one when neither the
// request nor the server has selected any.
func (s *Server) resolveSession(ctx context.Context, requested string) (string, error) {
	if id := strings.TrimSpace(requested); id != "" {
		s.setCurrentSession(id)
		return id, nil
	}
	if id := s.CurrentSession(); id != "" {
		return id, nil
	}
	projectPath := ""
	if s.app.Workspace != nil {
		projectPath = s.app.Workspace.Root()
	}
	sess, err := s.app.Sessions.Create(ctx, session.CreateOptions{
		ProjectPath: projectPath,
		Provider:    s.cfg.Provider,
		Model:       s.cfg.Model,
	})
	if err != nil {
		return "", fmt.Errorf("cannot start a session: %w", err)
	}
	s.setCurrentSession(sess.ID)
	s.app.Bus.Publish(app.Event{Type: app.EventSessionStarted, SessionID: sess.ID, Payload: sess})
	return sess.ID, nil
}

// startTurn runs a turn in the background, reporting only whether it started.
func (s *Server) startTurn(ctx context.Context, sessionID string, req messageRequest) (string, error) {
	turnCtx, cancel := context.WithCancel(ctx)
	if !s.beginTurn(sessionID, cancel) {
		cancel()
		return "", errTurnBusy
	}
	go func() {
		defer cancel()
		defer s.endTurn(sessionID)
		if _, err := s.execTurn(turnCtx, sessionID, req); err != nil {
			s.publishTurnError(sessionID, err)
		}
	}()
	return sessionID, nil
}

// runTurn runs a turn and waits for it.
func (s *Server) runTurn(ctx context.Context, sessionID string, req messageRequest) (*turnResponse, error) {
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if !s.beginTurn(sessionID, cancel) {
		return nil, errTurnBusy
	}
	defer s.endTurn(sessionID)
	return s.execTurn(turnCtx, sessionID, req)
}

// execTurn is the shared body of a user turn: record the prompt, assemble a
// bounded context, run the loop, and persist what came back.
func (s *Server) execTurn(ctx context.Context, sessionID string, req messageRequest) (*turnResponse, error) {
	userMsg := provider.Message{Role: provider.RoleUser, Content: req.Content}

	history, err := s.app.Sessions.History().Messages(ctx, sessionID, session.TranscriptOptions{
		Limit:  historyWindow,
		Newest: true,
	})
	if err != nil {
		return nil, fmt.Errorf("cannot read the transcript: %w", err)
	}
	if _, err := s.app.Sessions.AppendMessage(ctx, sessionID, userMsg); err != nil {
		return nil, fmt.Errorf("cannot record the prompt: %w", err)
	}

	selProvider := firstNonEmpty(req.Provider, s.cfg.Provider)
	selModel := firstNonEmpty(req.Model, s.cfg.Model)

	messages := make([]provider.Message, 0, len(history)+2)
	messages = append(messages, provider.Message{
		Role:    provider.RoleSystem,
		Content: s.systemPrompt(selProvider, selModel),
	})
	messages = append(messages, history...)
	messages = append(messages, userMsg)

	s.app.Bus.Publish(app.Event{Type: app.EventPromptReceived, SessionID: sessionID, Payload: req.Content})

	loop := s.app.NewLoop(sessionID)
	loop.Selection = provider.Selection{Provider: selProvider, Model: selModel}
	turn, err := loop.Run(ctx, messages)
	if err != nil {
		return nil, err
	}

	for _, msg := range turn.Messages {
		if _, err := s.app.Sessions.AppendMessage(ctx, sessionID, msg); err != nil {
			s.log.Printf("web: cannot record a message for session %s: %v", sessionID, err)
			break
		}
	}
	if turn.Usage.TotalTokens > 0 {
		if err := s.app.Sessions.RecordUsage(ctx, sessionID, session.UsageEntry{
			Provider: turn.Decision.Target.Provider,
			Model:    turn.Decision.Target.Model,
			Usage:    turn.Usage,
		}); err != nil {
			s.log.Printf("web: cannot record usage for session %s: %v", sessionID, err)
		}
	}

	return &turnResponse{
		SessionID:      sessionID,
		Text:           turn.Text,
		Messages:       turn.Messages,
		Usage:          turn.Usage,
		ToolCalls:      turn.ToolCalls,
		Iterations:     turn.Iterations,
		StoppedAtLimit: turn.StoppedAtLimit,
		Provider:       turn.Decision.Target.Provider,
		Model:          turn.Decision.Target.Model,
	}, nil
}

// publishTurnError reports a background failure on the bus, because an async
// caller has no response to read it from.
func (s *Server) publishTurnError(sessionID string, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	s.app.Bus.Publish(app.Event{
		Type:      app.EventError,
		SessionID: sessionID,
		Payload:   map[string]string{"error": err.Error()},
	})
}

// systemPrompt renders the runtime context onto the base prompt, matching what
// the plain CLI sends so the two frontends behave identically (§2.3).
func (s *Server) systemPrompt(providerName, model string) string {
	var memory string
	if s.app.Memory != nil {
		memory = string(s.app.Memory.Render())
	}
	root := ""
	if s.app.Workspace != nil {
		root = s.app.Workspace.Root()
	}
	return app.PromptContext{
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		Shell:       os.Getenv("SHELL"),
		WorkingDir:  root,
		Provider:    providerName,
		Model:       model,
		Mode:        string(s.cfg.Execution.Mode),
		Tools:       s.app.Tools.Names(),
		NetworkOn:   s.cfg.Network.Enabled,
		ProjectInfo: memory,
	}.Render(s.app.SystemPrompt())
}

// firstNonEmpty returns the first non-blank string.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
