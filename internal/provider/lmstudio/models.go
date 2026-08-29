package lmstudio

import (
	"context"
	"sort"
	"strings"

	"github.com/boop-dev/boop/internal/provider"
)

// ModelState is LM Studio's residency state for a model.
//
// INFERRED: the state vocabulary comes from LM Studio's REST API
// documentation. Any value not listed here is preserved verbatim and simply
// treated as "not loaded", so an unknown state degrades safely.
type ModelState string

const (
	// StateLoaded means the weights currently occupy memory.
	StateLoaded ModelState = "loaded"
	// StateNotLoaded means the model is downloaded but not resident.
	StateNotLoaded ModelState = "not-loaded"
	// StateLoading means a load is in progress.
	StateLoading ModelState = "loading"
)

// Model type values reported by the native listing.
//
// INFERRED, as above. Unknown values fall through to the shared name-based
// detection rather than being guessed at.
const (
	typeLLM       = "llm"
	typeVLM       = "vlm"
	typeEmbedding = "embeddings"
)

// ModelInfo is LM Studio's native per-model record.
//
// Every field beyond ID is INFERRED from LM Studio's REST API documentation.
// They are all optional on the wire: a field the running version does not send
// simply stays zero and the corresponding refinement is skipped.
type ModelInfo struct {
	// ID is the model identifier used in chat requests.
	ID string
	// Type is "llm", "vlm" or "embeddings".
	Type string
	// Publisher is the upstream author, e.g. "lmstudio-community".
	Publisher string
	// Architecture is the model family, e.g. "qwen2".
	Architecture string
	// Quantization is the weight format, e.g. "Q4_K_M".
	Quantization string
	// CompatibilityType is the runtime format, e.g. "gguf" or "mlx".
	CompatibilityType string
	// State is the residency state.
	State ModelState
	// MaxContextLength is the largest context the model supports.
	MaxContextLength int
	// LoadedContextLength is the context the current load was configured with,
	// and is zero unless the model is loaded.
	LoadedContextLength int
}

// Loaded reports whether the model currently occupies memory.
func (m ModelInfo) Loaded() bool { return m.State == StateLoaded }

// restModel is one entry of GET /api/v0/models.
type restModel struct {
	ID                  string `json:"id"`
	Object              string `json:"object"`
	Type                string `json:"type"`
	Publisher           string `json:"publisher"`
	Arch                string `json:"arch"`
	CompatibilityType   string `json:"compatibility_type"`
	Quantization        string `json:"quantization"`
	State               string `json:"state"`
	MaxContextLength    int    `json:"max_context_length"`
	LoadedContextLength int    `json:"loaded_context_length"`
}

// restModelsResponse is the envelope of GET /api/v0/models.
type restModelsResponse struct {
	Data []restModel `json:"data"`
}

func (r restModel) info() ModelInfo {
	return ModelInfo{
		ID:                  strings.TrimSpace(r.ID),
		Type:                strings.ToLower(strings.TrimSpace(r.Type)),
		Publisher:           r.Publisher,
		Architecture:        r.Arch,
		Quantization:        r.Quantization,
		CompatibilityType:   r.CompatibilityType,
		State:               ModelState(strings.ToLower(strings.TrimSpace(r.State))),
		MaxContextLength:    r.MaxContextLength,
		LoadedContextLength: r.LoadedContextLength,
	}
}

// displayName renders the extra identity LM Studio knows about a model, so a
// picker can distinguish two quantizations of the same weights.
func (m ModelInfo) displayName() string {
	extras := make([]string, 0, 3)
	if m.Quantization != "" {
		extras = append(extras, m.Quantization)
	}
	if m.CompatibilityType != "" {
		extras = append(extras, m.CompatibilityType)
	}
	if m.Loaded() {
		extras = append(extras, "loaded")
	}
	if len(extras) == 0 {
		return m.ID
	}
	return m.ID + " (" + strings.Join(extras, ", ") + ")"
}

// NativeModels returns LM Studio's richer model records.
//
// It reports ErrUnsupportedCapability when the running version does not serve
// /api/v0, so a caller that genuinely needs load state can tell "not
// supported" from "not loaded" instead of silently getting a wrong answer.
func (c *Client) NativeModels(ctx context.Context) ([]ModelInfo, error) {
	return c.fetchNative(ctx)
}

// IsLoaded reports whether the model currently occupies memory.
func (c *Client) IsLoaded(ctx context.Context, model string) (bool, error) {
	info, err := c.nativeModel(ctx, model)
	if err != nil {
		return false, err
	}
	return info.Loaded(), nil
}

// nativeModel returns one native record, refreshing the cache once if the
// model is unknown.
func (c *Client) nativeModel(ctx context.Context, model string) (ModelInfo, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return ModelInfo{}, c.errorf(provider.ErrInvalidRequest, "", "no model selected",
			"a model id is required")
	}
	if info, ok := c.cachedNative(model); ok {
		return info, nil
	}
	if _, err := c.NativeModels(ctx); err != nil {
		return ModelInfo{}, err
	}
	if info, ok := c.cachedNative(model); ok {
		return info, nil
	}
	return ModelInfo{}, c.errorf(provider.ErrInvalidRequest, model,
		"LM Studio does not know a model called "+model,
		"model absent from "+restModelsPath)
}

