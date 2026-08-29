package project

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// snapshotTree hashes every file in the tree so a test can prove nothing was
// modified. §17 is explicit: /prep must not touch source code.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return out
}

func prep(t *testing.T, dir string) *Report {
	t.Helper()
	rep, err := PrepWithOptions(context.Background(), dir, Options{DisableGitBinary: true})
	if err != nil {
		t.Fatalf("Prep(%s): %v", dir, err)
	}
	return rep
}

func TestPrepCreatesMemoryWithoutTouchingSources(t *testing.T) {
	files := merge(fakeGit("develop", "git@example.invalid:acme/widget.git"), map[string]string{
		"go.mod":               goModFixture,
		"Makefile":             makefileFixture,
		"main.go":              "package main\n\nfunc main() {}\n",
		"internal/x/x.go":      "package x\n",
		"internal/x/x_test.go": "package x\n",
		"README.md":            "# widget\n",
		"docs/architecture.md": "# architecture\n",
		".env":                 "SECRET=1\n",
		"k8s/deploy.yaml":      "apiVersion: apps/v1\nkind: Deployment\n",
	})
	root := writeTree(t, files)
	before := snapshotTree(t, root)

	rep := prep(t, root)

	if rep.Root != root {
		t.Errorf("Root = %q, want %q", rep.Root, root)
	}
	if !rep.MemoryCreated {
		t.Error("MemoryCreated should be true on first run")
	}
	if rep.MemoryPath != filepath.Join(root, MemoryFileName) {
		t.Errorf("MemoryPath = %q", rep.MemoryPath)
	}

	after := snapshotTree(t, root)
	for path, sum := range before {
		got, ok := after[path]
		if !ok {
			t.Errorf("/prep deleted %s", path)
			continue
		}
		if got != sum {
			t.Errorf("/prep modified %s", path)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok && path != MemoryFileName {
			t.Errorf("/prep created unexpected file %s", path)
		}
	}

	data, err := os.ReadFile(rep.MemoryPath)
	if err != nil {
		t.Fatalf("Boop.md not created: %v", err)
	}
	out := string(data)
	for _, title := range CanonicalSections() {
		containsSubstring(t, out, "## "+title)
	}
	for _, fragment := range []string{
		"Name: widget",
		"Root: " + root,
		"Languages: Go",
		"branch develop",
		"remote origin",
		"make test",
		"`internal/`",
		"docs/architecture.md",
		".env",
		"k8s/deploy.yaml",
		"Production-sensitive",
	} {
		containsSubstring(t, out, fragment)
	}
}

func TestPrepReportSummary(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":       goModFixture,
		"main.go":      "package main\n",
		"main_test.go": "package main\n",
		"Makefile":     makefileFixture,
	})
	rep := prep(t, root)
	summary := rep.Summary()
	for _, fragment := range []string{
		"Project: widget",
		"Root: " + root,
		"Languages: Go",
		"Git: not a repository",
		"Build: make build (from Makefile)",
		"Test: make test (from Makefile)",
		"Memory: created ",
	} {
		containsSubstring(t, summary, fragment)
	}
	if rep.String() != summary {
		t.Error("String should return Summary")
	}
	if !containsWarning(rep.Warnings, "no Git repository") {
		t.Errorf("warnings = %v", rep.Warnings)
	}
}

func TestPrepWarnings(t *testing.T) {
	tests := []struct {
		name         string
		files        map[string]string
		wantWarnings []string
		notWarnings  []string
	}{
		{
			name:         "bare directory",
			files:        map[string]string{"notes.txt": "hi\n"},
			wantWarnings: []string{"no Git repository", "no test files detected", "no test command"},
		},
		{
			name:         "git without remote",
			files:        merge(fakeGit("main", ""), map[string]string{"go.mod": goModFixture, "a_test.go": "package a\n"}),
			wantWarnings: []string{"no Git remote configured", "could not determine working-tree state"},
			notWarnings:  []string{"no Git repository", "no test files detected"},
		},
		{
			name: "production sensitive files",
			files: map[string]string{
				"go.mod":                  goModFixture,
				"a_test.go":               "package a\n",
				"docker-compose.prod.yml": "services: {}\n",
			},
			wantWarnings: []string{"production-sensitive file(s) detected"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rep := prep(t, writeTree(t, tc.files))
			for _, w := range tc.wantWarnings {
				if !containsWarning(rep.Warnings, w) {
					t.Errorf("warnings %v missing %q", rep.Warnings, w)
				}
			}
			for _, w := range tc.notWarnings {
				if containsWarning(rep.Warnings, w) {
					t.Errorf("warnings %v should not contain %q", rep.Warnings, w)
				}
			}
		})
	}
}

