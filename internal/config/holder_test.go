package config

import (
	"sync"
	"testing"

	"github.com/kawaiipantsu/boop/internal/permissions"
)

func TestHolderLoadStore(t *testing.T) {
	a := Default()
	h := NewHolder(a)
	if h.Load() != a {
		t.Fatal("Load did not return the stored pointer")
	}
	b := Default()
	b.Provider = "changed"
	h.Store(b)
	if got := h.Load(); got != b || got.Provider != "changed" {
		t.Fatalf("Store/Load round trip failed: %+v", got)
	}
}

// The holder is the seam that makes PUT /api/config safe: readers and a writer
// must not race. Run under -race.
func TestHolderIsRaceFree(t *testing.T) {
	h := NewHolder(Default())
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 200 {
				_ = h.Load().Execution.Mode
			}
		}()
		go func() {
			defer wg.Done()
			for range 200 {
				c := h.Load().Clone()
				c.Execution.Mode = permissions.ModeAuto
				h.Store(c)
			}
		}()
	}
	wg.Wait()
}

func TestConfigCloneIsDeep(t *testing.T) {
	a := Default()
	a.Provider = "ollama"
	a.Fallback = []string{"openai"}
	a.Providers["ollama"] = ProviderConfig{Type: "ollama", Headers: map[string]string{"X": "1"}}

	b := a.Clone()
	b.Provider = "openai"
	b.Fallback[0] = "anthropic"
	b.Providers["ollama"].Headers["X"] = "2"
	b.Providers["new"] = ProviderConfig{Type: "openai"}

	if a.Provider != "ollama" {
		t.Error("scalar field was shared")
	}
	if a.Fallback[0] != "openai" {
		t.Error("slice was shared")
	}
	if a.Providers["ollama"].Headers["X"] != "1" {
		t.Error("nested map was shared")
	}
	if _, ok := a.Providers["new"]; ok {
		t.Error("top-level map was shared")
	}
}

func TestConfigCloneNil(t *testing.T) {
	var c *Config
	if c.Clone() != nil {
		t.Error("Clone(nil) should be nil")
	}
}
