package lemonade

import (
	"strings"

	"github.com/kawaiipantsu/boop/internal/provider"
)

// backendSuffixes are the execution-backend markers Lemonade appends to model
// ids, for example "Llama-3.2-1B-Instruct-Hybrid" or "Qwen2.5-7B-CPU".
//
// They describe how a model runs — CPU, iGPU, Ryzen AI NPU, or a hybrid split
// — and never what it can do, so they are stripped before any id-based
// reasoning. Leaving them in would let "-npu" or "-oga" contribute noise to
// the shared name heuristics.
//
// INFERRED from Lemonade's model naming convention; harmless if a build uses
// none of them, since stripping simply finds nothing.
var backendSuffixes = []string{
	"-hybrid", "-npu", "-cpu", "-gpu", "-igpu", "-gguf", "-onnx", "-oga",
}

// embeddingMarkers identify an embedding model by name.
//
// Lemonade's OpenAI listing carries no capability metadata at all, so a name
// is the only signal available. Getting this wrong in the "yes" direction is
// the safer failure: an embedding model wrongly offered for chat produces
// nonsense, whereas a chat model wrongly withheld is merely inconvenient — and
// these markers do not appear in chat model names.
var embeddingMarkers = []string{
	"embed", "embedding", "bge-", "gte-", "e5-", "nomic-embed", "all-minilm",
	"mxbai", "text-embedding", "snowflake-arctic-embed",
}

// stripBackendSuffix removes a trailing execution-backend marker from a
// lowercased model id.
func stripBackendSuffix(id string) string {
	for _, suffix := range backendSuffixes {
		if strings.HasSuffix(id, suffix) {
			return strings.TrimSuffix(id, suffix)
		}
	}
	return id
}

// refineCapabilities adjusts the shared detection for Lemonade's conventions.
//
// It is deliberately close to an identity function. Lemonade's model listing
// reports no capability metadata that Boop could verify, and §8 forbids
// inventing one, so this only does the two things the model id genuinely
// supports: it keeps an embedding model out of chat routing, and it guarantees
// streaming for chat models, which Lemonade's OpenAI endpoint always provides.
//
// Everything else — tools, vision, reasoning — is left to the shared name
// heuristics, which are no less informed here than a vendor-specific guess
// would be.
func refineCapabilities(model string, base provider.Capabilities) provider.Capabilities {
	id := stripBackendSuffix(strings.ToLower(strings.TrimSpace(model)))
	if id == "" {
		return base
	}
	if isEmbeddingID(id) {
		return provider.Capabilities{}.Add(provider.CapabilityEmbeddings)
	}
	if base.Has(provider.CapabilityEmbeddings) || base.Has(provider.CapabilityAudio) {
		// The shared detection already decided this is not a chat model.
		return base
	}
	return base.Add(provider.CapabilityStreaming)
}

// isEmbeddingID reports whether a stripped model id names an embedding model.
// An explicit "chat" marker wins, because instruction-tuned embedders such as
// e5-mistral-7b-instruct are still embedders while a "chat" variant is not.
func isEmbeddingID(id string) bool {
	if strings.Contains(id, "chat") {
		return false
	}
	for _, marker := range embeddingMarkers {
		if strings.Contains(id, marker) {
			return true
		}
	}
	return false
}
