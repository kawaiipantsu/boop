package openaicompat

import (
	"context"
	"strings"

	"github.com/kawaiipantsu/boop/internal/provider"
)

// Capabilities reports what the given model can do.
//
// Detection is layered, cheapest first: a cached answer, then server-declared
// metadata from the model listing, then name heuristics, then the adapter's
// RefineCapabilities hook which always has the final word. §8 forbids assuming
// a capability, so heuristics only ever run where the server was silent.
//
// A failed model listing is not fatal — many local servers expose a usable chat
// endpoint with a thin or missing /models — so detection degrades to heuristics
// rather than failing the call. Context cancellation is still reported.
func (c *Client) Capabilities(ctx context.Context, model string) (provider.Capabilities, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, c.newError(provider.ErrInvalidRequest, "", "no model selected", "Capabilities called with an empty model id", 0, nil)
	}

	if caps, ok := c.cachedCapabilities(model); ok {
		return caps, nil
	}

	meta, ok := c.cachedMeta(model)
	if !ok {
		// Warm the metadata cache; ignore listing failures on purpose.
		if _, err := c.ListModels(ctx); err != nil {
			if cat, isProviderErr := provider.CategoryOf(err); isProviderErr &&
				(cat == provider.ErrCancelled || cat == provider.ErrTimeout) && ctx.Err() != nil {
				return nil, err
			}
		}
		if caps, cached := c.cachedCapabilities(model); cached {
			return caps, nil
		}
		meta, _ = c.cachedMeta(model)
	}

	caps := c.deriveCapabilities(model, meta)
	c.storeCapabilities(model, caps)
	return caps, nil
}

// deriveCapabilities combines server metadata with name heuristics and applies
// the adapter refinement hook.
func (c *Client) deriveCapabilities(model string, meta modelMeta) provider.Capabilities {
	caps := heuristicCapabilities(model)
	caps = applyDeclared(caps, meta)
	if c.refineCaps != nil {
		caps = c.refineCaps(model, caps)
	}
	return caps
}

// applyDeclared folds server-declared capability and modality names into the
// heuristic set. Declarations only add: a server that lists "vision" is
// authoritative, but a server that lists nothing is not saying "no tools".
func applyDeclared(caps provider.Capabilities, meta modelMeta) provider.Capabilities {
	add := func(cap provider.Capability) { caps = caps.Add(cap) }
	for _, name := range meta.Declared {
		switch {
		case strings.Contains(name, "tool"), strings.Contains(name, "function"):
			add(provider.CapabilityTools)
		case strings.Contains(name, "vision"), strings.Contains(name, "image"):
			add(provider.CapabilityVision)
		case strings.Contains(name, "reason"), strings.Contains(name, "think"):
			add(provider.CapabilityReasoning)
		case strings.Contains(name, "embed"):
			add(provider.CapabilityEmbeddings)
		case strings.Contains(name, "audio"), strings.Contains(name, "speech"):
			add(provider.CapabilityAudio)
		case strings.Contains(name, "json"), strings.Contains(name, "structured"), strings.Contains(name, "schema"):
			add(provider.CapabilityStructuredOutput)
		case strings.Contains(name, "stream"):
			add(provider.CapabilityStreaming)
		case strings.Contains(name, "response"):
			add(provider.CapabilityResponses)
		}
	}
	for _, modality := range meta.Modalities {
		switch {
		case strings.Contains(modality, "image"), strings.Contains(modality, "vision"):
			add(provider.CapabilityVision)
		case strings.Contains(modality, "audio"):
			add(provider.CapabilityAudio)
		}
	}
	return caps
}

