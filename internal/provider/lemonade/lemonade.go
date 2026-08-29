// Package lemonade adapts a Lemonade Server to the Boop provider contract.
//
// Lemonade exposes an OpenAI-compatible API, so chat, streaming, tool calls,
// model listing, capability detection and embeddings all come from
// internal/provider/openaicompat exactly as PROJECT.md §7 asks. The only
// vendor-specific code here is a health probe and model load/unload, and both
// fall back to the OpenAI path when the native endpoint is not there.
//
// # Verification status
//
// This adapter was written with no Lemonade server available to test against,
// and Lemonade's management API is the least stable of the three local
// backends Boop targets. The split is therefore deliberate and explicit:
//
//   - VERIFIED BY CONSTRUCTION — everything routed through openaicompat. It is
//     the standard OpenAI dialect under the APIPath prefix and is covered by
//     the openaicompat test suite.
//   - INFERRED — the native paths in this file (health, load, unload) and
//     their request bodies. Each is marked at its declaration. They are called
//     only in ways that degrade cleanly: health falls back to the model
//     listing, and load/unload try a second body shape before giving up with a
//     normalized error naming the endpoint that was attempted.
//
// Nothing here should be read as confirmed behaviour. Run the
// BOOP_TEST_LEMONADE_URL live tests against a real server to correct it; they
// are written to report what they find rather than to assume it.
package lemonade

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/boop-dev/boop/internal/provider"
	"github.com/boop-dev/boop/internal/provider/openaicompat"
)

const (
	// ProviderName is the stable identifier used in config, routing and errors.
	ProviderName = "lemonade"
	// DefaultBaseURL is Lemonade Server's default listen address.
	DefaultBaseURL = "http://127.0.0.1:13305"
	// APIPath is the prefix under which Lemonade serves both its
	// OpenAI-compatible endpoints and its management endpoints, so the
	// OpenAI-compatible base becomes <root>/api/v1.
	APIPath = "/api/v1"
)

// Native management endpoints, relative to APIPath.
//
// INFERRED — none of these was exercised against a running server. They are
// Lemonade's documented management routes as of writing; a 404 from any of
// them is handled as "this build does not have it" rather than as a failure.
const (
	healthPath = "/health"
	loadPath   = "/load"
	unloadPath = "/unload"
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
// more than one Lemonade host and needs to tell their errors apart.
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

// WithAPIKey sets a bearer token. Lemonade requires no credentials; this
// exists only for installations fronted by an authenticating proxy.
func WithAPIKey(key string) Option {
	return func(s *settings) { s.apiKey = strings.TrimSpace(key) }
}

// Client is the Lemonade provider.
//
// It embeds *openaicompat.Client, so Chat, ListModels, Capabilities, Embed and
// the raw JSON helpers are the shared implementation and are not reimplemented
// here. Only Health and the model lifecycle are Lemonade-specific.
//
// Client is safe for concurrent use.
type Client struct {
	*openaicompat.Client

	// root is the server root without the API prefix.
	root string
}

// Compile-time proof of the contracts this adapter fulfils.
//
// EmbeddingProvider is inherited from the shared client and therefore depends
// on Lemonade serving <APIPath>/embeddings, which is INFERRED.
var (
	_ provider.Provider               = (*Client)(nil)
	_ provider.ModelLifecycleProvider = (*Client)(nil)
	_ provider.EmbeddingProvider      = (*Client)(nil)
)

// New builds a Client for the Lemonade server at baseURL.
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

	c := &Client{root: normalizeBaseURL(baseURL)}
	c.Client = openaicompat.New(openaicompat.Options{
		Name:               s.name,
		BaseURL:            c.root + APIPath,
		APIKey:             s.apiKey,
		Headers:            s.headers,
		Timeout:            s.timeout,
		HTTPClient:         httpClient,
		RefineCapabilities: refineCapabilities,
	})
	return c
}

// normalizeBaseURL applies the default and tolerates the URLs a user is likely
// to paste: the server root, the full API prefix, or a bare host:port.
func normalizeBaseURL(raw string) string {
	u := strings.TrimRight(strings.TrimSpace(raw), "/")
	if u == "" {
		return DefaultBaseURL
	}
	if !strings.Contains(u, "://") {
		u = "http://" + u
	}
	u = strings.TrimRight(u, "/")
	if strings.HasSuffix(u, APIPath) {
		u = strings.TrimRight(strings.TrimSuffix(u, APIPath), "/")
	}
	return u
}

// Root reports the server root URL, without the API prefix.
func (c *Client) Root() string { return c.root }

// apiURL resolves a path relative to the API prefix into an absolute URL.
func (c *Client) apiURL(path string) string { return c.root + APIPath + path }

// healthResponse is the body of GET <APIPath>/health.
//
// INFERRED, and read leniently on purpose: the only thing that matters is that
// the endpoint answered with JSON. Fields that a given build does not send
// simply stay empty and are reported as such.
type healthResponse struct {
	Status         string `json:"status"`
	ModelLoaded    string `json:"model_loaded"`
	CheckpointLoad string `json:"checkpoint_loaded"`
}

// Health reports whether the server is usable.
//
// The native health endpoint is tried first because it is cheap and, on builds
// that have it, tells Boop which model is resident. Because that endpoint is
// INFERRED, a reply of "not found" or an unparseable body is treated as "this
// build does not have it" and the OpenAI model listing decides instead — that
// path exists on every build and is the one chat uses.
//
// Any other native failure is reported as-is. A server that answers its own
// health check with an outage must not be declared healthy just because it can
// still list models.
func (c *Client) Health(ctx context.Context) error {
	_, err := c.ServerStatus(ctx)
	if err == nil {
		return nil
	}
	if cat, ok := provider.CategoryOf(err); !ok ||
		(cat != provider.ErrInvalidRequest && cat != provider.ErrMalformedResponse) {
		return c.unreachable(err)
	}
	if fallbackErr := c.Client.Health(ctx); fallbackErr != nil {
		return c.unreachable(fallbackErr)
	}
	return nil
}

// ServerStatus queries the native health endpoint.
//
// INFERRED endpoint. It returns the resident model name when the server
// reports one and an empty string when it does not, so an empty result means
// "the server is up and told us nothing", never "no model".
func (c *Client) ServerStatus(ctx context.Context) (loadedModel string, err error) {
	var resp healthResponse
	if err := c.GetJSON(ctx, c.apiURL(healthPath), &resp); err != nil {
		return "", err
	}
	if status := strings.ToLower(strings.TrimSpace(resp.Status)); status != "" &&
		status != "ok" && status != "healthy" && status != "alive" {
		return "", c.errorf(provider.ErrServer, "",
			fmt.Sprintf("Lemonade reported status %q", resp.Status),
			healthPath+" answered with a non-ok status")
	}
	if m := strings.TrimSpace(resp.ModelLoaded); m != "" {
		return m, nil
	}
	return strings.TrimSpace(resp.CheckpointLoad), nil
}

// unreachable rewrites a connection failure into the answer the user needs.
func (c *Client) unreachable(err error) error {
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Category != provider.ErrUnavailable {
		return err
	}
	out := *pe
	out.Message = fmt.Sprintf("cannot reach Lemonade at %s — is Lemonade Server running? (start it with `lemonade-server serve`)", c.root)
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
