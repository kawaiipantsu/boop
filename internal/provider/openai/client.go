// Package openai adapts the OpenAI API to the Boop provider contract.
//
// OpenAI speaks the dialect that internal/provider/openaicompat implements, so
// this package is deliberately thin: it configures the shared client and adds
// only the three things OpenAI genuinely needs on top of it — capability
// refinement for families the generic name heuristics get wrong, the
// organization and project headers, and a correction to the normalized error
// category for the one OpenAI failure whose HTTP status lies about it.
package openai

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/kawaiipantsu/boop/internal/provider"
	"github.com/kawaiipantsu/boop/internal/provider/openaicompat"
)

const (
	// ProviderName is the name this adapter reports.
	ProviderName = "openai"
	// DefaultBaseURL is the public API root.
	DefaultBaseURL = "https://api.openai.com/v1"
)

// Options configures a Client.
type Options struct {
	// Name overrides the reported provider name. Defaults to ProviderName.
	Name string
	// BaseURL defaults to DefaultBaseURL. It exists so an Azure-style or
	// gateway deployment can reuse this adapter.
	BaseURL string
	// APIKey is sent as an Authorization bearer token.
	APIKey string
	// Organization sets the OpenAI-Organization header. Accounts that belong
	// to more than one organization must send it or requests are billed to,
	// and rate limited by, the default organization.
	Organization string
	// Project sets the OpenAI-Project header, which scopes usage and limits
	// within an organization.
	Project string
	// Headers are extra request headers, applied after the ones above.
	Headers map[string]string
	// Timeout bounds non-streaming requests. Zero uses the shared default.
	Timeout time.Duration
	// HTTPClient overrides the internally constructed client.
	HTTPClient *http.Client
	// RefineCapabilities runs after this adapter's own family table, so a
	// deployment can correct it without forking the package.
	RefineCapabilities func(model string, base provider.Capabilities) provider.Capabilities
}

// Client is the OpenAI provider adapter.
//
// It embeds the shared OpenAI-compatible client, so it satisfies both
// provider.Provider and provider.EmbeddingProvider, and overrides only the
// methods whose errors need re-categorizing.
type Client struct {
	*openaicompat.Client
}

var (
	_ provider.Provider          = (*Client)(nil)
	_ provider.EmbeddingProvider = (*Client)(nil)
)

// New builds a Client from opts.
func New(opts Options) *Client {
	headers := make(map[string]string, len(opts.Headers)+2)
	if org := strings.TrimSpace(opts.Organization); org != "" {
		headers["OpenAI-Organization"] = org
	}
	if project := strings.TrimSpace(opts.Project); project != "" {
		headers["OpenAI-Project"] = project
	}
	for k, v := range opts.Headers {
		headers[k] = v
	}

	base := opts.BaseURL
	if strings.TrimSpace(base) == "" {
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
		Headers:            headers,
		Timeout:            opts.Timeout,
		HTTPClient:         opts.HTTPClient,
		RefineCapabilities: refine,
	})}
}

// Health probes the API, normalizing the error the same way Chat does.
func (c *Client) Health(ctx context.Context) error {
	return reclassify(c.Client.Health(ctx))
}

// ListModels lists the account's models.
func (c *Client) ListModels(ctx context.Context) ([]provider.Model, error) {
	models, err := c.Client.ListModels(ctx)
	return models, reclassify(err)
}

// Chat starts a completion, re-categorizing OpenAI-specific failures both on
// the synchronous path and in the terminal EventError.
func (c *Client) Chat(ctx context.Context, req provider.ChatRequest) (<-chan provider.ChatEvent, error) {
	inner, err := c.Client.Chat(ctx, req)
	if err != nil {
		return nil, reclassify(err)
	}
	out := make(chan provider.ChatEvent, 16)
	go func() {
		defer close(out)
		for ev := range inner {
			if ev.Type == provider.EventError {
				ev.Err = reclassify(ev.Err)
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// Embed implements provider.EmbeddingProvider.
func (c *Client) Embed(ctx context.Context, req provider.EmbeddingRequest) (provider.EmbeddingResponse, error) {
	resp, err := c.Client.Embed(ctx, req)
	return resp, reclassify(err)
}

// quotaPhrases identify OpenAI's exhausted-billing response, which arrives as
// HTTP 429 with an insufficient_quota error type.
var quotaPhrases = []string{
	"insufficient_quota",
	"exceeded your current quota",
	"billing_hard_limit_reached",
	"billing_not_active",
}

// maxTokensPhrases identify the reasoning-model rejection of the max_tokens
// parameter, which those models spell max_completion_tokens.
var maxTokensPhrases = []string{
	"max_completion_tokens",
}

// reclassify corrects the normalized category for OpenAI failures whose HTTP
// status is misleading, and adds an actionable hint where the server's own
// message is not enough.
//
// The important case is insufficient_quota. OpenAI reports an exhausted
// billing quota as HTTP 429, which the generic path — correctly, for every
// other server — maps to ErrRateLimited, and ErrRateLimited is retryable. A
// spent quota does not recover by waiting, so leaving it as a rate limit makes
// Boop retry and fall back through every configured provider on a problem only
// the user can fix. Reporting it as an authentication failure keeps it
// non-retryable and points at the right remedy.
func reclassify(err error) error {
	if err == nil {
		return nil
	}
	var pe *provider.Error
	if !errors.As(err, &pe) {
		return err
	}

	haystack := strings.ToLower(pe.Message + " " + pe.Detail)

	if pe.Category == provider.ErrRateLimited && containsAny(haystack, quotaPhrases) {
		corrected := *pe
		corrected.Category = provider.ErrAuthentication
		corrected.Message = pe.Provider + " rejected the request: the account's quota is exhausted or billing is inactive"
		return &corrected
	}

	if pe.Category == provider.ErrInvalidRequest && containsAny(haystack, maxTokensPhrases) {
		// Reasoning models reject max_tokens outright. The server says so,
		// but not what to do about it from Boop's side.
		corrected := *pe
		corrected.Message = pe.Message +
			" (this model requires max_completion_tokens; leave max_tokens unset for it)"
		return &corrected
	}

	return err
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}
