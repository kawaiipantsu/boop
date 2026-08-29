package fixtures

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"
)

// TB is the subset of *testing.T (and *testing.B) the harness needs.
//
// Depending on an interface rather than on *testing.T keeps the testing
// package out of the harness's import graph and lets non-test drivers (a
// manual smoke binary, a fuzz target) reuse the server.
type TB interface {
	Helper()
	Cleanup(func())
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// FixedTime is the timestamp stamped onto every generated payload.
//
// Deterministic timestamps mean golden-output comparisons stay stable.
var FixedTime = time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)

// DefaultSystemFingerprint mirrors the value Ollama reports; adapters must
// tolerate it being present and must not depend on its content.
const DefaultSystemFingerprint = "fp_boop_fixture"

// CapturedRequest is one request the harness received, recorded in full so
// tests can assert exactly what an adapter sent.
type CapturedRequest struct {
	Method     string
	Path       string
	Query      url.Values
	Header     http.Header
	Body       []byte
	ReceivedAt time.Time
}

// JSON decodes the captured body into v.
func (c CapturedRequest) JSON(v any) error {
	if err := json.Unmarshal(c.Body, v); err != nil {
		return fmt.Errorf("fixtures: decode %s %s body: %w", c.Method, c.Path, err)
	}
	return nil
}

// BearerToken returns the token from an Authorization: Bearer header, or "".
func (c CapturedRequest) BearerToken() string {
	return strings.TrimSpace(strings.TrimPrefix(c.Header.Get("Authorization"), "Bearer "))
}

// String renders a short identification useful in failure messages.
func (c CapturedRequest) String() string {
	return fmt.Sprintf("%s %s (%d bytes)", c.Method, c.Path, len(c.Body))
}

// Option configures a [Server] at construction time.
type Option func(*Server)

// WithModels replaces the advertised model catalogue.
func WithModels(models ...ModelInfo) Option {
	return func(s *Server) { s.models = models }
}

// WithDefaultResponse sets what the server replies with once the scripted
// queue is drained. Passing nil makes an unscripted request a test failure,
// which is the strictest and often most useful setting.
func WithDefaultResponse(r *Response) Option {
	return func(s *Server) { s.defaultResp = r }
}

// WithAPIKey makes the server reject requests that do not present the given
// dummy token as either "Authorization: Bearer <key>" or "x-api-key: <key>",
// so adapters can prove they authenticate. Never pass a real credential.
func WithAPIKey(key string) Option {
	return func(s *Server) { s.apiKey = key }
}

// WithLatency delays every request by d, on top of any per-response delay.
func WithLatency(d time.Duration) Option {
	return func(s *Server) { s.latency = d }
}

// WithSystemFingerprint sets the system_fingerprint reported on
// OpenAI-compatible payloads.
func WithSystemFingerprint(fp string) Option {
	return func(s *Server) { s.fingerprint = fp }
}

// WithStrictStreamUsage makes streamed usage conditional on the client having
// sent stream_options.include_usage, as api.openai.com requires. By default
// the harness always reports scripted usage, matching local servers.
func WithStrictStreamUsage() Option {
	return func(s *Server) { s.strictUsage = true }
}

// pathFailure is a sticky, endpoint-scoped failure injection.
type pathFailure struct {
	status int
	body   string
}

// Server is the scriptable fake model server.
//
// It is safe for concurrent use: the adapter under test may issue parallel
// requests while the test inspects captures.
type Server struct {
	t   TB
	mux *http.ServeMux
	srv *httptest.Server

	mu           sync.Mutex
	models       []ModelInfo
	queue        []*Response
	defaultResp  *Response
	requests     []CapturedRequest
	pathFailures map[string]pathFailure
	custom       map[string]http.HandlerFunc
	latency      time.Duration
	seq          int

	apiKey      string
	fingerprint string
	strictUsage bool
	closeOnce   sync.Once
}

