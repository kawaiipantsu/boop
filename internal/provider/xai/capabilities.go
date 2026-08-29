package xai

import (
	"strings"

	"github.com/kawaiipantsu/boop/internal/provider"
)

// familyRule maps a Grok model-id prefix onto that family's capabilities.
type familyRule struct {
	// prefix is matched against the lowercased model id; the first match
	// wins, so the table runs specific to general.
	prefix string
	caps   provider.Capabilities
}

// Capability shorthands, to keep the table readable.
var (
	capStream     = provider.CapabilityStreaming
	capTools      = provider.CapabilityTools
	capVision     = provider.CapabilityVision
	capReasoning  = provider.CapabilityReasoning
	capStructured = provider.CapabilityStructuredOutput
)

// grokFamilies is consulted in order.
//
// The generic heuristics in openaicompat already get most of this right; the
// corrections that matter are the image-generation model (which is not a chat
// model at all) and the split between the reasoning and non-reasoning members
// of the Grok 3 generation, where routing to the wrong one silently costs the
// user the thinking they asked for.
var grokFamilies = []familyRule{
	// Image generation answers a different endpoint and can serve no chat
	// request; an empty set keeps the router away from it.
	{prefix: "grok-2-image", caps: provider.Capabilities{}},

	{prefix: "grok-4", caps: provider.Capabilities{}.Add(capStream, capTools, capVision, capReasoning, capStructured)},
	{prefix: "grok-3-mini", caps: provider.Capabilities{}.Add(capStream, capTools, capReasoning, capStructured)},
	{prefix: "grok-3", caps: provider.Capabilities{}.Add(capStream, capTools, capStructured)},
	{prefix: "grok-code", caps: provider.Capabilities{}.Add(capStream, capTools, capStructured)},
	{prefix: "grok-2-vision", caps: provider.Capabilities{}.Add(capStream, capTools, capVision, capStructured)},
	{prefix: "grok-2", caps: provider.Capabilities{}.Add(capStream, capTools, capStructured)},

	// Preview-era models, kept because deployments still pin them.
	{prefix: "grok-vision-beta", caps: provider.Capabilities{}.Add(capStream, capVision)},
	{prefix: "grok-beta", caps: provider.Capabilities{}.Add(capStream, capTools)},
}

// refineCapabilities is installed as openaicompat's RefineCapabilities hook.
//
// An id matching no known family keeps the generic heuristics, so a Grok
// release newer than this table still gets a usable answer.
func refineCapabilities(model string, base provider.Capabilities) provider.Capabilities {
	id := strings.ToLower(strings.TrimSpace(model))
	if id == "" {
		return base
	}
	for _, rule := range grokFamilies {
		if strings.HasPrefix(id, rule.prefix) {
			return append(provider.Capabilities(nil), rule.caps...)
		}
	}
	return base
}
