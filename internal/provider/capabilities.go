package provider

import "sort"

// Capability is a runtime-discovered model feature.
//
// Boop never assumes a capability is present; unsupported requests are
// reported to the user with compatible alternatives rather than attempted.
type Capability string

const (
	CapabilityStreaming        Capability = "streaming"
	CapabilityTools            Capability = "tools"
	CapabilityVision           Capability = "vision"
	CapabilityReasoning        Capability = "reasoning"
	CapabilityResponses        Capability = "responses"
	CapabilityEmbeddings       Capability = "embeddings"
	CapabilityStructuredOutput Capability = "structured_output"
	CapabilityAudio            Capability = "audio"
)

// Capabilities is a set of capabilities supported by a provider/model pair.
type Capabilities []Capability

// Has reports whether c contains the given capability.
func (c Capabilities) Has(want Capability) bool {
	for _, got := range c {
		if got == want {
			return true
		}
	}
	return false
}

// HasAll reports whether c contains every requested capability.
func (c Capabilities) HasAll(want ...Capability) bool {
	for _, w := range want {
		if !c.Has(w) {
			return false
		}
	}
	return true
}

// Missing returns the requested capabilities absent from c, in request order.
func (c Capabilities) Missing(want ...Capability) []Capability {
	var missing []Capability
	for _, w := range want {
		if !c.Has(w) {
			missing = append(missing, w)
		}
	}
	return missing
}

// Add returns a copy of c with the given capabilities added, deduplicated and
// sorted so that comparisons and test assertions are stable.
func (c Capabilities) Add(add ...Capability) Capabilities {
	seen := make(map[Capability]struct{}, len(c)+len(add))
	out := make(Capabilities, 0, len(c)+len(add))
	for _, cap := range append(append(Capabilities{}, c...), add...) {
		if _, dup := seen[cap]; dup {
			continue
		}
		seen[cap] = struct{}{}
		out = append(out, cap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Strings renders the capability set for display.
func (c Capabilities) Strings() []string {
	out := make([]string, len(c))
	for i, cap := range c {
		out[i] = string(cap)
	}
	return out
}
