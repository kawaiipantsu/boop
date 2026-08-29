package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/app"
	"github.com/kawaiipantsu/boop/internal/config"
	"github.com/kawaiipantsu/boop/internal/permissions"
)

// newProjectServer builds a server over a small but realistic tree, so the
// discovery assertions are about real detection rather than about fixtures.
func newProjectServer(t *testing.T, files map[string]string) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	application, err := app.New(t.Context(), app.Options{
		Config:       config.Default(),
		WorkingDir:   root,
		DatabasePath: ":memory:",
		Approver:     permissions.DenyAll(),
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { _ = application.Close() })

	srv := newTestServer(t, func(o *Options) {
		o.App = application
		o.Config = application.Config
	})
	return srv, root
}

// widgetTree is a Go project with a Makefile, a secret and a doc.
func widgetTree() map[string]string {
	return map[string]string{
		"go.mod":         "module github.com/example/widget\n\ngo 1.25\n",
		"main.go":        "package main\n\nfunc main() {}\n",
		"widget_test.go": "package main\n",
		"Makefile":       ".PHONY: build test\nbuild:\n\tgo build ./...\ntest:\n\tgo test ./...\n",
		"README.md":      "# widget\n",
		".env":           "DATABASE_URL=postgres://localhost/widget\n",
	}
}

// TestProjectEndpoint: the Project tab needs root, languages, commands, git
// state, sensitive files and Boop.md from one call.
func TestProjectEndpoint(t *testing.T) {
	srv, root := newProjectServer(t, widgetTree())

	rec, body := doJSON(t, srv, http.MethodGet, "/api/project", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/project = %d (body %s)", rec.Code, body)
	}
	var resp projectResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Project.Root != root {
		t.Errorf("root = %q, want %q", resp.Project.Root, root)
	}
	if resp.Project.PrimaryLang != "Go" {
		t.Errorf("primary_language = %q, want Go (languages %+v)", resp.Project.PrimaryLang, resp.Project.Languages)
	}
	if resp.Project.Name != "widget" {
		t.Errorf("name = %q, want widget", resp.Project.Name)
	}

	var kinds []string
	for _, c := range resp.Project.Commands {
		kinds = append(kinds, c.Kind)
	}
	for _, want := range []string{"build", "test"} {
		if !containsString(kinds, want) {
			t.Errorf("commands %v do not include a %s command", kinds, want)
		}
	}

	found := false
	for _, f := range resp.Project.Sensitive {
		if f.Path == ".env" {
			found = true
			if f.Sensitivity == "" || f.Reason == "" {
				t.Errorf("sensitive .env is missing its grading: %+v", f)
			}
		}
	}
	if !found {
		t.Errorf("sensitive = %+v, want .env flagged", resp.Project.Sensitive)
	}

	// Git is absent here, and must say so rather than claiming a clean tree.
	if resp.Project.Git.Present {
		t.Error("git reported present in a directory with no repository")
	}
	if resp.Project.Git.Remotes == nil {
		t.Error("remotes is null; the frontend should never have to handle that")
	}

	if !resp.Memory.Exists || resp.Memory.Content == "" {
		t.Fatalf("memory = %+v, want the Boop.md content", resp.Memory)
	}
	if !strings.Contains(resp.Memory.Content, "Boop Project Memory") {
		t.Errorf("memory content is not a Boop.md document:\n%s", resp.Memory.Content)
	}
	if resp.Cached {
		t.Error("the first scan reported itself as cached")
	}

	// A second call inside the TTL reuses the scan rather than rewalking.
	_, body = doJSON(t, srv, http.MethodGet, "/api/project", nil)
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Cached {
		t.Error("a repeated call rescanned the tree instead of reusing the result")
	}

	_, body = doJSON(t, srv, http.MethodGet, "/api/project?refresh=true", nil)
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Cached {
		t.Error("refresh=true returned the cached scan")
	}
}

// TestProjectPrep runs the §17 sequence and reports it.
func TestProjectPrep(t *testing.T) {
	files := widgetTree()
	srv, root := newProjectServer(t, files)

	// app.New already created Boop.md, so this run updates rather than
	// creates; either way it must be present and describe the project.
	rec, body := doJSON(t, srv, http.MethodPost, "/api/project/prep", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/project/prep = %d (body %s)", rec.Code, body)
	}
	var resp prepResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.MemoryPath == "" {
		t.Fatal("prep reported no memory path")
	}
	if filepath.Dir(resp.MemoryPath) != root {
		t.Errorf("memory path %q is not in the project root %q", resp.MemoryPath, root)
	}
	if resp.Summary == "" {
		t.Error("prep produced no summary")
	}
	if resp.Project.PrimaryLang != "Go" {
		t.Errorf("primary_language = %q, want Go", resp.Project.PrimaryLang)
	}
	if !resp.Memory.Exists || !strings.Contains(resp.Memory.Content, "widget") {
		t.Errorf("the returned memory does not describe the project:\n%s", resp.Memory.Content)
	}

	onDisk, err := os.ReadFile(resp.MemoryPath)
	if err != nil {
		t.Fatalf("read Boop.md: %v", err)
	}
	if string(onDisk) != resp.Memory.Content {
		t.Error("the response does not match the file prep wrote")
	}

	// Prep must not touch source files (§17).
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("prep modified %s", name)
		}
	}
}

// TestProjectRouting covers the verbs and the unknown subpath.
func TestProjectRouting(t *testing.T) {
	srv, _ := newProjectServer(t, widgetTree())

	tests := []struct {
		name       string
		method     string
		target     string
		wantStatus int
	}{
		{"project is read-only", http.MethodPost, "/api/project", http.StatusMethodNotAllowed},
		{"prep is a write", http.MethodGet, "/api/project/prep", http.StatusMethodNotAllowed},
		{"unknown subpath", http.MethodGet, "/api/project/nope", http.StatusNotFound},
		{"unknown subpath post", http.MethodPost, "/api/project/nope", http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec, body := doJSON(t, srv, tc.method, tc.target, nil)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, body)
			}
			var env errorEnvelope
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Error.Code == "" {
				t.Error("the failure carried no error code")
			}
		})
	}
}

// TestProjectWithoutRuntime: the endpoints must not pretend to work when the
// server has no runtime attached.
func TestProjectWithoutRuntime(t *testing.T) {
	srv := newTestServer(t, nil)
	for _, target := range []string{"/api/project", "/api/project/prep"} {
		method := http.MethodGet
		if strings.HasSuffix(target, "prep") {
			method = http.MethodPost
		}
		rec, body := doJSON(t, srv, method, target, nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503 (body %s)", method, target, rec.Code, body)
		}
	}
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
