package app

import (
	"os"
	"path/filepath"
	"testing"
)

// The embedded prompt is a copy of prompts/system.md, because go:embed cannot
// reach outside its own package directory. §6 puts the canonical prompts at the
// repository root, so this test pins the two together: without it the copy
// drifts silently and the shipped binary stops matching the reviewed file.
func TestEmbeddedSystemPromptMatchesTheCanonicalFile(t *testing.T) {
	canonical, err := os.ReadFile(filepath.Join("..", "..", "prompts", "system.md"))
	if err != nil {
		t.Fatalf("reading prompts/system.md: %v", err)
	}
	embedded, err := os.ReadFile(filepath.Join("prompts", "system.md"))
	if err != nil {
		t.Fatalf("reading the embedded copy: %v", err)
	}
	if string(canonical) != string(embedded) {
		t.Errorf("prompts/system.md and internal/app/prompts/system.md have diverged.\n" +
			"Run: cp prompts/system.md internal/app/prompts/system.md")
	}
	if DefaultSystemPrompt() == "" {
		t.Error("DefaultSystemPrompt() is empty")
	}
}
