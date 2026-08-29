package lmstudio

import (
	"context"
	"fmt"
	"strings"

	"github.com/boop-dev/boop/internal/provider"
)

// jitProbeText is the shortest prompt that still forces LM Studio to bring a
// model into memory. Its content is irrelevant; only the load is wanted.
const jitProbeText = "."

// LoadModel brings a model into memory.
//
// LM Studio has no documented HTTP endpoint that loads a model, so this uses
// the mechanism the server itself provides: just-in-time loading. Any request
// naming a model that is not resident causes LM Studio to load it, so a
// one-token completion (or a one-string embedding, for embedding models) is
// issued and discarded.
//
// When the /api/v0 REST API is available the result is verified rather than
// assumed: the model's state must read "loaded" afterwards. If JIT loading has
// been switched off in LM Studio, the probe request fails and that failure is
// returned unchanged, which is the honest answer.
//
// The load is skipped entirely when the model is already resident.
func (c *Client) LoadModel(ctx context.Context, model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return c.errorf(provider.ErrInvalidRequest, "", "no model selected",
			"LoadModel called with an empty model id")
	}

	// Best effort: without the REST API there is nothing to check against, and
	// that is not a reason to refuse to try.
	info, infoErr := c.nativeModel(ctx, model)
	if infoErr == nil && info.Loaded() {
		return nil
	}

	kind := ""
	if infoErr == nil {
		kind = info.Type
	}
	if err := c.jitLoad(ctx, model, kind); err != nil {
		return err
	}

	if infoErr != nil {
		// No REST API, so no way to confirm. The probe request succeeded,
		// which is the strongest evidence available.
		return nil
	}
	after, err := c.nativeModel(ctx, model)
	if err != nil {
		return nil
	}
	if !after.Loaded() {
		return c.errorf(provider.ErrUnavailable, model,
			fmt.Sprintf("LM Studio did not load %s", model),
			fmt.Sprintf("state is %q after a just-in-time load request; check that JIT loading is enabled in LM Studio", after.State))
	}
	return nil
}

// jitLoad issues the smallest request that makes LM Studio load the model.
//
// The request kind has to match the model: sending a chat completion to an
// embedding model fails with a request error that would look like a load
// failure, so the declared type picks the endpoint when it is known.
func (c *Client) jitLoad(ctx context.Context, model, kind string) error {
	// nativeModel caches by id, and the state it holds is now stale.
	defer c.invalidateNative(model)

	if kind == typeEmbedding {
		body := map[string]any{"model": model, "input": []string{jitProbeText}}
		if err := c.PostJSON(ctx, "/embeddings", body, nil); err != nil {
			return c.unreachable(err)
		}
		return nil
	}

	body := map[string]any{
		"model":       model,
		"messages":    []map[string]string{{"role": "user", "content": jitProbeText}},
		"max_tokens":  1,
		"temperature": 0,
		"stream":      false,
	}
	if err := c.PostJSON(ctx, "/chat/completions", body, nil); err != nil {
		return c.unreachable(err)
	}
	return nil
}

// invalidateNative drops one cached record so the next read re-fetches it.
func (c *Client) invalidateNative(model string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.native, model)
}

// UnloadModel is not supported over LM Studio's HTTP API.
//
// LM Studio exposes no unload endpoint: eviction is driven by the `lms` CLI
// (`lms unload <model>`), by the desktop app, or by a per-model TTL configured
// in the server settings. PROJECT.md §7 allows an adapter to decline an
// optional capability, and §57 requires the failure to be a normalized,
// explicable error — reporting success while the model stays resident would
// make the caller mis-plan its memory budget, which is worse than an honest
// refusal.
//
// The method exists so that Client satisfies provider.ModelLifecycleProvider
// and LoadModel is usable; callers should treat ErrUnsupportedCapability here
// as "leave it loaded".
func (c *Client) UnloadModel(ctx context.Context, model string) error {
	_ = ctx
	return c.errorf(provider.ErrUnsupportedCapability, strings.TrimSpace(model),
		"LM Studio cannot unload a model over HTTP",
		"no unload endpoint exists; use `lms unload "+strings.TrimSpace(model)+"`, the LM Studio app, or configure a model TTL")
}
