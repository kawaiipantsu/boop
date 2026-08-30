package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// The embedded prompts are copies: go:embed cannot reach outside its own
// package directory, but §6 puts the canonical prompts at the repository root.
// Without this test the copies drift and the shipped binary stops matching the
// reviewed files.
func TestEmbeddedPromptsMatchTheCanonicalFiles(t *testing.T) {
	for _, tc := range []struct{ canonical, embedded string }{
		{"planner.md", "planner.md"},
		{"agent.md", "worker.md"},
		{"reviewer.md", "reviewer.md"},
	} {
		t.Run(tc.canonical, func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join("..", "..", "prompts", tc.canonical))
			if err != nil {
				t.Fatalf("reading prompts/%s: %v", tc.canonical, err)
			}
			got, err := os.ReadFile(filepath.Join("prompts", tc.embedded))
			if err != nil {
				t.Fatalf("reading the embedded copy: %v", err)
			}
			if string(want) != string(got) {
				t.Errorf("prompts/%s and internal/agent/prompts/%s have diverged.\n"+
					"Run: cp prompts/%s internal/agent/prompts/%s",
					tc.canonical, tc.embedded, tc.canonical, tc.embedded)
			}
		})
	}
}
