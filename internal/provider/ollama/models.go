package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/boop-dev/boop/internal/provider"
)

// tagDetails is the "details" object attached to a model by /api/tags and
// /api/show. context_length and embedding_length appear in /api/tags but not
// in /api/show, which is why showResponse also reads model_info.
type tagDetails struct {
	ParentModel       string   `json:"parent_model"`
	Format            string   `json:"format"`
	Family            string   `json:"family"`
	Families          []string `json:"families"`
	ParameterSize     string   `json:"parameter_size"`
	QuantizationLevel string   `json:"quantization_level"`
	ContextLength     int      `json:"context_length"`
	EmbeddingLength   int      `json:"embedding_length"`
}

// tagModel is one entry of GET /api/tags.
//
// Capabilities is a top-level array of Ollama capability names — not a nested
// or per-family field — and it is authoritative: a model absent from it cannot
// do the thing, and asking anyway produces an HTTP 400.
type tagModel struct {
	Name         string     `json:"name"`
	Model        string     `json:"model"`
	Size         int64      `json:"size"`
	Digest       string     `json:"digest"`
	ModifiedAt   string     `json:"modified_at"`
	Details      tagDetails `json:"details"`
	Capabilities []string   `json:"capabilities"`
}

// identifier returns the id to address the model by. Name is Ollama's canonical
// "repository:tag" form; Model repeats it and is used only as a safety net.
func (t tagModel) identifier() string {
	if n := strings.TrimSpace(t.Name); n != "" {
		return n
	}
	return strings.TrimSpace(t.Model)
}

// tagsResponse is the envelope of GET /api/tags.
type tagsResponse struct {
	Models []tagModel `json:"models"`
}

// showResponse is the part of POST /api/show that Boop uses.
//
// model_info is a free-form map keyed by architecture, e.g.
// "qwen2.context_length" or "qwen2.usage.context_length", so it is decoded as
// generic JSON and searched by key suffix.
type showResponse struct {
	Details      tagDetails     `json:"details"`
	ModelInfo    map[string]any `json:"model_info"`
	Capabilities []string       `json:"capabilities"`
	Parameters   string         `json:"parameters"`
	Template     string         `json:"template"`
	Error        string         `json:"error"`
}

// capabilityFor maps one Ollama capability token onto Boop's vocabulary.
//
// "completion" is Ollama's word for "this is a chat/completion model", and
// every such model streams over /v1/chat/completions, so it maps to
// CapabilityStreaming. Tokens Boop has no concept for (for example "insert",
// fill-in-the-middle) are dropped rather than guessed at.
func capabilityFor(name string) (provider.Capability, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "completion":
		return provider.CapabilityStreaming, true
	case "tools":
		return provider.CapabilityTools, true
	case "embedding":
		return provider.CapabilityEmbeddings, true
	case "vision":
		return provider.CapabilityVision, true
	case "thinking":
		return provider.CapabilityReasoning, true
	default:
		return "", false
	}
}

// mapCapabilities converts Ollama's declared capability list into Boop's set.
//
// The result replaces rather than extends any name-based guess: Ollama knows
// what the model manifest says, and §8 prefers evidence to heuristics. An empty
// declaration yields an empty set so callers can tell "nothing declared" from
// "declared nothing".
func mapCapabilities(declared []string) provider.Capabilities {
	if len(declared) == 0 {
		return nil
	}
	caps := provider.Capabilities{}
	chat := false
	for _, name := range declared {
		mapped, ok := capabilityFor(name)
		if !ok {
			continue
		}
		caps = caps.Add(mapped)
		if mapped == provider.CapabilityStreaming {
			chat = true
		}
	}
	if chat {
		// Ollama constrains decoding to a JSON schema for any chat model via
		// the format parameter, so structured output is a property of the
		// server rather than of the model, and is never listed per model.
		caps = caps.Add(provider.CapabilityStructuredOutput)
	}
	return caps
}