// Capability detection tables.
//
// tokens match a whole dash/dot-delimited segment of the model id, so "o1"
// matches "o1-preview" but not "phio1"; substrings match anywhere. Both lists
// are lowercase.
var (
	visionTokens = []string{"vl", "4o", "vision"}
	visionParts  = []string{
		"vision", "llava", "bakllava", "moondream", "pixtral", "internvl",
		"minicpm-v", "gemma-3", "gemma3", "llama-3.2-11b", "llama-3.2-90b",
		"llama3.2-vision", "gpt-4o", "gpt-4.1", "gpt-5", "grok-2-vision",
		"grok-4", "qwen2-vl", "qwen2.5-vl", "qwen3-vl", "phi-3-vision",
		"phi-4-multimodal", "mistral-small-3.1", "granite3.2-vision",
	}

	reasoningTokens = []string{"o1", "o3", "r1", "qwq"}
	reasoningParts  = []string{
		"o4-mini", "deepseek-r1", "reason", "thinking", "magistral",
		"gpt-5", "grok-3-mini", "grok-4", "qwq", "phi-4-reasoning",
		"exaone-deep", "openthinker", "gpt-oss",
	}

	embeddingParts = []string{
		"embed", "embedding", "bge-", "gte-", "e5-", "nomic-embed",
		"all-minilm", "mxbai", "text-embedding", "snowflake-arctic-embed",
	}

	audioParts = []string{"whisper", "tts", "audio", "voice", "speech", "parakeet"}

	// Models that are known not to accept tools even though almost every
	// modern chat model does. Base (non-instruct) checkpoints are the common
	// case; advertising tools for them produces silent nonsense.
	noToolsParts = []string{"-base", "base-", "codellama", "starcoder", "stable-code", "tinyllama"}
)

// heuristicCapabilities guesses capabilities from a model id.
//
// The guesses are conservative in the directions that matter: a wrong "yes" for
// vision makes Boop send an image a model cannot read, so vision and reasoning
// require a positive signal, while streaming and tools — near-universal in the
// OpenAI dialect — are assumed for chat models and withdrawn for known
// exceptions.
func heuristicCapabilities(model string) provider.Capabilities {
	id := strings.ToLower(strings.TrimSpace(model))
	toks := idTokens(id)

	if containsAnyPart(id, embeddingParts) && !strings.Contains(id, "chat") {
		// Embedding models do not chat; claiming streaming or tools for them
		// would let the router pick one for a conversation. Instruction-tuned
		// embedding models (e5-mistral-7b-instruct) are still embedders, so
		// only an explicit "chat" marker overrides this.
		return provider.Capabilities{}.Add(provider.CapabilityEmbeddings)
	}
	if containsAnyPart(id, audioParts) {
		return provider.Capabilities{}.Add(provider.CapabilityAudio)
	}

	caps := provider.Capabilities{}.Add(
		provider.CapabilityStreaming,
		provider.CapabilityStructuredOutput,
	)
	if !containsAnyPart(id, noToolsParts) {
		caps = caps.Add(provider.CapabilityTools)
	}
	if containsAnyPart(id, visionParts) || hasAnyToken(toks, visionTokens) {
		caps = caps.Add(provider.CapabilityVision)
	}
	if containsAnyPart(id, reasoningParts) || hasAnyToken(toks, reasoningTokens) {
		caps = caps.Add(provider.CapabilityReasoning)
	}
	return caps
}

// idTokens splits a model id into alphanumeric segments, so short markers can
// be matched without false positives from the middle of a word.
func idTokens(id string) []string {
	return strings.FieldsFunc(id, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
}

func hasAnyToken(tokens, want []string) bool {
	for _, t := range tokens {
		for _, w := range want {
			if t == w {
				return true
			}
		}
	}
	return false
}

func containsAnyPart(id string, parts []string) bool {
	for _, p := range parts {
		if strings.Contains(id, p) {
			return true
		}
	}
	return false
}

// cachedCapabilities returns a previously derived capability set.
func (c *Client) cachedCapabilities(model string) (provider.Capabilities, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	caps, ok := c.capsByID[model]
	return caps, ok
}

func (c *Client) storeCapabilities(model string, caps provider.Capabilities) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.capsByID[model] = caps
}

func (c *Client) cachedMeta(model string) (modelMeta, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	meta, ok := c.metaByID[model]
	return meta, ok
}

func (c *Client) storeMeta(meta modelMeta) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.metaByID[meta.ID] = meta
}
