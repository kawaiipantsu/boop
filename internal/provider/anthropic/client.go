// Package anthropic implements the Anthropic Messages API adapter.
//
// Unlike the other cloud adapters, this one does not build on
// internal/provider/openaicompat. The Messages API is not the OpenAI dialect:
// the system prompt is a top-level field rather than a message, max_tokens is
// required, tools declare an input_schema rather than function parameters,
// tool calls and results are content blocks, and streaming is a typed event
// stream rather than a sequence of choice deltas. Forcing that through the
// OpenAI path would mean a translation layer larger and more fragile than a
// native implementation, so the wire protocol is implemented here and only the
// neutral provider contract is shared.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kawaiipantsu/boop/internal/provider"
)

// Defaults applied when the corresponding Options field is left zero.
const (
	// ProviderName is the name this adapter reports.
	ProviderName = "anthropic"
	// DefaultBaseURL is the Anthropic API root. Note that, unlike the
	// OpenAI-dialect providers, the version segment is part of the paths
	// rather than the base URL, because /v1/models and /v1/messages are
	// reached from the same root.
	DefaultBaseURL = "https://api.anthropic.com"
	// APIVersion is sent as the required anthropic-version header. The
	// Messages API has been stable at this version since its introduction;
	// bumping it is a deliberate act, not a default.
	APIVersion = "2023-06-01"
	// DefaultTimeout bounds non-streaming requests and, for streaming
	// requests, the wait for response headers. Generation time itself is
	// never bounded by it.
	DefaultTimeout = 120 * time.Second

	// DefaultMaxTokens is sent when a ChatRequest leaves MaxTokens at zero.
	//
	// The Messages API rejects a request without max_tokens, so the adapter
	// must invent a value. 4096 is chosen because it is accepted by every
	// Claude model ever published — including the older ones whose output
	// cap is exactly 4096 — so a caller that does not care about the limit
	// never turns an unset field into an HTTP 400. Deployments that want the
	// larger ceilings of current models set Options.DefaultMaxTokens, and
	// callers that know what they want set ChatRequest.MaxTokens.
	DefaultMaxTokens = 4096

	// messagesPath is the completion endpoint.
	messagesPath = "/v1/messages"
	// modelsPath is the model listing endpoint.
	modelsPath = "/v1/models"
)

// maxResponseBytes caps a non-streaming body so a broken or hostile server
// cannot exhaust memory.
const maxResponseBytes = 32 << 20

// Options configures a Client. Only APIKey is effectively required; everything
// else has a working default.
type Options struct {
	// Name overrides the reported provider name. Defaults to ProviderName.
	// It exists so a configuration can register two Anthropic endpoints (for
	// example a proxy alongside the public API) under distinct names.
	Name string
	// BaseURL defaults to DefaultBaseURL.
	BaseURL string
	// APIKey is sent as the x-api-key header. It is never echoed into
	// errors, logs or String output (§45).
	APIKey string
	// Version overrides the anthropic-version header.
	Version string
	// Beta lists anthropic-beta feature ids to send.
	Beta []string
	// Headers are extra request headers, applied last, so they may override
	// the defaults.
	Headers map[string]string
	// DefaultMaxTokens overrides the package DefaultMaxTokens for requests
	// that do not set their own.
	DefaultMaxTokens int
	// Timeout bounds non-streaming requests. Zero means DefaultTimeout.
	Timeout time.Duration
	// HTTPClient overrides the internally constructed client. When set, the
	// caller owns its transport policy and Timeout is not applied to it.
	HTTPClient *http.Client
	// RefineCapabilities lets a caller adjust detected capabilities, mainly
	// so tests and unusual deployments are not bound to the built-in family
	// table.
	RefineCapabilities func(model string, base provider.Capabilities) provider.Capabilities
}

