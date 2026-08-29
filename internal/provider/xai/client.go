// Package xai adapts the xAI (Grok) API to the Boop provider contract.
//
// xAI serves the OpenAI REST dialect, including the same error envelope and
// the same SSE chunk format, so this adapter is a configuration of
// internal/provider/openaicompat plus one thing that is genuinely
// vendor-specific: which Grok model can do what. Everything else — HTTP,
// streaming, tool-call assembly, error normalization, redaction — is reused.
package xai

import (
	"net/http"
	"strings"
	"time"

	"github.com/kawaiipantsu/boop/internal/provider"
	"github.com/kawaiipantsu/boop/internal/provider/openaicompat"
)

const (
	// ProviderName is the name this adapter reports.
	ProviderName = "xai"
	// DefaultBaseURL is the xAI API root.
	DefaultBaseURL = "https://api.x.ai/v1"
)

// Options configures a Client.
type Options struct {
	// Name overrides the reported provider name. Defaults to ProviderName.
	Name string
	// BaseURL defaults to DefaultBaseURL.
	BaseURL string
	// APIKey is sent as an Authorization bearer token.
	APIKey string
	// Headers are extra request headers.
	Headers map[string]string
	// Timeout bounds non-streaming requests. Zero uses the shared default.
	Timeout time.Duration
	// HTTPClient overrides the internally constructed client.
	HTTPClient *http.Client
	// RefineCapabilities runs after this adapter's own family table.
	RefineCapabilities func(model string, base provider.Capabilities) provider.Capabilities
}

// Client is the xAI provider adapter.
//
// It embeds the shared OpenAI-compatible client and adds no behaviour of its
// own, so it satisfies provider.Provider and provider.EmbeddingProvider
// directly.
type Client struct {
	*openaicompat.Client
}

var _ provider.Provider = (*Client)(nil)

// New builds a Client from opts.
func New(opts Options) *Client {
	base := strings.TrimSpace(opts.BaseURL)
	if base == "" {
		base = DefaultBaseURL
	}
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		name = ProviderName
	}

	refine := refineCapabilities
	if opts.RefineCapabilities != nil {
		extra := opts.RefineCapabilities
		refine = func(model string, caps provider.Capabilities) provider.Capabilities {
			return extra(model, refineCapabilities(model, caps))
		}
	}

	return &Client{Client: openaicompat.New(openaicompat.Options{
		Name:               name,
		BaseURL:            base,
		APIKey:             opts.APIKey,
		Headers:            opts.Headers,
		Timeout:            opts.Timeout,
		HTTPClient:         opts.HTTPClient,
		RefineCapabilities: refine,
	})}
}
