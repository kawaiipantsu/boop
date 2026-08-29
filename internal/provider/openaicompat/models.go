package openaicompat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/kawaiipantsu/boop/internal/provider"
)

// modelMeta is the server-declared metadata Boop keeps for a model. It is the
// evidence half of capability detection; the heuristic half only runs where the
// server said nothing.
type modelMeta struct {
	ID            string
	DisplayName   string
	ContextWindow int
	MaxOutput     int
	// Declared holds capability names the server advertised, lowercased.
	Declared []string
	// Modalities holds declared input modalities, lowercased.
	Modalities []string
}

// wireModel is one entry of a /models listing. The many alternative field names
// exist because every OpenAI-compatible server invented its own.
type wireModel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Model       string `json:"model"`
	Object      string `json:"object"`
	OwnedBy     string `json:"owned_by"`
	DisplayName string `json:"display_name"`

	ContextLength    int `json:"context_length"`
	MaxContextLength int `json:"max_context_length"`
	ContextWindow    int `json:"context_window"`
	MaxModelLen      int `json:"max_model_len"`

	MaxOutputTokens     int `json:"max_output_tokens"`
	MaxCompletionTokens int `json:"max_completion_tokens"`

	Capabilities json.RawMessage `json:"capabilities"`
	Modalities   []string        `json:"modalities"`
	InputModal   []string        `json:"input_modalities"`
	Architecture *struct {
		Modality        string   `json:"modality"`
		InputModalities []string `json:"input_modalities"`
	} `json:"architecture"`
}

// modelsResponse covers the standard {"data":[...]} envelope. A bare array and
// an {"models":[...]} envelope are handled separately in decodeModels.
type modelsResponse struct {
	Data   []wireModel `json:"data"`
	Models []wireModel `json:"models"`
}

// identifier returns the best available model id.
func (m wireModel) identifier() string {
	return firstNonEmpty(strings.TrimSpace(m.ID), strings.TrimSpace(m.Model), strings.TrimSpace(m.Name))
}

func (m wireModel) contextWindow() int {
	return firstNonZero(m.ContextLength, m.MaxContextLength, m.ContextWindow, m.MaxModelLen)
}

func (m wireModel) maxOutput() int {
	return firstNonZero(m.MaxOutputTokens, m.MaxCompletionTokens)
}

// declaredCapabilities normalizes the several shapes servers use for a
// capability list: an array of names, or an object of name -> bool.
func (m wireModel) declaredCapabilities() []string {
	if len(m.Capabilities) == 0 {
		return nil
	}
	var names []string
	if err := json.Unmarshal(m.Capabilities, &names); err == nil {
		return lowerAll(names)
	}
	var flags map[string]bool
	if err := json.Unmarshal(m.Capabilities, &flags); err == nil {
		out := make([]string, 0, len(flags))
		for name, on := range flags {
			if on {
				out = append(out, strings.ToLower(name))
			}
		}
		sort.Strings(out)
		return out
	}
	return nil
}

func (m wireModel) modalities() []string {
	var out []string
	out = append(out, m.Modalities...)
	out = append(out, m.InputModal...)
	if m.Architecture != nil {
		out = append(out, m.Architecture.InputModalities...)
		if m.Architecture.Modality != "" {
			out = append(out, m.Architecture.Modality)
		}
	}
	return lowerAll(out)
}

