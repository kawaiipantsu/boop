// Package ollama adapts an Ollama server to the Boop provider contract.
//
// Ollama speaks the OpenAI dialect under /v1, so chat, streaming, tool calls
// and embeddings are handled entirely by internal/provider/openaicompat as
// PROJECT.md §7 requires. This package adds only what the OpenAI surface
// cannot express:
//
//   - GET /api/tags, which reports a per-model capability list and the model's
//     real context window. That makes capability detection evidence-based
//     rather than a guess from the model id, which is what §8 asks for.
//   - POST /api/show, which describes a single model that /api/tags covered
//     sparsely or not at all.
//   - GET /api/version, the cheapest available health probe.
//   - POST /api/generate with keep_alive, Ollama's idiomatic load/unload.
//
// Every endpoint and response shape encoded here was verified against a live
// Ollama 0.31.2 server.
package ollama

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/boop-dev/boop/internal/provider"
	"github.com/boop-dev/boop/internal/provider/openaicompat"
)

const (
	// ProviderName is the stable identifier used in config, routing and errors.
	ProviderName = "ollama"
	// DefaultBaseURL is Ollama's default listen address. Loopback rather than
	// localhost so a broken IPv6 stack cannot stall the first request.
	DefaultBaseURL = "http://127.0.0.1:11434"
	// OpenAIPath is the sub-path under which Ollama serves the OpenAI dialect.
	OpenAIPath = "/v1"
	// DefaultKeepAlive matches Ollama's own default residency for a loaded
	// model, so LoadModel does not silently change server behaviour.
	DefaultKeepAlive = 5 * time.Minute
)

// Native endpoint paths. These hang off the server root, not off OpenAIPath.
const (
	tagsPath     = "/api/tags"
	showPath     = "/api/show"
	versionPath  = "/api/version"
	generatePath = "/api/generate"
)

// Option customizes a Client.
type Option func(*settings)

// settings holds the resolved constructor options.
type settings struct {
	name      string
	apiKey    string
	headers   map[string]string
	timeout   time.Duration
	keepAlive time.Duration
}

// WithName overrides the provider name, which matters when a user configures
// two Ollama hosts and needs to tell their errors and stats apart.
func WithName(name string) Option {
	return func(s *settings) {
		if n := strings.TrimSpace(name); n != "" {
			s.name = n
		}
	}
}

// WithTimeout bounds non-streaming requests and the response-header wait of
// streaming ones. Token generation is never bounded by it: a slow local model
// is not a failure.
func WithTimeout(d time.Duration) Option {
	return func(s *settings) {
		if d > 0 {
			s.timeout = d
		}
	}
}

// WithHeaders adds request headers, for deployments that sit behind a reverse
// proxy expecting extra metadata.
func WithHeaders(h map[string]string) Option {
	return func(s *settings) {
		if len(h) == 0 {
			return
		}
		s.headers = make(map[string]string, len(h))
		for k, v := range h {
			s.headers[k] = v
		}
	}
}

// WithAPIKey sets a bearer token. Ollama itself requires no credentials; this
// exists only for installations fronted by an authenticating proxy.
func WithAPIKey(key string) Option {
	return func(s *settings) { s.apiKey = strings.TrimSpace(key) }
}

// WithKeepAlive sets how long LoadModel asks Ollama to keep the weights
// resident. A non-positive value would mean "unload immediately", so it is
// ignored here; UnloadModel is the way to express that.
func WithKeepAlive(d time.Duration) Option {
	return func(s *settings) {
		if d > 0 {
			s.keepAlive = d
		}
	}
}

// Client is the Ollama provider.
//
// It embeds *openaicompat.Client, so every method not defined here — Chat,
// Embed, and the raw JSON helpers — is the shared OpenAI-compatible
// implementation. Overrides exist only where a native endpoint gives a
// materially better answer.
//
// Client is safe for concurrent use.
type Client struct {
	*openaicompat.Client

	// root is the server root without the /v1 suffix, e.g.
	// http://127.0.0.1:11434. Native endpoints are addressed from it.
	root      string
	keepAlive time.Duration

	// mu guards the /api/tags cache, which is the authoritative capability
	// source and is consulted from RefineCapabilities on the shared path.
	mu   sync.RWMutex
	tags map[string]tagModel
}

