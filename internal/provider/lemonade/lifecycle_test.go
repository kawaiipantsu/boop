package lemonade

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/boop-dev/boop/internal/provider"
)

func TestLoadModelPreferredBodyShape(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	f.static(api(loadPath), `{"status":"success"}`)
	_, c := f.start()

	if err := c.LoadModel(context.Background(), "Llama-3.2-1B-Instruct-Hybrid"); err != nil {
		t.Fatalf("LoadModel: %v", err)
	}

	bodies := f.rawBodies(api(loadPath))
	if len(bodies) != 1 {
		t.Fatalf("sent %d requests, want 1", len(bodies))
	}
	if bodies[0] != `{"model_name":"Llama-3.2-1B-Instruct-Hybrid"}` {
		t.Fatalf("body = %s", bodies[0])
	}
}

// TestLoadModelFallsBackToModelField exercises the whole reason the inferred
// field name is not trusted: if the server rejects "model_name", the
// OpenAI-conventional "model" is tried before giving up.
func TestLoadModelFallsBackToModelField(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	f.handle(api(loadPath), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if len(f.rawBodies(api(loadPath))) == 1 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"detail":"field required: model"}`)
			return
		}
		_, _ = io.WriteString(w, `{"status":"success"}`)
	})
	_, c := f.start()

	if err := c.LoadModel(context.Background(), "m"); err != nil {
		t.Fatalf("LoadModel: %v", err)
	}

	bodies := f.rawBodies(api(loadPath))
	if len(bodies) != 2 {
		t.Fatalf("sent %d requests, want 2", len(bodies))
	}
	if bodies[0] != `{"model_name":"m"}` || bodies[1] != `{"model":"m"}` {
		t.Fatalf("bodies = %v", bodies)
	}
}

func TestLoadModelUnsupportedEndpoint(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	// No /load route at all: the build has no management API.
	_, c := f.start()

	pe := providerError(t, c.LoadModel(context.Background(), "m"))
	if pe.Category != provider.ErrUnsupportedCapability {
		t.Fatalf("category = %q, want %q", pe.Category, provider.ErrUnsupportedCapability)
	}
	if !strings.Contains(pe.Detail, api(loadPath)) {
		t.Fatalf("detail %q should name the endpoint that was tried", pe.Detail)
	}
	if got := f.count(api(loadPath)); got != 2 {
		t.Fatalf("tried %d body shapes, want 2", got)
	}
}

func TestLoadModelDoesNotRetryRealFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		body   string
		want   provider.ErrorCategory
	}{
		{"server error", http.StatusInternalServerError, `{"detail":"out of memory"}`, provider.ErrServer},
		{"auth", http.StatusUnauthorized, `{"detail":"nope"}`, provider.ErrAuthentication},
		{"rate limited", http.StatusTooManyRequests, `{"detail":"slow down"}`, provider.ErrRateLimited},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			f.status(api(loadPath), tc.status, tc.body)
			_, c := f.start()

			pe := providerError(t, c.LoadModel(context.Background(), "m"))
			if pe.Category != tc.want {
				t.Fatalf("category = %q, want %q", pe.Category, tc.want)
			}
			if got := f.count(api(loadPath)); got != 1 {
				t.Fatalf("made %d attempts; a real failure must not be retried against a guess", got)
			}
		})
	}
}

func TestLoadModelErrorInsideSuccess(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	f.static(api(loadPath), `{"status":"error","error":"model not found"}`)
	_, c := f.start()

	pe := providerError(t, c.LoadModel(context.Background(), "ghost"))
	if pe.Category != provider.ErrServer {
		t.Fatalf("category = %q, want %q", pe.Category, provider.ErrServer)
	}
	if pe.Message != "model not found" {
		t.Fatalf("message = %q", pe.Message)
	}
}

func TestLoadModelRejectsEmptyModel(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	_, c := f.start()

	pe := providerError(t, c.LoadModel(context.Background(), " "))
	if pe.Category != provider.ErrInvalidRequest {
		t.Fatalf("category = %q, want %q", pe.Category, provider.ErrInvalidRequest)
	}
	if got := f.count(api(loadPath)); got != 0 {
		t.Fatal("an empty model id must not reach the network")
	}
}

func TestUnloadModel(t *testing.T) {
	t.Parallel()

	t.Run("names the model when one is given", func(t *testing.T) {
		t.Parallel()
		f := newFake(t)
		f.static(api(unloadPath), `{"status":"success"}`)
		_, c := f.start()

		if err := c.UnloadModel(context.Background(), "m"); err != nil {
			t.Fatalf("UnloadModel: %v", err)
		}
		bodies := f.rawBodies(api(unloadPath))
		if len(bodies) != 1 || bodies[0] != `{"model_name":"m"}` {
			t.Fatalf("bodies = %v", bodies)
		}
	})

	t.Run("an empty id unloads whatever is resident", func(t *testing.T) {
		t.Parallel()
		f := newFake(t)
		f.static(api(unloadPath), `{"status":"success"}`)
		_, c := f.start()

		if err := c.UnloadModel(context.Background(), ""); err != nil {
			t.Fatalf("UnloadModel: %v", err)
		}
		bodies := f.rawBodies(api(unloadPath))
		if len(bodies) != 1 || bodies[0] != `{}` {
			t.Fatalf("bodies = %v", bodies)
		}
	})

	t.Run("falls through to an empty body", func(t *testing.T) {
		t.Parallel()
		f := newFake(t)
		f.handle(api(unloadPath), func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if len(f.rawBodies(api(unloadPath))) < 3 {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = io.WriteString(w, `{"detail":"unexpected field"}`)
				return
			}
			_, _ = io.WriteString(w, `{"status":"success"}`)
		})
		_, c := f.start()

		if err := c.UnloadModel(context.Background(), "m"); err != nil {
			t.Fatalf("UnloadModel: %v", err)
		}
		bodies := f.rawBodies(api(unloadPath))
		if len(bodies) != 3 || bodies[2] != `{}` {
			t.Fatalf("bodies = %v", bodies)
		}
	})

	t.Run("unsupported endpoint", func(t *testing.T) {
		t.Parallel()
		f := newFake(t)
		_, c := f.start()

		pe := providerError(t, c.UnloadModel(context.Background(), "m"))
		if pe.Category != provider.ErrUnsupportedCapability {
			t.Fatalf("category = %q, want %q", pe.Category, provider.ErrUnsupportedCapability)
		}
	})
}

func TestLifecycleUnreachableServer(t *testing.T) {
	t.Parallel()

	c := unreachableClient(t)
	pe := providerError(t, c.LoadModel(context.Background(), "m"))
	if pe.Category != provider.ErrUnavailable {
		t.Fatalf("category = %q, want %q", pe.Category, provider.ErrUnavailable)
	}
	if !strings.Contains(pe.Message, c.Root()) {
		t.Fatalf("message %q does not name the address", pe.Message)
	}
}
