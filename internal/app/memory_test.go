package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kawaiipantsu/boop/internal/config"
)

// newMemoryTestApp builds a runtime rooted at dir with an in-memory store.
func newMemoryTestApp(t *testing.T, dir string) *App {
	t.Helper()
	application, err := New(context.Background(), Options{
		Config:       config.Default(),
		WorkingDir:   dir,
		DatabasePath: ":memory:",
		LogPath:      ":discard",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = application.Close() })
	return application
}

// A Boop.md that already exists must actually be loaded — the old code passed
// LoadOrCreate a file path where it wanted a directory, so it always handed
// back an empty document and the model never saw project memory.
func TestNewLoadsAnExistingBoopMd(t *testing.T) {
	dir := t.TempDir()
	const marker = "PROJECT-MEMORY-MARKER-42"
	if err := os.WriteFile(filepath.Join(dir, "Boop.md"),
		[]byte("# Boop Project Memory\n\n## Goals\n\n"+marker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	application := newMemoryTestApp(t, dir)

	mem := application.Memory()
	if mem == nil {
		t.Fatal("Memory() is nil despite a Boop.md on disk")
	}
	if !strings.Contains(string(mem.Render()), marker) {
		t.Errorf("loaded memory does not contain the on-disk content:\n%s", mem.Render())
	}
}

// ReloadMemory picks up a change written after startup, which is what makes a
// mid-session /prep reach the model (issue #7).
func TestReloadMemoryPicksUpChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Boop.md")
	if err := os.WriteFile(path, []byte("# Boop Project Memory\n\n## Goals\n\nfirst\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	application := newMemoryTestApp(t, dir)

	if got := string(application.Memory().Render()); !strings.Contains(got, "first") {
		t.Fatalf("initial memory = %q", got)
	}

	if err := os.WriteFile(path, []byte("# Boop Project Memory\n\n## Goals\n\nsecond\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The snapshot taken before the reload must not change under the reader.
	before := application.Memory()
	if err := application.ReloadMemory(); err != nil {
		t.Fatalf("ReloadMemory: %v", err)
	}
	if strings.Contains(string(before.Render()), "second") {
		t.Error("an already-taken snapshot changed under the caller")
	}
	if got := string(application.Memory().Render()); !strings.Contains(got, "second") {
		t.Errorf("after reload, memory = %q, want the new content", got)
	}
}

// Memory() and ReloadMemory() must be safe to call concurrently: the tool loop
// reads it from turn goroutines while /prep swaps it. Run under -race.
func TestMemoryAccessIsRaceFree(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Boop.md"),
		[]byte("# Boop Project Memory\n\n## Goals\n\nx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	application := newMemoryTestApp(t, dir)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 50 {
				if m := application.Memory(); m != nil {
					_ = m.Render()
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range 50 {
				_ = application.ReloadMemory()
			}
		}()
	}
	wg.Wait()
}