// Client is the Anthropic provider adapter. It is safe for concurrent use.
//
// Anthropic publishes no embedding endpoint, so Client implements
// provider.Provider only; it deliberately does not pretend to be an
// EmbeddingProvider.
type Client struct {
	name       string
	baseURL    string
	apiKey     string
	version    string
	beta       string
	headers    map[string]string
	maxTokens  int
	timeout    time.Duration
	http       *http.Client
	refineCaps func(string, provider.Capabilities) provider.Capabilities

	// mu guards the lazily populated model metadata cache, which lets
	// Capabilities answer without a round trip once ListModels has run.
	mu   sync.RWMutex
	byID map[string]wireModel
	caps map[string]provider.Capabilities
}

var (
	_ provider.Provider = (*Client)(nil)
	_ fmt.Stringer      = (*Client)(nil)
)

// New builds a Client, applying defaults for every unset Options field.
func New(opts Options) *Client {
	c := &Client{
		name:       firstNonEmpty(strings.TrimSpace(opts.Name), ProviderName),
		baseURL:    strings.TrimRight(firstNonEmpty(strings.TrimSpace(opts.BaseURL), DefaultBaseURL), "/"),
		apiKey:     strings.TrimSpace(opts.APIKey),
		version:    firstNonEmpty(strings.TrimSpace(opts.Version), APIVersion),
		beta:       strings.Join(opts.Beta, ","),
		maxTokens:  opts.DefaultMaxTokens,
		timeout:    opts.Timeout,
		http:       opts.HTTPClient,
		refineCaps: opts.RefineCapabilities,
		byID:       make(map[string]wireModel),
		caps:       make(map[string]provider.Capabilities),
	}
	if c.maxTokens <= 0 {
		c.maxTokens = DefaultMaxTokens
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
// There is no overall client Timeout on purpose: it would abort a long
// streaming response mid-generation. ResponseHeaderTimeout bounds the part that
// genuinely indicates a dead endpoint.
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

// MaxTokensDefault reports the value this client substitutes when a request
// leaves ChatRequest.MaxTokens unset.
func (c *Client) MaxTokensDefault() int { return c.maxTokens }

// String renders the client for diagnostics. It never includes credentials.
func (c *Client) String() string {
	auth := "none"
	if c.apiKey != "" {
		auth = "x-api-key(redacted)"
	}
	return fmt.Sprintf("anthropic.Client{name:%s base:%s auth:%s}", c.name, c.baseURL, auth)
}

// Health probes the endpoint with the cheapest authenticated call available.
//
// A model listing both proves reachability and validates the credential, which
// matters more for a cloud provider than for a local server: an unreachable
// Ollama and an unusable API key are very different problems for the user.
func (c *Client) Health(ctx context.Context) error {
	var out modelsResponse
	return c.getJSON(ctx, modelsPath+"?limit=1", &out)
}

// newRequest builds an authenticated request.
//
// Anthropic authenticates with x-api-key rather than an Authorization bearer
// token, and rejects any request missing anthropic-version. Both are the most
// common porting mistakes, so they are set in exactly one place.
func (c *Client) newRequest(ctx context.Context, method, path string, body []byte, accept string) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return nil, c.newError(provider.ErrInvalidRequest, "", "invalid request URL", causeDetail(err), 0, err)
	}
	req.Header.Set("Accept", accept)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("anthropic-version", c.version)
	if c.apiKey != "" {
		req.Header.Set("x-api-key", c.apiKey)
	}
	if c.beta != "" {
		req.Header.Set("anthropic-beta", c.beta)
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// withTimeout applies the client timeout unless the caller set a deadline.
func (c *Client) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.timeout)
}

// getJSON performs a GET and decodes the JSON response into out.
func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	reqCtx, cancel := c.withTimeout(ctx)
	defer cancel()

	req, err := c.newRequest(reqCtx, http.MethodGet, path, nil, "application/json")
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return c.transportError(reqCtx, ctx, "", err)
	}
	defer drainAndClose(resp.Body)

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return c.statusError(resp.StatusCode, "", raw, requestFeatures{})
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
