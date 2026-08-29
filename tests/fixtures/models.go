package fixtures

// ModelInfo is the harness's single description of a model, projected onto
// whichever vendor shape an endpoint needs: OpenAI's /v1/models entry,
// Ollama's /api/tags entry and /api/show payload, or LM Studio's
// /api/v0/models entry.
//
// One struct for all vendors keeps a test's setup in one place: describe the
// model once, then point any adapter at the server.
type ModelInfo struct {
	// ID is the model identifier clients pass back as "model".
	ID string
	// DisplayName is a human label where the vendor shape has one.
	DisplayName string
	// OwnedBy fills OpenAI's owned_by field.
	OwnedBy string
	// Publisher and Arch fill LM Studio's native metadata.
	Publisher string
	Arch      string

	// ContextWindow is surfaced as OpenAI context_length, Ollama
	// details.context_length and LM Studio max_context_length.
	ContextWindow int
	// EmbeddingLength is surfaced as Ollama details.embedding_length.
	EmbeddingLength int
	// MaxOutput is the advertised generation limit, zero when unknown.
	MaxOutput int

	// Capabilities is the vendor-reported capability list, e.g.
	// ["completion", "tools", "vision"]. Ollama reports exactly this shape as
	// a top-level array per model, and it is what capability detection tests
	// key off.
	Capabilities []string

	// Family, ParameterSize and QuantizationLevel fill Ollama's details.
	Family            string
	ParameterSize     string
	QuantizationLevel string
	// Format defaults to "gguf" when empty.
	Format string
	// Digest and Size fill Ollama's per-model fields.
	Digest string
	Size   int64

	// Loaded marks the model as resident, reported by LM Studio as
	// state:"loaded" and relevant to ModelLifecycleProvider tests.
	Loaded bool
}

// HasCapability reports whether the model advertises the named capability.
func (m ModelInfo) HasCapability(name string) bool {
	for _, c := range m.Capabilities {
		if c == name {
			return true
		}
	}
	return false
}

// format resolves the weight format, defaulting to gguf like local runtimes.
func (m ModelInfo) format() string {
	if m.Format != "" {
		return m.Format
	}
	return "gguf"
}

// DefaultModels is the catalogue served when a test does not supply one: a
// tool-capable text model and a vision model, so capability-gating logic has
// both a positive and a negative case out of the box.
func DefaultModels() []ModelInfo {
	return []ModelInfo{
		{
			ID:                "boop-test-model",
			DisplayName:       "Boop Test Model",
			OwnedBy:           "boop",
			Publisher:         "boop",
			Arch:              "llama",
			ContextWindow:     8192,
			EmbeddingLength:   4096,
			MaxOutput:         2048,
			Capabilities:      []string{"completion", "tools"},
			Family:            "llama",
			ParameterSize:     "8.0B",
			QuantizationLevel: "Q4_K_M",
			Digest:            "sha256:1111111111111111111111111111111111111111111111111111111111111111",
			Size:              4920753328,
			Loaded:            true,
		},
		{
			ID:                "boop-test-vision",
			DisplayName:       "Boop Test Vision",
			OwnedBy:           "boop",
			Publisher:         "boop",
			Arch:              "llama",
			ContextWindow:     32768,
			EmbeddingLength:   4096,
			MaxOutput:         4096,
			Capabilities:      []string{"completion", "tools", "vision"},
			Family:            "llama",
			ParameterSize:     "11.0B",
			QuantizationLevel: "Q8_0",
			Digest:            "sha256:2222222222222222222222222222222222222222222222222222222222222222",
			Size:              7365960704,
		},
	}
}

// TextOnlyModel is a catalogue entry with neither tools nor vision, for
// testing the "model lacks the required capability" path of §8.
func TextOnlyModel(id string) ModelInfo {
	return ModelInfo{
		ID:                id,
		OwnedBy:           "boop",
		Arch:              "llama",
		ContextWindow:     4096,
		EmbeddingLength:   4096,
		Capabilities:      []string{"completion"},
		Family:            "llama",
		ParameterSize:     "1.5B",
		QuantizationLevel: "Q4_0",
		Digest:            "sha256:3333333333333333333333333333333333333333333333333333333333333333",
		Size:              1073741824,
	}
}

// findModel looks a model up by id.
func findModel(models []ModelInfo, id string) (ModelInfo, bool) {
	for _, m := range models {
		if m.ID == id {
			return m, true
		}
	}
	return ModelInfo{}, false
}