// displayName renders a model id with the size and quantization a user needs
// in order to choose between two tags of the same model.
func displayName(id string, d tagDetails) string {
	extras := make([]string, 0, 2)
	if d.ParameterSize != "" {
		extras = append(extras, d.ParameterSize)
	}
	if d.QuantizationLevel != "" {
		extras = append(extras, d.QuantizationLevel)
	}
	if len(extras) == 0 {
		return id
	}
	return fmt.Sprintf("%s (%s)", id, strings.Join(extras, " "))
}

// ListModels returns the installed models with capabilities and context
// windows attached.
//
// GET /api/tags is preferred over the OpenAI /v1/models listing because it is
// the only endpoint that says what each model can do and how large its context
// is; /v1/models returns ids and nothing else. If the native call fails, the
// OpenAI listing still answers, so a proxied or non-standard deployment
// degrades rather than breaking.
//
// The result is sorted by id so the model picker and the tests see a stable
// order; Ollama returns modification order, which changes under the user.
func (c *Client) ListModels(ctx context.Context) ([]provider.Model, error) {
	tags, err := c.fetchTags(ctx)
	if err != nil {
		models, fallbackErr := c.Client.ListModels(ctx)
		if fallbackErr != nil {
			// The OpenAI listing is the path chat itself uses, so its failure
			// is the more actionable one to report.
			return nil, c.unreachable(fallbackErr)
		}
		return models, nil
	}

	out := make([]provider.Model, 0, len(tags))
	for _, t := range tags {
		id := t.identifier()
		if id == "" {
			continue
		}
		caps := mapCapabilities(t.Capabilities)
		if len(caps) == 0 {
			// Ollama builds older than 0.6 do not report capabilities. Falling
			// through to the shared detection is better than declaring the
			// model featureless, which would make the router skip it.
			caps, _ = c.Client.Capabilities(ctx, id)
		}
		out = append(out, provider.Model{
			ID:            id,
			Provider:      c.Name(),
			DisplayName:   displayName(id, t.Details),
			ContextWindow: t.Details.ContextLength,
			Capabilities:  caps,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// fetchTags loads GET /api/tags and refreshes the cache.
func (c *Client) fetchTags(ctx context.Context) ([]tagModel, error) {
	var resp tagsResponse
	if err := c.GetJSON(ctx, c.native(tagsPath), &resp); err != nil {
		return nil, err
	}
	c.storeTags(resp.Models)
	return resp.Models, nil
}

// storeTags replaces the cache wholesale, so a model deleted on the server
// stops being reported as available.
func (c *Client) storeTags(tags []tagModel) {
	next := make(map[string]tagModel, len(tags))
	for _, t := range tags {
		if id := t.identifier(); id != "" {
			next[id] = t
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tags = next
}

// lookupTag resolves a model reference against the cached listing.
//
// Users type "llama3.1" or "nomic-embed-text" where Ollama reports
// "llama3.1:8b" and "nomic-embed-text:latest", so an exact match is tried
// first, then the implicit ":latest" tag, then the bare repository name — but
// only when that name identifies exactly one installed model, because guessing
// between two tags would silently run the wrong weights.
func (c *Client) lookupTag(model string) (tagModel, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return tagModel{}, false
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	if t, ok := c.tags[model]; ok {
		return t, true
	}
	if !strings.Contains(model, ":") {
		if t, ok := c.tags[model+":latest"]; ok {
			return t, true
		}
	}
	if bare := strings.TrimSuffix(model, ":latest"); bare != model {
		if t, ok := c.tags[bare]; ok {
			return t, true
		}
	}

	repo := model
	if i := strings.IndexByte(repo, ':'); i >= 0 {
		repo = repo[:i]
	}
	var found tagModel
	matches := 0
	for name, t := range c.tags {
		if repositoryOf(name) == repo {
			matches++
			found = t
		}
	}
	if matches == 1 {
		return found, true
	}
	return tagModel{}, false
}

// repositoryOf strips the ":tag" suffix from an Ollama model name.
func repositoryOf(name string) string {
	if i := strings.IndexByte(name, ':'); i >= 0 {
		return name[:i]
	}
	return name
}

// refineCapabilities is the hook the shared OpenAI path calls.
//
// It lets the plain /v1/models listing benefit from anything already learned
// from /api/tags. It never issues a request: it runs inside the shared
// client's own listing, and a nested fetch there would be both surprising and
// recursive.
func (c *Client) refineCapabilities(model string, base provider.Capabilities) provider.Capabilities {
	if t, ok := c.lookupTag(model); ok {
		if caps := mapCapabilities(t.Capabilities); len(caps) > 0 {
			return caps
		}
	}
	return base
}

// Capabilities reports what the given model can do, preferring Ollama's own
// declaration over any guess derived from the model id.
//
// Order: the /api/tags cache, a refreshed /api/tags, a direct /api/show for a
// model the listing did not cover (a bare digest, or one pulled since the last
// refresh), and finally the shared heuristics.
func (c *Client) Capabilities(ctx context.Context, model string) (provider.Capabilities, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, c.errorf(provider.ErrInvalidRequest, "", "no model selected",
			"Capabilities called with an empty model id")
	}

	if t, ok := c.lookupTag(model); ok {
		if caps := mapCapabilities(t.Capabilities); len(caps) > 0 {
			return caps, nil
		}
	}

	if _, err := c.fetchTags(ctx); err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
	} else if t, ok := c.lookupTag(model); ok {
		if caps := mapCapabilities(t.Capabilities); len(caps) > 0 {
			return caps, nil
		}
	}

	shown, showErr := c.Show(ctx, model)
	switch {
	case showErr == nil && len(shown.Capabilities) > 0:
		return shown.Capabilities, nil
	case showErr != nil && ctx.Err() != nil:
		return nil, showErr
	}

	return c.Client.Capabilities(ctx, model)
}

// Show describes a single model using POST /api/show.
//
// It is the way to enrich a model that /api/tags described sparsely: /api/show
// reports the capability list plus a model_info map carrying the architecture's
// true context length, which /api/tags omits for some models.
func (c *Client) Show(ctx context.Context, model string) (provider.Model, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return provider.Model{}, c.errorf(provider.ErrInvalidRequest, "", "no model selected",
			"Show called with an empty model id")
	}

	var resp showResponse
	body := map[string]string{"model": model}
	if err := c.PostJSON(ctx, c.native(showPath), body, &resp); err != nil {
		return provider.Model{}, c.unreachable(err)
	}
	if msg := strings.TrimSpace(resp.Error); msg != "" {
		return provider.Model{}, c.errorf(provider.ErrInvalidRequest, model, msg,
			"error field in a 200 /api/show response")
	}

	return provider.Model{
		ID:            model,
		Provider:      c.Name(),
		DisplayName:   displayName(model, resp.Details),
		ContextWindow: resp.contextLength(),
		Capabilities:  mapCapabilities(resp.Capabilities),
	}, nil
}

// contextLength extracts the model's context window.
//
// details.context_length is used when present. Otherwise model_info is
// searched: its keys are architecture-prefixed ("qwen2.context_length",
// "qwen2.usage.context_length"), so the family prefix wins and, failing that,
// the largest context_length value is taken — map iteration is unordered, and
// picking a maximum keeps the answer deterministic.
func (r showResponse) contextLength() int {
	if r.Details.ContextLength > 0 {
		return r.Details.ContextLength
	}
	prefix := ""
	if fam := strings.ToLower(strings.TrimSpace(r.Details.Family)); fam != "" {
		prefix = fam + "."
	}
	best := 0
	for key, raw := range r.ModelInfo {
		k := strings.ToLower(key)
		if k != "context_length" && !strings.HasSuffix(k, ".context_length") {
			continue
		}
		n := asInt(raw)
		if n <= 0 {
			continue
		}
		if prefix != "" && strings.HasPrefix(k, prefix) {
			return n
		}
		if n > best {
			best = n
		}
	}
	return best
}

// asInt coerces a JSON scalar to an int, tolerating the number, string and
// json.Number forms different Ollama builds have used.
func asInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0
		}
		return int(i)
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil {
			return 0
		}
		return i
	default:
		return 0
	}
}
