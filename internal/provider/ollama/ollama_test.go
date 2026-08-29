package ollama

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/provider"
)

func TestNormalizeBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty falls back to the documented default", "", DefaultBaseURL},
		{"whitespace only", "   ", DefaultBaseURL},
		{"trailing slash removed", "http://box:11434/", "http://box:11434"},
		{"openai suffix removed", "http://box:11434/v1", "http://box:11434"},
		{"openai suffix with slash removed", "http://box:11434/v1/", "http://box:11434"},
		{"bare host gets http", "box:11434", "http://box:11434"},
		{"https preserved", "https://box/v1", "https://box"},
		{"unrelated path preserved", "http://box/ollama", "http://box/ollama"},
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
	if c.keepAlive != DefaultKeepAlive {
		t.Fatalf("keepAlive = %v, want %v", c.keepAlive, DefaultKeepAlive)
	}
}

func TestNewOptions(t *testing.T) {
	t.Parallel()

	c := New("http://box:1/v1", nil,
		WithName("remote-ollama"),
		WithKeepAlive(0), // non-positive is ignored: UnloadModel expresses that
		WithHeaders(map[string]string{"X-Test": "1"}),
	)
	if got := c.Name(); got != "remote-ollama" {
		t.Fatalf("Name() = %q, want remote-ollama", got)
	}
	if c.keepAlive != DefaultKeepAlive {
		t.Fatalf("WithKeepAlive(0) changed residency to %v", c.keepAlive)
	}
}

func TestHealth(t *testing.T) {
	t.Parallel()

	t.Run("version endpoint answers", func(t *testing.T) {
		t.Parallel()
		f := newFake(t)
		f.static(versionPath, `{"version":"0.31.2"}`)
		_, c := f.start()

		if err := c.Health(context.Background()); err != nil {
			t.Fatalf("Health: %v", err)
		}
		if got := f.count(versionPath); got != 1 {
			t.Fatalf("version probed %d times, want 1", got)
		}
	})

	t.Run("falls back to the OpenAI listing when version is missing", func(t *testing.T) {
		t.Parallel()
		f := newFake(t)
		f.static("/v1/models", `{"object":"list","data":[{"id":"m"}]}`)
		_, c := f.start()

		if err := c.Health(context.Background()); err != nil {
			t.Fatalf("Health: %v", err)
		}
	})

	t.Run("reports a server error when both probes fail", func(t *testing.T) {
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

func TestVersion(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	f.static(versionPath, `{"version":"0.31.2"}`)
	_, c := f.start()

	got, err := c.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if got != "0.31.2" {
		t.Fatalf("Version() = %q, want 0.31.2", got)
	}
}
