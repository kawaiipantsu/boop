package anthropic

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/boop-dev/boop/internal/provider"
)

// modelPageLimit is the page size for the listing. The listing is cursor
// paginated and the catalogue is small, so one page normally suffices.
const modelPageLimit = 100

// maxModelPages bounds pagination so a server that always reports has_more
// cannot spin forever.
const maxModelPages = 10

// ListModels returns the models the account can use.
//
// The result also warms the capability cache, so a later Capabilities call for
// a listed model costs nothing.
func (c *Client) ListModels(ctx context.Context) ([]provider.Model, error) {
	var (
		out    []provider.Model
		cursor string
	)
	for page := 0; page < maxModelPages; page++ {
		path := fmt.Sprintf("%s?limit=%d", modelsPath, modelPageLimit)
		if cursor != "" {
			path += "&after_id=" + url.QueryEscape(cursor)
		}
		var resp modelsResponse
		if err := c.getJSON(ctx, path, &resp); err != nil {
			return nil, err
		}
		for _, wm := range resp.Data {
			id := strings.TrimSpace(wm.ID)
			if id == "" {
				continue
			}
			c.storeModel(wm)
			out = append(out, provider.Model{
				ID:            id,
				Provider:      c.name,
				DisplayName:   wm.DisplayName,
				ContextWindow: wm.MaxInputTokens,
				MaxOutput:     wm.MaxTokens,
				Capabilities:  c.deriveCapabilities(id),
			})
		}
		if !resp.HasMore || resp.LastID == "" {
			break
		}
		cursor = resp.LastID
	}
	return out, nil
}

// Capabilities reports what the given model can do.
//
// Anthropic's model listing does not describe features, so detection is a
// family table over the model id rather than server metadata. §8 forbids
// assuming a capability, so the table only claims what the published families
// actually have, and anything unrecognized falls back to the conservative
// modern-Claude baseline.
func (c *Client) Capabilities(ctx context.Context, model string) (provider.Capabilities, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, c.newError(provider.ErrInvalidRequest, "", "no model selected",
			"Capabilities called with an empty model id", 0, nil)
	}
	if caps, ok := c.cachedCapabilities(model); ok {
		return caps, nil
	}
	caps := c.deriveCapabilities(model)
	c.storeCapabilities(model, caps)
	return caps, nil
}

// Legacy family markers. Everything not listed here is treated as a current
// Claude model, which is the safe direction: the modern baseline is the
// smaller claim for vision and reasoning only where a family genuinely lacks
// them.
var (
	// preToolFamilies predate tool use entirely.
	preToolFamilies = []string{"claude-instant", "claude-1", "claude-2"}
	// nonVisionFamilies are chat models that accept text only.
	nonVisionFamilies = []string{"claude-instant", "claude-1", "claude-2", "claude-3-5-haiku"}
	// preThinkingFamilies predate extended thinking, which arrived with the
	// 3.7 generation.
	preThinkingFamilies = []string{
		"claude-instant", "claude-1", "claude-2",
		"claude-3-opus", "claude-3-sonnet", "claude-3-haiku",
		"claude-3-5-sonnet", "claude-3-5-haiku",
	}
)

// deriveCapabilities builds the capability set for a model id.
func (c *Client) deriveCapabilities(model string) provider.Capabilities {
	id := strings.ToLower(strings.TrimSpace(model))

	caps := provider.Capabilities{}.Add(provider.CapabilityStreaming)
	if !matchesAny(id, preToolFamilies) {
		caps = caps.Add(provider.CapabilityTools, provider.CapabilityStructuredOutput)
	}
	if !matchesAny(id, nonVisionFamilies) {
		caps = caps.Add(provider.CapabilityVision)
	}
	if !matchesAny(id, preThinkingFamilies) {
		caps = caps.Add(provider.CapabilityReasoning)
	}
	// Anthropic publishes no embedding, audio or Responses-API surface, so
	// those capabilities are never claimed.
	if c.refineCaps != nil {
		caps = c.refineCaps(model, caps)
	}
	return caps
}

func matchesAny(id string, families []string) bool {
	for _, f := range families {
		if strings.Contains(id, f) {
			return true
		}
	}
	return false
}

func (c *Client) storeModel(wm wireModel) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byID[wm.ID] = wm
}

func (c *Client) cachedCapabilities(model string) (provider.Capabilities, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	caps, ok := c.caps[model]
	return caps, ok
}

func (c *Client) storeCapabilities(model string, caps provider.Capabilities) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.caps[model] = caps
}