// Compile-time proof of the contracts this adapter fulfils.
var (
	_ provider.Provider               = (*Client)(nil)
	_ provider.ModelLifecycleProvider = (*Client)(nil)
	_ provider.EmbeddingProvider      = (*Client)(nil)
)

// New builds a Client for the Ollama server at baseURL.
//
// An empty baseURL selects DefaultBaseURL. A nil httpClient selects the shared
// client's default transport, which is tuned so that streaming responses are
// not aborted mid-generation.
func New(baseURL string, httpClient *http.Client, opts ...Option) *Client {
	s := settings{name: ProviderName, keepAlive: DefaultKeepAlive}
	for _, opt := range opts {
		if opt != nil {
			opt(&s)
		}
	}

	c := &Client{
		root:      normalizeBaseURL(baseURL),
		keepAlive: s.keepAlive,
		tags:      make(map[string]tagModel),
	}
	c.Client = openaicompat.New(openaicompat.Options{
		Name:               s.name,
		BaseURL:            c.root + OpenAIPath,
		APIKey:             s.apiKey,
		Headers:            s.headers,
		Timeout:            s.timeout,
		HTTPClient:         httpClient,
		RefineCapabilities: c.refineCapabilities,
	})
	return c
}

// normalizeBaseURL applies the default and tolerates the two URLs a user is
// likely to paste: the server root and the OpenAI-compatible sub-path. A bare
// host:port is assumed to be plain HTTP, which is what Ollama serves.
func normalizeBaseURL(raw string) string {
	u := strings.TrimRight(strings.TrimSpace(raw), "/")
	if u == "" {
		return DefaultBaseURL
	}
	if !strings.Contains(u, "://") {
		u = "http://" + u
	}
	u = strings.TrimRight(u, "/")
	if strings.HasSuffix(u, OpenAIPath) {
		u = strings.TrimRight(strings.TrimSuffix(u, OpenAIPath), "/")
	}
	return u
}

// Root reports the server root URL, without the OpenAI-compatible suffix.
func (c *Client) Root() string { return c.root }

// native resolves a root-relative endpoint to an absolute URL.
//
// openaicompat.GetJSON and PostJSON accept absolute URLs precisely so an
// adapter can escape the versioned base it was configured with.
func (c *Client) native(path string) string { return c.root + path }

// Version reports the running Ollama version.
func (c *Client) Version(ctx context.Context) (string, error) {
	var out struct {
		Version string `json:"version"`
	}
	if err := c.GetJSON(ctx, c.native(versionPath), &out); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.Version), nil
}

// Health reports whether the server is usable.
//
// GET /api/version is tried first because it is the cheapest call Ollama
// offers and it identifies the server as Ollama. When it answers with
// something unrecognizable — an old build, or an unrelated service on the port
// — the OpenAI model listing is used as a second opinion, so an unusual
// deployment degrades instead of being declared dead.
func (c *Client) Health(ctx context.Context) error {
	version, err := c.Version(ctx)
	if err == nil && version != "" {
		return nil
	}
	if err != nil {
		if cat, ok := provider.CategoryOf(err); ok {
			switch cat {
			case provider.ErrUnavailable, provider.ErrTimeout, provider.ErrCancelled:
				// The server is not answering at all; a second probe would
				// only double the wait before reporting the same thing.
				return c.unreachable(err)
			}
		}
	}
	if fallbackErr := c.Client.Health(ctx); fallbackErr != nil {
		return c.unreachable(fallbackErr)
	}
	return nil
}

// unreachable rewrites a connection failure into the answer the user actually
// needs.
//
// Connection-refused against a loopback port practically always means the
// server is not running, and §57 asks for useful errors rather than transport
// internals, so the message says so and names the address.
func (c *Client) unreachable(err error) error {
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Category != provider.ErrUnavailable {
		return err
	}
	out := *pe
	out.Message = fmt.Sprintf("cannot reach Ollama at %s — is the Ollama server running? (start it with `ollama serve`)", c.root)
	return &out
}

// errorf builds a normalized provider error attributed to this adapter.
func (c *Client) errorf(category provider.ErrorCategory, model, message, detail string) *provider.Error {
	return &provider.Error{
		Category: category,
		Provider: c.Name(),
		Model:    model,
		Message:  message,
		Detail:   detail,
	}
}
