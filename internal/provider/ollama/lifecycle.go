package ollama

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kawaiipantsu/boop/internal/provider"
)

// generateRequest is the POST /api/generate body used for residency control.
//
// Prompt is deliberately absent: an empty prompt is what turns the call into a
// pure load. Stream is sent explicitly because /api/generate defaults to a
// newline-delimited stream, and a single JSON object is what this code decodes.
type generateRequest struct {
	Model     string `json:"model"`
	Stream    bool   `json:"stream"`
	KeepAlive any    `json:"keep_alive"`
}

// generateResponse is the acknowledgement Ollama returns for a load or unload.
// DoneReason is "load" or "unload" on current servers and absent on older ones,
// so Done is what the code actually checks.
type generateResponse struct {
	Model      string `json:"model"`
	Response   string `json:"response"`
	Done       bool   `json:"done"`
	DoneReason string `json:"done_reason"`
	Error      string `json:"error"`
}

// LoadModel makes Ollama load the model's weights into memory.
//
// Ollama has no dedicated load endpoint. The idiomatic mechanism — the one
// `ollama run <model> ""` and the official clients use — is a generate request
// carrying no prompt: the server starts the runner, generates nothing, and
// answers immediately with done_reason "load". The keep_alive field then
// decides how long the weights stay resident; see WithKeepAlive.
//
// Preloading matters because the first real request to a cold model otherwise
// pays several seconds of load time that looks like a hung UI.
func (c *Client) LoadModel(ctx context.Context, model string) error {
	return c.setResidency(ctx, model, keepAliveValue(c.keepAlive), "LoadModel", "load")
}

// UnloadModel evicts the model's weights immediately.
//
// This is the same empty generate request with keep_alive 0, which is Ollama's
// documented unload mechanism; the server replies with done_reason "unload".
func (c *Client) UnloadModel(ctx context.Context, model string) error {
	return c.setResidency(ctx, model, 0, "UnloadModel", "unload")
}

// keepAliveValue renders a residency duration for the wire.
//
// Ollama accepts either a Go duration string or a number of seconds. The
// duration string is used because it survives a round trip through logs
// legibly; a non-positive duration collapses to the numeric 0 that means
// "unload now".
func keepAliveValue(d time.Duration) any {
	if d <= 0 {
		return 0
	}
	return d.String()
}

// setResidency performs the load/unload round trip and verifies the answer.
//
// caller and action name the operation in error messages only.
func (c *Client) setResidency(ctx context.Context, model string, keepAlive any, caller, action string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return c.errorf(provider.ErrInvalidRequest, "", "no model selected",
			fmt.Sprintf("%s called with an empty model id", caller))
	}

	var resp generateResponse
	req := generateRequest{Model: model, Stream: false, KeepAlive: keepAlive}
	if err := c.PostJSON(ctx, c.native(generatePath), req, &resp); err != nil {
		return c.unreachable(err)
	}
	if msg := strings.TrimSpace(resp.Error); msg != "" {
		return c.errorf(provider.ErrServer, model, msg,
			fmt.Sprintf("error field in a 200 /api/generate response during %s", action))
	}
	if !resp.Done {
		// Reporting success here would let the caller believe a cold model is
		// hot and blame the resulting stall on the model.
		return c.errorf(provider.ErrMalformedResponse, model,
			fmt.Sprintf("Ollama did not confirm %s of %s", action, model),
			fmt.Sprintf("/api/generate returned done=false, done_reason=%q", resp.DoneReason))
	}
	return nil
}