// fetchNative loads GET /api/v0/models and refreshes the cache.
//
// A definitive "not found" is remembered, so an LM Studio build without the
// REST API is probed once rather than on every listing.
func (c *Client) fetchNative(ctx context.Context) ([]ModelInfo, error) {
	if c.restUnsupported() {
		return nil, c.noRESTAPI("")
	}

	var resp restModelsResponse
	if err := c.GetJSON(ctx, c.nativeURL(restModelsPath), &resp); err != nil {
		if cat, ok := provider.CategoryOf(err); ok &&
			(cat == provider.ErrInvalidRequest || cat == provider.ErrMalformedResponse) {
			// The server answered but has no usable REST API: an older build,
			// or a proxy exposing only the OpenAI surface.
			c.markRESTAbsent()
			return nil, c.noRESTAPI("")
		}
		return nil, c.unreachable(err)
	}

	infos := make([]ModelInfo, 0, len(resp.Data))
	for _, rm := range resp.Data {
		info := rm.info()
		if info.ID == "" {
			continue
		}
		infos = append(infos, info)
	}
	if len(infos) == 0 && len(resp.Data) > 0 {
		// Decoded, but nothing usable came out: treat it as an unfamiliar
		// shape rather than as an empty installation.
		c.markRESTAbsent()
		return nil, c.noRESTAPI("")
	}
	c.storeNative(infos)
	sort.Slice(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })
	return infos, nil
}

// noRESTAPI is the typed error for a build without /api/v0.
func (c *Client) noRESTAPI(model string) *provider.Error {
	return c.errorf(provider.ErrUnsupportedCapability, model,
		"this LM Studio version does not expose the "+restModelsPath+" REST API",
		"native model state is unavailable; the OpenAI-compatible endpoints still work")
}

func (c *Client) restUnsupported() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.restAbsent
}

func (c *Client) markRESTAbsent() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.restAbsent = true
}

func (c *Client) cachedNative(model string) (ModelInfo, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	info, ok := c.native[model]
	return info, ok
}

// storeNative replaces the cache wholesale so a deleted model stops being
// reported, and clears the "no REST API" flag because the API evidently works.
func (c *Client) storeNative(infos []ModelInfo) {
	next := make(map[string]ModelInfo, len(infos))
	for _, info := range infos {
		next[info.ID] = info
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.native = next
	c.restAbsent = false
}

// ListModels returns the models LM Studio offers.
//
// The native listing is preferred because it carries the context length, the
// model kind and the load state; when it is unavailable the OpenAI listing
// answers instead, since LM Studio versions differ and losing the extra
// metadata is far better than losing the model list.
func (c *Client) ListModels(ctx context.Context) ([]provider.Model, error) {
	infos, err := c.fetchNative(ctx)
	if err != nil {
		models, fallbackErr := c.Client.ListModels(ctx)
		if fallbackErr != nil {
			return nil, c.unreachable(fallbackErr)
		}
		return models, nil
	}

	out := make([]provider.Model, 0, len(infos))
	for _, info := range infos {
		base, _ := c.Client.Capabilities(ctx, info.ID)
		out = append(out, provider.Model{
			ID:            info.ID,
			Provider:      c.Name(),
			DisplayName:   info.displayName(),
			ContextWindow: info.MaxContextLength,
			Capabilities:  refine(info, base),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Capabilities reports what the given model can do.
//
// The native cache is warmed first so the shared detection sees LM Studio's
// own answer through refineCapabilities; without that, the shared path would
// cache a name-derived guess and keep returning it.
func (c *Client) Capabilities(ctx context.Context, model string) (provider.Capabilities, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, c.errorf(provider.ErrInvalidRequest, "", "no model selected",
			"Capabilities called with an empty model id")
	}
	if _, ok := c.cachedNative(model); !ok && !c.restUnsupported() {
		// Best effort: a build without the REST API is not an error here.
		_, _ = c.fetchNative(ctx)
	}
	return c.Client.Capabilities(ctx, model)
}

// refineCapabilities is the hook the shared OpenAI path calls. It never issues
// a request: it runs inside the shared client's own listing.
func (c *Client) refineCapabilities(model string, base provider.Capabilities) provider.Capabilities {
	info, ok := c.cachedNative(model)
	if !ok {
		return base
	}
	return refine(info, base)
}

// refine folds LM Studio's declared model kind into the detected capabilities.
//
// The kind is treated as authoritative in both directions: an "embeddings"
// model must never be offered for chat, and an "llm" is explicitly not a
// vision model, so a name-derived vision guess is withdrawn. Everything LM
// Studio does not report — tools, reasoning — is left to the shared detection,
// because the REST API says nothing about them and inventing an answer is what
// §8 forbids.
func refine(info ModelInfo, base provider.Capabilities) provider.Capabilities {
	switch info.Type {
	case typeEmbedding:
		return provider.Capabilities{}.Add(provider.CapabilityEmbeddings)
	case typeVLM:
		return base.Add(provider.CapabilityStreaming, provider.CapabilityVision)
	case typeLLM:
		return without(base.Add(provider.CapabilityStreaming), provider.CapabilityVision)
	default:
		return base
	}
}

// without returns a copy of caps with the given capability removed.
func without(caps provider.Capabilities, drop provider.Capability) provider.Capabilities {
	out := make(provider.Capabilities, 0, len(caps))
	for _, c := range caps {
		if c != drop {
			out = append(out, c)
		}
	}
	return out
}
