package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/config"
)

func TestAppMemoryReload(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0755); err != nil {
		t.Fatalf("Mkdir .git: %v", err)
	}
	boopPath := filepath.Join(dir, "Boop.md")

	err := os.WriteFile(boopPath, []byte("# Boop Project Memory\n\n## Project\n\nInitial content\n"), 0600)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg := config.Default()

	application, err := New(context.Background(), Options{
		Config:       cfg,
		WorkingDir:   dir,
		DatabasePath: ":memory:",
		LogPath:      ":discard",
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	defer application.Close()

	mem := application.GetMemory()
	if mem == nil {
		t.Fatal("GetMemory() is nil")
	}

	// Update Boop.md on disk
	updated := "# Boop Project Memory\n\n## Project\n\nUpdated content after prep\n"
	if err := os.WriteFile(boopPath, []byte(updated), 0600); err != nil {
		t.Fatalf("WriteFile update: %v", err)
	}

	reloaded, err := application.ReloadMemory()
	if err != nil {
		t.Fatalf("ReloadMemory: %v", err)
	}
	if reloaded == nil {
		t.Fatal("ReloadMemory returned nil")
	}

	memAfter := application.GetMemory()
	rendered := string(memAfter.Render())
	if !strings.Contains(rendered, "Updated content after prep") {
		t.Errorf("GetMemory rendered = %q, want it to contain 'Updated content after prep'", rendered)
	}
}
