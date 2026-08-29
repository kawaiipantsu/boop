package lmstudio

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/boop-dev/boop/internal/provider"
)

func TestNormalizeBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty falls back to the documented default", "", DefaultBaseURL},
		{"whitespace only", "  \t ", DefaultBaseURL},
		{"trailing slash removed", "http://box:1234/", "http://box:1234"},
		{"openai suffix removed", "http://box:1234/v1", "http://box:1234"},
		{"openai suffix with slash removed", "http://box:1234/v1/", "http://box:1234"},
		{"bare host gets http", "box:1234", "http://box:1234"},
		{"https preserved", "https://box/v1", "https://box"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeBaseURL(tc.in); got != tc.want {
				t.Fatalf("normalizeBaseURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNewDefaults(t *testing.T) {
	t.Parallel()

	c := New("", nil)
	if got := c.Name(); got != ProviderName {
		t.Fatalf("Name() = %q, want %q", got, ProviderName)
	}
	if got := c.Root(); got != DefaultBaseURL {
		t.Fatalf("Root() = %q, want %q", got, DefaultBaseURL)
	}
	if want := DefaultBaseURL + OpenAIPath; c.BaseURL() != want {
		t.Fatalf("BaseURL() = %q, want %q", c.BaseURL(), want)
	}
}

func TestHealth(t *testing.T) {
	t.Parallel()

	t.Run("openai listing answers", func(t *testing.T) {
		t.Parallel()
		f := newFake(t)
		f.static("/v1/models", openAIFixture)
		_, c := f.start()

		if err := c.Health(context.Background()); err != nil {
			t.Fatalf("Health: %v", err)
		}
	})

	t.Run("server error surfaces", func(t *testing.T) {
		t.Parallel()
		f := newFake(t)
		f.status("/v1/models", http.StatusInternalServerError, `{"error":"boom"}`)
		_, c := f.start()

		pe := providerError(t, c.Health(context.Background()))
		if pe.Category != provider.ErrServer {
			t.Fatalf("category = %q, want %q", pe.Category, provider.ErrServer)
		}
	})

	t.Run("connection refused names the address and the fix", func(t *testing.T) {
		t.Parallel()
		c := unreachableClient(t)

		pe := providerError(t, c.Health(context.Background()))
		if pe.Category != provider.ErrUnavailable {
			t.Fatalf("category = %q, want %q", pe.Category, provider.ErrUnavailable)
		}
		if !strings.Contains(pe.Message, c.Root()) {
			t.Fatalf("message %q does not name the address %q", pe.Message, c.Root())
		}
		if !strings.Contains(strings.ToLower(pe.Message), "running") {
			t.Fatalf("message %q does not suggest the server may not be running", pe.Message)
		}
	})
}
