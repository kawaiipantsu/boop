package openai

import (
	"strings"

	"github.com/kawaiipantsu/boop/internal/provider"
)

// familyRule maps an OpenAI model-id prefix onto the capabilities that family
// actually has.
//
// OpenAI's catalogue is not uniform — the o-series reasoning models drop tools
// and vision in their small variants, the audio and image endpoints share the
// gpt-4o prefix without being chat models at all — so the generic name
// heuristics in openaicompat get several of these wrong. This table is the only
// vendor knowledge this adapter adds beyond headers and error shape.
type familyRule struct {
	// prefix is matched against the normalized (lowercased, fine-tune
	// stripped) model id. The first matching rule wins, so the table is
	// ordered specific to general.
	prefix string
	caps   provider.Capabilities
}

// Capability shorthands, purely to keep the table below readable.
var (
	capStream     = provider.CapabilityStreaming
	capTools      = provider.CapabilityTools
	capVision     = provider.CapabilityVision
	capReasoning  = provider.CapabilityReasoning
	capStructured = provider.CapabilityStructuredOutput
	capResponses  = provider.CapabilityResponses
	capEmbeddings = provider.CapabilityEmbeddings
	capAudio      = provider.CapabilityAudio
)

// openAIFamilies is consulted in order; the first prefix match decides.
//
// An empty capability set is meaningful: it marks a model that is not usable
// for anything Boop's capability vocabulary describes (image generation,
// moderation), which keeps the router from ever selecting it for a
// conversation.
var openAIFamilies = []familyRule{
	// Embedding endpoints.
	{prefix: "text-embedding", caps: provider.Capabilities{}.Add(capEmbeddings)},

	// Non-chat endpoints. Image generation and moderation share the naming
	// space with chat models but answer a different API.
	{prefix: "dall-e", caps: provider.Capabilities{}},
	{prefix: "gpt-image", caps: provider.Capabilities{}},
	{prefix: "omni-moderation", caps: provider.Capabilities{}},
	{prefix: "text-moderation", caps: provider.Capabilities{}},

	// Speech and realtime models. These must precede the gpt-4o rules,
	// because they are spelled as gpt-4o variants.
	{prefix: "whisper", caps: provider.Capabilities{}.Add(capAudio)},
	{prefix: "tts-", caps: provider.Capabilities{}.Add(capAudio)},
	{prefix: "gpt-4o-transcribe", caps: provider.Capabilities{}.Add(capAudio)},
	{prefix: "gpt-4o-mini-transcribe", caps: provider.Capabilities{}.Add(capAudio)},
	{prefix: "gpt-4o-mini-tts", caps: provider.Capabilities{}.Add(capAudio)},
	{prefix: "gpt-4o-audio", caps: provider.Capabilities{}.Add(capAudio, capStream, capTools)},
	{prefix: "gpt-4o-mini-audio", caps: provider.Capabilities{}.Add(capAudio, capStream, capTools)},
	{prefix: "gpt-4o-realtime", caps: provider.Capabilities{}.Add(capAudio, capStream, capTools)},
	{prefix: "gpt-4o-mini-realtime", caps: provider.Capabilities{}.Add(capAudio, capStream, capTools)},
	{prefix: "gpt-audio", caps: provider.Capabilities{}.Add(capAudio, capStream, capTools)},
	{prefix: "gpt-realtime", caps: provider.Capabilities{}.Add(capAudio, capStream, capTools)},

	// Reasoning models. The mini and preview variants of the first o-series
	// generation accept neither tools nor images; claiming otherwise makes
	// Boop send a request that is rejected at the API.
	{prefix: "o1-mini", caps: provider.Capabilities{}.Add(capStream, capReasoning)},
	{prefix: "o1-preview", caps: provider.Capabilities{}.Add(capStream, capReasoning)},
	{prefix: "o3-mini", caps: provider.Capabilities{}.Add(capStream, capTools, capReasoning, capStructured, capResponses)},
	{prefix: "o1", caps: provider.Capabilities{}.Add(capStream, capTools, capVision, capReasoning, capStructured, capResponses)},
	{prefix: "o3", caps: provider.Capabilities{}.Add(capStream, capTools, capVision, capReasoning, capStructured, capResponses)},
	{prefix: "o4-mini", caps: provider.Capabilities{}.Add(capStream, capTools, capVision, capReasoning, capStructured, capResponses)},

	// Current chat models.
	{prefix: "gpt-5", caps: provider.Capabilities{}.Add(capStream, capTools, capVision, capReasoning, capStructured, capResponses)},
	{prefix: "gpt-4.1", caps: provider.Capabilities{}.Add(capStream, capTools, capVision, capStructured, capResponses)},
	{prefix: "chatgpt-4o", caps: provider.Capabilities{}.Add(capStream, capVision, capStructured)},
	{prefix: "gpt-4o", caps: provider.Capabilities{}.Add(capStream, capTools, capVision, capStructured, capResponses)},
	{prefix: "gpt-4-turbo", caps: provider.Capabilities{}.Add(capStream, capTools, capVision, capStructured, capResponses)},

	// Legacy chat models: text only.
	{prefix: "gpt-4", caps: provider.Capabilities{}.Add(capStream, capTools, capStructured, capResponses)},
	{prefix: "gpt-3.5", caps: provider.Capabilities{}.Add(capStream, capTools)},
}

// refineCapabilities is installed as openaicompat's RefineCapabilities hook.
//
// A model id that matches no known family keeps the generic heuristics: a
// family released after this table was written is better served by a guess
// than by an empty set.
func refineCapabilities(model string, base provider.Capabilities) provider.Capabilities {
	id := normalizeModelID(model)
	if id == "" {
		return base
	}
	for _, rule := range openAIFamilies {
		if strings.HasPrefix(id, rule.prefix) {
			return append(provider.Capabilities(nil), rule.caps...)
		}
	}
	return base
}

// normalizeModelID lowercases a model id and unwraps a fine-tune id, which is
// spelled ft:<base model>:<org>::<suffix> and inherits the base model's
// capabilities.
func normalizeModelID(model string) string {
	id := strings.ToLower(strings.TrimSpace(model))
	if !strings.HasPrefix(id, "ft:") {
		return id
	}
	rest := id[len("ft:"):]
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		return rest[:i]
	}
	return rest
}