func TestPrepPreservesHandEditedMemory(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":       goModFixture,
		"main.go":      "package main\n",
		MemoryFileName: handEdited,
	})
	rep := prep(t, root)
	if rep.MemoryCreated {
		t.Error("MemoryCreated should be false when Boop.md exists")
	}
	data, err := os.ReadFile(rep.MemoryPath)
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	for _, fragment := range []string{
		"Some notes I wrote above the first section.",
		"## Team Conventions",
		"We squash-merge. Ask Sam before touching billing.",
		"- Ship v2",
		"- Keep the parser boring",
		"The parser is intentionally dumb.",
		"## Not A Real Section",
		"### 2026-01-02 09:00 — Bootstrapped",
		"- did things",
	} {
		containsSubstring(t, out, fragment)
	}
	// The stale hand-written Project facts stay; the generated block carries
	// the current ones.
	containsSubstring(t, out, "Name: widget")
	containsSubstring(t, out, "Root: /srv/widget")
	if !strings.Contains(out, managedStart) {
		t.Error("generated block missing")
	}
}

func TestPrepIsIdempotent(t *testing.T) {
	root := writeTree(t, map[string]string{
		"package.json": packageJSONFixture,
		"src/index.ts": "export const x = 1\n",
		MemoryFileName: handEdited,
	})
	prep(t, root)
	first, err := os.ReadFile(filepath.Join(root, MemoryFileName))
	if err != nil {
		t.Fatal(err)
	}
	prep(t, root)
	second, err := os.ReadFile(filepath.Join(root, MemoryFileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("second /prep changed Boop.md:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	if n := strings.Count(string(second), managedStart); n != 5 {
		t.Errorf("expected 5 generated blocks, got %d", n)
	}
}

func TestPrepFromSubdirectoryUsesRoot(t *testing.T) {
	root := writeTree(t, merge(fakeGit("main", ""), map[string]string{
		"go.mod":          goModFixture,
		"internal/x/x.go": "package x\n",
	}))
	rep := prep(t, filepath.Join(root, "internal", "x"))
	if rep.Root != root {
		t.Errorf("Root = %q, want %q", rep.Root, root)
	}
	if _, err := os.Stat(filepath.Join(root, MemoryFileName)); err != nil {
		t.Errorf("Boop.md not written at the project root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "internal", "x", MemoryFileName)); !os.IsNotExist(err) {
		t.Error("Boop.md must not be written in a subdirectory")
	}
}

func TestPrepUserEditsSurviveRegeneration(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":  goModFixture,
		"main.go": "package main\n",
	})
	prep(t, root)

	// The user adds prose to a generated section and a decision of their own.
	m, err := LoadOrCreate(root)
	if err != nil {
		t.Fatal(err)
	}
	sec := m.Document().Section(SectionUsefulCommands)
	sec.SetText(sec.Text() + "\n\nAlso run `make release-check` before tagging.")
	m.AppendDecision("Pin the toolchain")
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}

	// A new manifest appears, so regenerated facts must change.
	writeInto(t, root, map[string]string{"Makefile": makefileFixture})
	prep(t, root)

	data, err := os.ReadFile(filepath.Join(root, MemoryFileName))
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	containsSubstring(t, out, "Also run `make release-check` before tagging.")
	containsSubstring(t, out, "Pin the toolchain")
	containsSubstring(t, out, "`make build`")
}

func TestPrepContextCancelled(t *testing.T) {
	root := writeTree(t, map[string]string{"go.mod": goModFixture})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Prep(ctx, root); err == nil {
		t.Fatal("expected cancellation error")
	}
	if _, err := os.Stat(filepath.Join(root, MemoryFileName)); !os.IsNotExist(err) {
		t.Error("cancelled /prep must not write Boop.md")
	}
}

func containsWarning(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}
