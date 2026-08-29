package lemonade

import (
	"context"
	"fmt"
	"strings"

	"github.com/boop-dev/boop/internal/provider"
)

// loadRequest is the body sent to the native load endpoint.
//
// INFERRED: Lemonade's management API names the field "model_name". Because
// that is unverified, requestBodies below also tries the OpenAI-conventional
// "model" before giving up, so a rename on either side does not break loading.
type loadRequest struct {
	ModelName string `json:"model_name,omitempty"`
	Model     string `json:"model,omitempty"`
}

// lifecycleResponse is read leniently: a 2xx is taken as success unless the
// body carries an explicit error, which some servers do return alongside 200.
// Both field names are covered because Lemonade is FastAPI-based and FastAPI
// conventionally reports failures under "detail".
type lifecycleResponse struct {
	Error  string `json:"error"`
	Detail string `json:"detail"`
}

// requestBodies returns the body shapes to try, in order of confidence.
//
// Sending both field names in one request was rejected as an approach: a
// strict schema would refuse the unknown one and the whole call would fail, so
// the shapes are attempted one at a time instead.
func requestBodies(model string) []any {
	return []any{
		loadRequest{ModelName: model},
		loadRequest{Model: model},
	}
}

// LoadModel asks Lemonade to load a model into memory.
//
// INFERRED endpoint: POST <APIPath>/load. Lemonade holds one model resident at
// a time and loading is what makes the first request fast rather than
// multi-second, which is why this is worth doing ahead of a conversation.
//
// Two request shapes are attempted, and only a definitive rejection of the
// first ("invalid request") triggers the second — an outage or a server error
// is reported immediately rather than retried against a guess.
func (c *Client) LoadModel(ctx context.Context, model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return c.errorf(provider.ErrInvalidRequest, "", "no model selected",
			"LoadModel called with an empty model id")
	}
	return c.lifecycleCall(ctx, loadPath, model, requestBodies(model), "load")
}

// UnloadModel asks Lemonade to release the resident model.
//
// INFERRED endpoint: POST <APIPath>/unload. Lemonade keeps a single model
// resident, so the naming may be optional there; the model name is sent first
// and an empty body is tried if the server rejects that shape.
func (c *Client) UnloadModel(ctx context.Context, model string) error {
	model = strings.TrimSpace(model)
	// Unloading "whatever is loaded" is a legitimate request for a
	// single-slot server, so an empty id is not an error here.
	bodies := []any{struct{}{}}
	if model != "" {
		bodies = append(requestBodies(model), struct{}{})
	}
	return c.lifecycleCall(ctx, unloadPath, model, bodies, "unload")
}

// lifecycleCall posts each candidate body until one is accepted.
//
// action names the operation in error messages only.
func (c *Client) lifecycleCall(ctx context.Context, path, model string, bodies []any, action string) error {
	var lastErr error
	for _, body := range bodies {
		var resp lifecycleResponse
		err := c.PostJSON(ctx, c.apiURL(path), body, &resp)
		if err == nil {
			if msg := firstNonEmpty(resp.Error, resp.Detail); msg != "" {
				return c.errorf(provider.ErrServer, model, msg,
					fmt.Sprintf("error field in a 2xx %s response", path))
			}
			return nil
		}

		lastErr = err
		cat, ok := provider.CategoryOf(err)
		if !ok {
			return err
		}
		switch cat {
		case provider.ErrInvalidRequest:
			// The server answered and refused the shape: worth one more try
			// with the alternative field name. A 404 lands here too and is
			// turned into an explicit "not supported" below.
			continue
		default:
			// Outage, timeout, auth, server error: retrying a different body
			// cannot help and would only delay the real answer.
			return c.unreachable(err)
		}
	}

	return c.unsupported(model, path, action, lastErr)
}

// unsupported reports that every attempted shape was refused.
//
// The endpoint is inferred, so the error says which path was tried: that is
// what a user or maintainer needs in order to correct it, and §57 wants a
// useful message rather than a transport dump.
func (c *Client) unsupported(model, path, action string, cause error) error {
	detail := fmt.Sprintf("POST %s was refused for every known request shape", c.apiURL(path))
	if cause != nil {
		detail += "; last error: " + cause.Error()
	}
	err := c.errorf(provider.ErrUnsupportedCapability, model,
		fmt.Sprintf("this Lemonade server does not support %sing a model over its management API", action),
		detail)
	err.Err = cause
	return err
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
