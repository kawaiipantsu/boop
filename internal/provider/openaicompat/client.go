// Package openaicompat implements the generic OpenAI-compatible provider path.
//
// Every backend that speaks the OpenAI REST dialect — Lemonade, LM Studio,
// Ollama's /v1 surface, OpenAI itself, xAI and most local inference servers —
// is built on this package. Vendor adapters embed or wrap *Client and use the
// Options hooks (RefineCapabilities, RefineModels) plus the raw GetJSON and
// PostJSON escape hatches instead of re-implementing HTTP, SSE parsing and
// error normalization.
//
// The package deliberately exposes no vendor-specific behaviour of its own: it
// is the shared substrate, and anything that only one backend needs belongs in
// that backend's adapter.
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/boop-dev/boop/internal/provider"
)

// Defaults applied when the corresponding Options field is left zero.
const (
	// DefaultName is reported by Name when Options.Name is empty.
	DefaultName = "openai-compat"
	// DefaultTimeout bounds non-streaming requests and, for streaming
	// requests, the wait for response headers. Token generation itself is
	// never bounded by it, because a slow local model is not a failure.
	DefaultTimeout = 120 * time.Second
	// DefaultModelsPath is the OpenAI model listing endpoint.
	DefaultModelsPath = "/models"
	// DefaultChatPath is the OpenAI chat completion endpoint.
	DefaultChatPath = "/chat/completions"
	// DefaultEmbeddingsPath is the OpenAI embedding endpoint.
	DefaultEmbeddingsPath = "/embeddings"
)

// redactedPlaceholder replaces any credential material found in user-visible
// strings. See Client.redact.
const redactedPlaceholder = "[REDACTED]"

// Options configures a Client.
//
// The zero value is usable except for BaseURL, which is required.
type Options struct {
	// Name is the provider name reported by Name and embedded in errors.
	Name string
	// BaseURL is the API root, e.g. http://127.0.0.1:1234/v1.
	BaseURL string
	// APIKey is optional; when set it is sent as an Authorization bearer
	// token. It is never echoed into errors, logs or String output.
	APIKey string
	// Headers are extra request headers. They are applied last and may
	// therefore override the defaults, including Authorization.
	Headers map[string]string
	// Timeout bounds non-streaming requests and the response-header wait of
	// streaming requests. Zero means DefaultTimeout.
	Timeout time.Duration
	// HTTPClient overrides the internally constructed client. When set,
	// Timeout is not applied to it; the caller owns its transport policy.
	HTTPClient *http.Client
	// ModelsPath defaults to DefaultModelsPath.
	ModelsPath string
	// ChatPath defaults to DefaultChatPath.
	ChatPath string
	// EmbeddingsPath defaults to DefaultEmbeddingsPath.
	EmbeddingsPath string
	// RefineCapabilities lets a vendor adapter adjust detected capabilities.
	// It receives the model id and the capabilities derived from the model
	// id and any server metadata, and returns the final set.
	RefineCapabilities func(model string, base provider.Capabilities) provider.Capabilities
	// RefineModels lets a vendor adapter post-process the model list, for
	// example to add display names or drop non-chat entries.
	RefineModels func([]provider.Model) []provider.Model
}

// Client is a generic OpenAI-compatible provider.
//
// It implements provider.Provider and provider.EmbeddingProvider and is safe
// for concurrent use.
type Client struct {
	name           string
	baseURL        string
	apiKey         string
	headers        map[string]string
	timeout        time.Duration
	http           *http.Client
	modelsPath     string
	chatPath       string
	embeddingsPath string

	refineCaps   func(string, provider.Capabilities) provider.Capabilities
	refineModels func([]provider.Model) []provider.Model

	// mu guards the capability and metadata caches, which are populated
	// lazily from ListModels and from per-model capability probes.
	mu       sync.RWMutex
	capsByID map[string]provider.Capabilities
	metaByID map[string]modelMeta
}

// Compile-time proof that Client satisfies the contracts other packages
// program against.
var (
	_ provider.Provider          = (*Client)(nil)
	_ provider.EmbeddingProvider = (*Client)(nil)
	_ fmt.Stringer               = (*Client)(nil)
)

// New builds a Client from opts, applying defaults for every unset field.
func New(opts Options) *Client {
	c := &Client{
		name:           firstNonEmpty(opts.Name, DefaultName),
		baseURL:        strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/"),
		apiKey:         strings.TrimSpace(opts.APIKey),
		timeout:        opts.Timeout,
		http:           opts.HTTPClient,
		modelsPath:     firstNonEmpty(opts.ModelsPath, DefaultModelsPath),
		chatPath:       firstNonEmpty(opts.ChatPath, DefaultChatPath),
		embeddingsPath: firstNonEmpty(opts.EmbeddingsPath, DefaultEmbeddingsPath),
		refineCaps:     opts.RefineCapabilities,
		refineModels:   opts.RefineModels,
		capsByID:       make(map[string]provider.Capabilities),
		metaByID:       make(map[string]modelMeta),
	}
	if c.timeout <= 0 {
		c.timeout = DefaultTimeout
	}
	if len(opts.Headers) > 0 {
		c.headers = make(map[string]string, len(opts.Headers))
		for k, v := range opts.Headers {
			c.headers[k] = v
		}
	}
	if c.http == nil {
		c.http = newHTTPClient(c.timeout)
	}
	return c
}

