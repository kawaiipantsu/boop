package fixtures

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// ---------------------------------------------------------------------------
// Ollama
// ---------------------------------------------------------------------------

// ollamaDetails is the nested details object Ollama reports per model. Note
// that context_length lives here, while capabilities is a sibling of details
// on the model itself — a distinction capability-detection code must respect.
type ollamaDetails struct {
	ParentModel       string   `json:"parent_model"`
	Format            string   `json:"format"`
	Family            string   `json:"family"`
	Families          []string `json:"families"`
	ParameterSize     string   `json:"parameter_size"`
	QuantizationLevel string   `json:"quantization_level"`
	ContextLength     int      `json:"context_length,omitempty"`
	EmbeddingLength   int      `json:"embedding_length,omitempty"`
}

type ollamaTag struct {
	Name         string        `json:"name"`
	Model        string        `json:"model"`
	ModifiedAt   string        `json:"modified_at"`
	Size         int64         `json:"size"`
	Digest       string        `json:"digest"`
	Details      ollamaDetails `json:"details"`
	Capabilities []string      `json:"capabilities,omitempty"`
}

// detailsFor projects a ModelInfo onto Ollama's details object.
func detailsFor(m ModelInfo) ollamaDetails {
	var families []string
	if m.Family != "" {
		families = []string{m.Family}
	}
	return ollamaDetails{
		Format:            m.format(),
		Family:            m.Family,
		Families:          families,
		ParameterSize:     m.ParameterSize,
		QuantizationLevel: m.QuantizationLevel,
		ContextLength:     m.ContextWindow,
		EmbeddingLength:   m.EmbeddingLength,
	}
}

// handleOllamaTags serves GET /api/tags, Ollama's model listing.
func (s *Server) handleOllamaTags(w http.ResponseWriter, _ *http.Request) {
	models := s.catalogue()
	tags := make([]ollamaTag, 0, len(models))
	for _, m := range models {
		tags = append(tags, ollamaTag{
			Name:         m.ID,
			Model:        m.ID,
			ModifiedAt:   FixedTime.Format("2006-01-02T15:04:05.000000000Z07:00"),
			Size:         m.Size,
			Digest:       m.Digest,
			Details:      detailsFor(m),
			Capabilities: m.Capabilities,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": tags})
}

// handleOllamaShow serves POST /api/show, which is how an Ollama adapter
// discovers per-model capabilities and context length.
func (s *Server) handleOllamaShow(w http.ResponseWriter, r *http.Request) {
	body, _ := readBody(r)
	var req struct {
		Model string `json:"model"`
		Name  string `json:"name"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	name := orDefault(req.Model, req.Name)
	m, ok := findModel(s.catalogue(), name)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": fmt.Sprintf("model '%s' not found", name),
		})
		return
	}

	arch := orDefault(m.Arch, orDefault(m.Family, "llama"))
	info := map[string]any{
		"general.architecture":    arch,
		"general.parameter_count": m.Size,
	}
	if m.ContextWindow > 0 {
		info[arch+".context_length"] = m.ContextWindow
	}
	if m.EmbeddingLength > 0 {
		info[arch+".embedding_length"] = m.EmbeddingLength
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"license":      "",
		"modelfile":    "FROM " + m.ID,
		"parameters":   "stop \"<|eot_id|>\"",
		"template":     "{{ .Prompt }}",
		"details":      detailsFor(m),
		"model_info":   info,
		"capabilities": m.Capabilities,
	})
}

// ---------------------------------------------------------------------------
// LM Studio
// ---------------------------------------------------------------------------

type lmStudioModel struct {
	ID                string `json:"id"`
	Object            string `json:"object"`
	Type              string `json:"type"`
	Publisher         string `json:"publisher"`
	Arch              string `json:"arch"`
	CompatibilityType string `json:"compatibility_type"`
	Quantization      string `json:"quantization"`
	State             string `json:"state"`
	MaxContextLength  int    `json:"max_context_length"`
}

// handleLMStudioModels serves GET /api/v0/models, LM Studio's native listing,
// which unlike /v1/models reports load state and context length.
func (s *Server) handleLMStudioModels(w http.ResponseWriter, _ *http.Request) {
	models := s.catalogue()
	out := make([]lmStudioModel, 0, len(models))
	for _, m := range models {
		state := "not-loaded"
		if m.Loaded {
			state = "loaded"
		}
		kind := "llm"
		if m.HasCapability("vision") {
			kind = "vlm"
		}
		out = append(out, lmStudioModel{
			ID:                m.ID,
			Object:            "model",
			Type:              kind,
			Publisher:         orDefault(m.Publisher, "boop"),
			Arch:              orDefault(m.Arch, m.Family),
			CompatibilityType: m.format(),
			Quantization:      m.QuantizationLevel,
			State:             state,
			MaxContextLength:  m.ContextWindow,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

// ---------------------------------------------------------------------------
// Lemonade
// ---------------------------------------------------------------------------

// handleLemonadeLifecycle serves POST /api/v1/{load,unload,pull}, the
// endpoints behind provider.ModelLifecycleProvider.
//
// Loading a model flips its reported state so a follow-up listing observes the
// change; the request itself is captured like any other, so tests can assert
// the payload.
func (s *Server) handleLemonadeLifecycle(w http.ResponseWriter, r *http.Request) {
	body, _ := readBody(r)
	var req struct {
		Model     string `json:"model"`
		ModelName string `json:"model_name"`
		Name      string `json:"name"`
	}
	_ = json.Unmarshal(body, &req)
	name := orDefault(req.Model, orDefault(req.ModelName, req.Name))

	loading := r.URL.Path != "/api/v1/unload"
	s.mu.Lock()
	found := name == ""
	for i := range s.models {
		if s.models[i].ID == name {
			s.models[i].Loaded = loading
			found = true
		}
	}
	s.mu.Unlock()

	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"detail": fmt.Sprintf("model %q is not installed", name),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "success", "message": name})
}