func lowerAll(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ListModels returns the models the backend offers, with capabilities attached.
//
// The result feeds the router and the model picker, so it also warms the
// capability cache: a later Capabilities call for a listed model costs nothing.
func (c *Client) ListModels(ctx context.Context) ([]provider.Model, error) {
	raw, err := c.rawListModels(ctx)
	if err != nil {
		return nil, err
	}

	models := make([]provider.Model, 0, len(raw))
	for _, wm := range raw {
		id := wm.identifier()
		if id == "" {
			continue
		}
		meta := modelMeta{
			ID:            id,
			DisplayName:   firstNonEmpty(wm.DisplayName, wm.Name),
			ContextWindow: wm.contextWindow(),
			MaxOutput:     wm.maxOutput(),
			Declared:      wm.declaredCapabilities(),
			Modalities:    wm.modalities(),
		}
		c.storeMeta(meta)
		models = append(models, provider.Model{
			ID:            id,
			Provider:      c.name,
			DisplayName:   meta.DisplayName,
			ContextWindow: meta.ContextWindow,
			MaxOutput:     meta.MaxOutput,
			Capabilities:  c.deriveCapabilities(id, meta),
		})
	}

	if c.refineModels != nil {
		models = c.refineModels(models)
	}
	// Cache from the refined list so an adapter's corrections are what the
	// rest of Boop sees.
	for _, m := range models {
		if len(m.Capabilities) > 0 {
			c.storeCapabilities(m.ID, m.Capabilities)
		}
	}
	return models, nil
}

// rawListModels fetches and decodes the model listing.
func (c *Client) rawListModels(ctx context.Context) ([]wireModel, error) {
	reqCtx, cancel := c.withTimeout(ctx)
	defer cancel()

	req, err := c.newRequest(reqCtx, http.MethodGet, c.modelsPath, nil, "application/json")
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.transportError(reqCtx, ctx, "", err)
	}
	defer drainAndClose(resp.Body)

	raw, readErr := readLimited(resp.Body, maxResponseBytes)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, c.statusError(resp.StatusCode, "", raw)
	}
	if readErr != nil {
		return nil, c.transportError(reqCtx, ctx, "", readErr)
	}
	models, err := decodeModels(raw)
	if err != nil {
		return nil, c.malformedError("", raw, err)
	}
	return models, nil
}

// decodeModels accepts the standard envelope, an Ollama-style {"models":[...]}
// envelope and a bare array.
func decodeModels(raw []byte) ([]wireModel, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, fmt.Errorf("empty model listing")
	}
	if strings.HasPrefix(trimmed, "[") {
		var list []wireModel
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, err
		}
		return list, nil
	}
	var env modelsResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if len(env.Data) > 0 {
		return env.Data, nil
	}
	return env.Models, nil
}

// Embed implements provider.EmbeddingProvider.
func (c *Client) Embed(ctx context.Context, req provider.EmbeddingRequest) (provider.EmbeddingResponse, error) {
	if strings.TrimSpace(req.Model) == "" {
		return provider.EmbeddingResponse{}, c.newError(provider.ErrInvalidRequest, "",
			"no embedding model selected", "EmbeddingRequest.Model is empty", 0, nil)
	}
	if len(req.Input) == 0 {
		return provider.EmbeddingResponse{}, c.newError(provider.ErrInvalidRequest, req.Model,
			"nothing to embed", "EmbeddingRequest.Input is empty", 0, nil)
	}

	body := struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}{Model: req.Model, Input: req.Input}

	var decoded struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Usage *wireUsage `json:"usage"`
	}
	if err := c.PostJSON(ctx, c.embeddingsPath, body, &decoded); err != nil {
		return provider.EmbeddingResponse{}, err
	}
	if len(decoded.Data) == 0 {
		return provider.EmbeddingResponse{}, c.newError(provider.ErrMalformedResponse, req.Model,
			fmt.Sprintf("%s returned no embeddings", c.name), "embedding response contained an empty data array", 0, nil)
	}

	// Servers are not required to return inputs in order; the caller relies on
	// index alignment with req.Input, so sort explicitly.
	sort.SliceStable(decoded.Data, func(i, j int) bool { return decoded.Data[i].Index < decoded.Data[j].Index })

	out := provider.EmbeddingResponse{Vectors: make([][]float32, 0, len(decoded.Data))}
	for _, d := range decoded.Data {
		out.Vectors = append(out.Vectors, d.Embedding)
	}
	if u := convertUsage(decoded.Usage); u != nil {
		out.Usage = *u
	}
	return out, nil
}