// newHTTPClient builds the default transport.
//
// The client deliberately has no overall Timeout: that would abort long
// streaming responses mid-generation. ResponseHeaderTimeout bounds the part
// that genuinely indicates a dead server, and per-call context deadlines cover
// the non-streaming paths.
func newHTTPClient(timeout time.Duration) *http.Client {
	dialTimeout := 10 * time.Second
	if timeout < dialTimeout {
		dialTimeout = timeout
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          32,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
			ResponseHeaderTimeout: timeout,
		},
	}
}

// Name reports the configured provider name.
func (c *Client) Name() string { return c.name }

// BaseURL reports the normalized API root, without a trailing slash.
func (c *Client) BaseURL() string { return c.baseURL }

// String renders the client for diagnostics. It never includes credentials.
func (c *Client) String() string {
	return fmt.Sprintf("openaicompat.Client{name:%s base:%s auth:%s}", c.name, c.baseURL, authState(c.apiKey))
}

func authState(key string) string {
	if key == "" {
		return "none"
	}
	return "bearer(redacted)"
}

// Health probes the backend with the cheapest call the OpenAI dialect offers,
// a model listing, and reports a normalized error when it is unreachable.
func (c *Client) Health(ctx context.Context) error {
	_, err := c.rawListModels(ctx)
	return err
}

// GetJSON performs a GET against path and decodes the JSON response into out.
//
// It exists so vendor adapters can reach native endpoints (Ollama's /api/tags,
// Lemonade's /api/v1/system-info) while reusing this client's HTTP client,
// headers, authentication and error normalization. Pass nil for out to discard
// the body.
//
// path is joined to BaseURL unless it is already an absolute http(s) URL, which
// lets adapters escape a versioned base such as .../v1.
func (c *Client) GetJSON(ctx context.Context, path string, out any) error {
	return c.doJSON(ctx, http.MethodGet, path, nil, out)
}

// PostJSON performs a POST of body (JSON-encoded, or no body when nil) against
// path and decodes the JSON response into out. See GetJSON for path semantics.
func (c *Client) PostJSON(ctx context.Context, path string, body, out any) error {
	return c.doJSON(ctx, http.MethodPost, path, body, out)
}

// endpoint resolves path against the configured base URL.
func (c *Client) endpoint(path string) string {
	p := strings.TrimSpace(path)
	if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
		return p
	}
	if p == "" {
		return c.baseURL
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return c.baseURL + p
}

// newRequest builds an authenticated JSON request.
func (c *Client) newRequest(ctx context.Context, method, path string, body []byte, accept string) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(path), rdr)
	if err != nil {
		return nil, c.wrapError(ctx, provider.ErrInvalidRequest, "", "invalid request URL", err)
	}
	req.Header.Set("Accept", accept)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// withTimeout applies the configured timeout to ctx unless the caller already
// set a deadline. Streaming calls skip this: their bound is the transport's
// ResponseHeaderTimeout.
func (c *Client) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.timeout)
}

// doJSON runs a complete non-streaming JSON round trip.
func (c *Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return c.wrapError(ctx, provider.ErrInvalidRequest, "", "could not encode request body", err)
		}
	}

	reqCtx, cancel := c.withTimeout(ctx)
	defer cancel()

	req, err := c.newRequest(reqCtx, method, path, payload, "application/json")
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return c.transportError(reqCtx, ctx, "", err)
	}
	defer drainAndClose(resp.Body)

	raw, readErr := readLimited(resp.Body, maxResponseBytes)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return c.statusError(resp.StatusCode, "", raw)
	}
	if readErr != nil {
		return c.transportError(reqCtx, ctx, "", readErr)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return c.malformedError("", raw, err)
	}
	return nil
}

// readLimited reads at most n bytes, so a hostile or broken server cannot
// exhaust memory through an unbounded body.
func readLimited(r io.Reader, n int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, n))
}

// drainAndClose releases a response body so the connection can be reused.
func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 1<<16))
	_ = body.Close()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// errIsAny reports whether err matches any of the sentinel targets.
func errIsAny(err error, targets ...error) bool {
	for _, t := range targets {
		if errors.Is(err, t) {
			return true
		}
	}
	return false
}
