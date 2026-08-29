// Package lmstudio adapts an LM Studio server to the Boop provider contract.
//
// LM Studio serves the OpenAI dialect under /v1, so chat, streaming, tool
// calls and embeddings come entirely from internal/provider/openaicompat as
// PROJECT.md §7 requires. The only native surface used here is LM Studio's
// REST API under /api/v0, which reports per-model state that the OpenAI
// listing cannot express: whether a model is currently in memory, what kind of
// model it is, and its context length.
//
// # Verification status
//
// Nothing in this package was verified against a running LM Studio server: the
// implementation was written without one available. The OpenAI-compatible path
// is the standard dialect and is exercised by the openaicompat tests. The
// /api/v0 shapes are inferred from LM Studio's documented REST API and must be
// treated as unconfirmed — which is exactly why every native call degrades to
// the OpenAI path instead of failing, and why the /api/v0 field names are read
// leniently. Each inferred detail is called out at its declaration.
package lmstudio

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
	ProviderName = "lmstudio"
	// DefaultBaseURL is LM Studio's default local server address.
	DefaultBaseURL = "http://127.0.0.1:1234"
	// OpenAIPath is the sub-path under which LM Studio serves the OpenAI
	// dialect. Verified only in the sense that it is LM Studio's documented
	// and long-standing address for it.
	OpenAIPath = "/v1"
	// restModelsPath is LM Studio's native model listing.
	//
	// INFERRED: taken from LM Studio's REST API documentation, not confirmed
	// against a live server. A 404 here is treated as "this build has no REST
	// API" and the OpenAI listing is used instead.
	restModelsPath = "/api/v0/models"
)

// Option customizes a Client.
type Option func(*settings)

type settings struct {
	name    string
	apiKey  string
	headers map[string]string
	timeout time.Duration
}

// WithName overrides the provider name, which matters when a user configures
// more than one LM Studio host and needs to tell their errors apart.
func WithName(name string) Option {
	return func(s *settings) {
		if n := strings.TrimSpace(name); n != "" {
			s.name = n
		}
	}
}

// WithTimeout bounds non-streaming requests and the response-header wait of
// streaming ones. Generation itself is never bounded by it.
func WithTimeout(d time.Duration) Option {
	return func(s *settings) {
		if d > 0 {
			s.timeout = d
		}
	}
}

// WithHeaders adds request headers, for installations behind a reverse proxy.
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

// WithAPIKey sets a bearer token. LM Studio requires no credentials; this
// exists only for installations fronted by an authenticating proxy.
func WithAPIKey(key string) Option {
	return func(s *settings) { s.apiKey = strings.TrimSpace(key) }
}

// Client is the LM Studio provider.
//
// It embeds *openaicompat.Client, so Chat, Embed and the raw JSON helpers are
// the shared implementation. Only listing, capabilities, health and model
// lifecycle are overridden, and each of those falls back to the shared
// behaviour when the native REST API is absent.
//
// Client is safe for concurrent use.
type Client struct {
	*openaicompat.Client

	// root is the server root without the /v1 suffix.
	root string

	// mu guards the native model cache and the REST-API support flag.
	mu sync.RWMutex
	// native maps model id to LM Studio's own record.
	native map[string]ModelInfo
	// restAbsent records that /api/v0 answered "not found". Only a definitive
	// answer sets it; a transport failure does not, because the server may
	// simply have been restarting.
	restAbsent bool
}

// Compile-time proof of the contracts this adapter fulfils.
var (
	_ provider.Provider               = (*Client)(nil)
	_ provider.ModelLifecycleProvider = (*Client)(nil)
	_ provider.EmbeddingProvider      = (*Client)(nil)
)

// New builds a Client for the LM Studio server at baseURL.
//
// An empty baseURL selects DefaultBaseURL. A nil httpClient selects the shared
// client's default transport.
func New(baseURL string, httpClient *http.Client, opts ...Option) *Client {
	s := settings{name: ProviderName}
	for _, opt := range opts {
		if opt != nil {
			opt(&s)
		}
	}

	c := &Client{
		root:   normalizeBaseURL(baseURL),
		native: make(map[string]ModelInfo),
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

// normalizeBaseURL applies the default and tolerates the URLs a user is likely
// to paste: the server root, the OpenAI sub-path, or a bare host:port.
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

// native resolves a root-relative endpoint to an absolute URL, escaping the
// /v1 base the shared client was configured with.
func (c *Client) nativeURL(path string) string { return c.root + path }

// Health reports whether the server is usable.
//
// The OpenAI model listing is the probe rather than /api/v0, because it is the
// one endpoint every LM Studio version serves and it is also the path chat
// uses: a health check that passes while chat cannot work would be worse than
// none.
func (c *Client) Health(ctx context.Context) error {
	if err := c.Client.Health(ctx); err != nil {
		return c.unreachable(err)
	}
	return nil
}

// unreachable rewrites a connection failure into the answer the user needs.
//
// LM Studio's server is off by default and has to be started explicitly, so
// "connection refused" on the default port overwhelmingly means it was never
// switched on — and §57 asks for the useful answer rather than the transport
// detail.
func (c *Client) unreachable(err error) error {
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Category != provider.ErrUnavailable {
		return err
	}
	out := *pe
	out.Message = fmt.Sprintf("cannot reach LM Studio at %s — is its local server running? (LM Studio → Developer → Start Server)", c.root)
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
