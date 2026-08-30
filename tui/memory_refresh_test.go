package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/provider"
)

// The system message sent to the model is rebuilt each turn, so a Boop.md that
// changed mid-session (a `/prep`, or a hand edit) reaches the model on the next
// message rather than only after a restart (issue #7).
func TestRequestHistoryRefreshesProjectMemory(t *testing.T) {
	m := newAttachedModel(t, nil)
	m.history = append(m.history, provider.Message{Role: provider.RoleUser, Content: "hello"})

	const marker = "REFRESHED-MEMORY-MARKER"
	root := m.app.Workspace.Root()
	if err := os.WriteFile(filepath.Join(root, "Boop.md"),
		[]byte("# Boop Project Memory\n\n## Goals\n\n"+marker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := m.requestHistory(); strings.Contains(got[0].Content, marker) {
		t.Fatal("memory appeared in the prompt before it was reloaded")
	}

	if err := m.app.ReloadMemory(); err != nil {
		t.Fatalf("ReloadMemory: %v", err)
	}

	got := m.requestHistory()
	if got[0].Role != provider.RoleSystem {
		t.Fatalf("history[0] role = %q, want system", got[0].Role)
	}
	if !strings.Contains(got[0].Content, marker) {
		t.Errorf("system prompt was not refreshed with the new Boop.md:\n%s", got[0].Content)
	}
	// The stored transcript keeps the original system message untouched.
	if strings.Contains(m.history[0].Content, marker) {
		t.Error("requestHistory mutated the stored system message")
	}
}