// NewServer starts a fake provider server and registers its shutdown with the
// test's cleanup, so callers never have to close it.
func NewServer(t TB, opts ...Option) *Server {
	t.Helper()
	s := &Server{
		t:            t,
		models:       DefaultModels(),
		pathFailures: map[string]pathFailure{},
		custom:       map[string]http.HandlerFunc{},
		fingerprint:  DefaultSystemFingerprint,
	}
	s.defaultResp = DefaultResponse()
	for _, opt := range opts {
		opt(s)
	}
	s.mux = http.NewServeMux()
	s.routes()
	s.srv = httptest.NewServer(s)
	t.Cleanup(s.Close)
	return s
}

// URL is the base URL of the server, without a trailing slash, e.g.
// "http://127.0.0.1:39211". Adapters normally take exactly this as base_url.
func (s *Server) URL() string { return s.srv.URL }

// OpenAIBaseURL is the OpenAI-compatible root, i.e. URL()+"/v1".
func (s *Server) OpenAIBaseURL() string { return s.srv.URL + "/v1" }

// LemonadeBaseURL is the /api/v1 root Lemonade serves its OpenAI-compatible
// surface from.
func (s *Server) LemonadeBaseURL() string { return s.srv.URL + "/api/v1" }

// Client returns an HTTP client wired to the server. It is the plain
// httptest client; tests wanting timeouts should set them on a copy.
func (s *Server) Client() *http.Client { return s.srv.Client() }

// Close shuts the server down. It is idempotent and called automatically.
func (s *Server) Close() {
	s.closeOnce.Do(func() { s.srv.Close() })
}

// Enqueue appends scripted responses, consumed one per chat request in order.
//
// The queue is shared by every chat endpoint (OpenAI-compatible and
// Anthropic), so a single script can drive whichever adapter is under test.
func (s *Server) Enqueue(responses ...*Response) *Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = append(s.queue, responses...)
	return s
}

// EnqueueText is shorthand for Enqueue(TextResponse(text)).
func (s *Server) EnqueueText(text string) *Server { return s.Enqueue(TextResponse(text)) }

// QueueLen reports how many scripted responses remain unconsumed. Tests should
// normally assert it is zero at the end of a scripted exchange.
func (s *Server) QueueLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

// SetModels replaces the advertised catalogue after construction.
func (s *Server) SetModels(models ...ModelInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.models = models
}

// SetLatency delays every subsequent request by d.
func (s *Server) SetLatency(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latency = d
}

// SetPathFailure makes every subsequent request to path fail with the given
// status and body until [Server.ClearPathFailure]. Use it for endpoints that
// the response queue does not cover, such as /v1/models or /api/tags.
func (s *Server) SetPathFailure(path string, status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pathFailures[path] = pathFailure{status: status, body: body}
}

// ClearPathFailure removes an injected endpoint failure.
func (s *Server) ClearPathFailure(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pathFailures, path)
}

// SetHandler installs or overrides the handler for an exact path, the escape
// hatch for endpoints the harness does not model.
func (s *Server) SetHandler(path string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.custom[path] = h
}

// Requests returns a copy of every request received, in arrival order.
func (s *Server) Requests() []CapturedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]CapturedRequest(nil), s.requests...)
}

// RequestCount reports how many requests have been received.
func (s *Server) RequestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// LastRequest returns the most recent request, ok=false if there is none.
func (s *Server) LastRequest() (CapturedRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		return CapturedRequest{}, false
	}
	return s.requests[len(s.requests)-1], true
}

// RequestsTo returns every captured request for an exact path.
func (s *Server) RequestsTo(path string) []CapturedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []CapturedRequest
	for _, r := range s.requests {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// Reset clears the queue, the captures and any injected endpoint failures,
// leaving the model catalogue alone. Useful between subtests.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = nil
	s.requests = nil
	s.pathFailures = map[string]pathFailure{}
	s.latency = 0
	s.seq = 0
}

// ServeHTTP applies capture, global latency, injected endpoint failures and
// authentication before dispatching, so those behaviours hold for every
// endpoint including custom ones.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	// Replay the captured body so handlers can still decode it.
	r.Body = io.NopCloser(bytes.NewReader(body))

	s.mu.Lock()
	s.requests = append(s.requests, CapturedRequest{
		Method:     r.Method,
		Path:       r.URL.Path,
		Query:      r.URL.Query(),
		Header:     r.Header.Clone(),
		Body:       body,
		ReceivedAt: time.Now(),
	})
	latency := s.latency
	failure, failed := s.pathFailures[r.URL.Path]
	custom := s.custom[r.URL.Path]
	apiKey := s.apiKey
	s.mu.Unlock()

	if latency > 0 {
		time.Sleep(latency)
	}
	if failed {
		writeRaw(w, failure.status, "application/json", failure.body)
		return
	}
	if apiKey != "" && !authorized(r, apiKey) {
		s.writeAuthError(w, r)
		return
	}
	if custom != nil {
		custom(w, r)
		return
	}
	s.mux.ServeHTTP(w, r)
}

// authorized accepts either the OpenAI bearer scheme or the Anthropic
// x-api-key header, since one server stands in for both.
func authorized(r *http.Request, key string) bool {
	if strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) == key {
		return true
	}
	return r.Header.Get("x-api-key") == key
}

// writeAuthError returns a 401 shaped like the vendor the path belongs to.
func (s *Server) writeAuthError(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == anthropicMessagesPath {
		writeJSON(w, http.StatusUnauthorized, anthropicErrorBody("authentication_error", "invalid x-api-key"))
		return
	}
	writeJSON(w, http.StatusUnauthorized, openAIErrorBody("invalid_api_key", "Incorrect API key provided"))
}

// routes registers the emulated vendor surfaces.
func (s *Server) routes() {
	// OpenAI-compatible, mounted twice: /v1 (OpenAI, LM Studio, Ollama) and
	// /api/v1 (Lemonade).
	for _, prefix := range []string{"/v1", "/api/v1"} {
		s.mux.HandleFunc("GET "+prefix+"/models", s.handleOpenAIModels)
		s.mux.HandleFunc("POST "+prefix+"/chat/completions", s.handleChatCompletions)
	}
	// Anthropic.
	s.mux.HandleFunc("POST "+anthropicMessagesPath, s.handleAnthropicMessages)
	// Ollama native.
	s.mux.HandleFunc("GET /api/tags", s.handleOllamaTags)
	s.mux.HandleFunc("POST /api/show", s.handleOllamaShow)
	// LM Studio native.
	s.mux.HandleFunc("GET /api/v0/models", s.handleLMStudioModels)
	// Lemonade model lifecycle.
	s.mux.HandleFunc("POST /api/v1/load", s.handleLemonadeLifecycle)
	s.mux.HandleFunc("POST /api/v1/unload", s.handleLemonadeLifecycle)
	s.mux.HandleFunc("POST /api/v1/pull", s.handleLemonadeLifecycle)
	// Health probes.
	for _, p := range []string{"/health", "/api/health", "/api/v1/health"} {
		s.mux.HandleFunc("GET "+p, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
		})
	}
	s.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		writeRaw(w, http.StatusOK, "text/plain; charset=utf-8", "Ollama is running")
	})
}

// nextResponse pops the next scripted response, falling back to the default.
func (s *Server) nextResponse() *Response {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) > 0 {
		r := s.queue[0]
		s.queue = s.queue[1:]
		if r == nil {
			return &Response{}
		}
		return r
	}
	if s.defaultResp == nil {
		s.t.Errorf("fixtures: chat request arrived with an empty response queue and no default response")
		return ErrorResponse(http.StatusInternalServerError, "fixtures: unscripted request")
	}
	return s.defaultResp
}

// nextSeq yields a monotonic counter used to build deterministic ids.
func (s *Server) nextSeq() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return s.seq
}

// catalogue snapshots the advertised models.
func (s *Server) catalogue() []ModelInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ModelInfo(nil), s.models...)
}

// fingerprintFor resolves the effective system_fingerprint for a response.
func (s *Server) fingerprintFor(r *Response) string {
	if r.SystemFingerprint != "" {
		return r.SystemFingerprint
	}
	return s.fingerprint
}

// writeJSON writes v as a JSON body with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}

// writeRaw writes a verbatim body, used for malformed-response injection.
func writeRaw(w http.ResponseWriter, status int, contentType, body string) {
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	io.WriteString(w, body)
}

// applyHeaders copies scripted headers onto the response.
func applyHeaders(w http.ResponseWriter, r *Response) {
	for k, vs := range r.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
}
