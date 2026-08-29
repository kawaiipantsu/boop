package project

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree materialises a fixture project under a fresh temp dir.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	writeInto(t, root, files)
	return root
}

func writeInto(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

// fakeGit writes a .git directory that is readable without the git binary.
func fakeGit(branch, remoteURL string) map[string]string {
	files := map[string]string{
		".git/HEAD":                 "ref: refs/heads/" + branch + "\n",
		".git/refs/heads/" + branch: "1111111111111111111111111111111111111111\n",
	}
	if remoteURL != "" {
		files[".git/config"] = "[core]\n\trepositoryformatversion = 0\n[remote \"origin\"]\n\turl = " + remoteURL + "\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
	} else {
		files[".git/config"] = "[core]\n\trepositoryformatversion = 0\n"
	}
	return files
}

func merge(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

const goModFixture = `module github.com/example/widget

go 1.25.0

require (
	github.com/spf13/cobra v1.8.0
	github.com/charmbracelet/bubbletea v1.3.10
)
`

const packageJSONFixture = `{
  "name": "web-widget",
  "scripts": {
    "build": "vite build",
    "test": "vitest run",
    "lint": "eslint .",
    "format": "prettier --write ."
  },
  "dependencies": {"react": "^18.0.0", "next": "^14.0.0"},
  "devDependencies": {"vitest": "^1.0.0", "eslint": "^8.0.0"}
}
`

const makefileFixture = `.PHONY: build test lint fmt

build:
	go build ./...

test:
	go test ./...

lint:
	golangci-lint run

fmt:
	gofmt -w .

VERSION := 1.0
`

func discover(t *testing.T, dir string) *Info {
	t.Helper()
	info, err := Discover(context.Background(), dir, Options{DisableGitBinary: true})
	if err != nil {
		t.Fatalf("Discover(%s): %v", dir, err)
	}
	return info
}

func TestFindRoot(t *testing.T) {
	goRoot := writeTree(t, map[string]string{
		"go.mod":            goModFixture,
		"internal/a/a.go":   "package a\n",
		"internal/a/go.mod": "module nested\n",
	})
	gitRoot := writeTree(t, merge(fakeGit("main", ""), map[string]string{
		"services/api/go.mod": "module api\n",
		"services/api/m.go":   "package main\n",
	}))
	bare := writeTree(t, map[string]string{"notes.txt": "hello\n"})

	tests := []struct {
		name string
		from string
		want string
	}{
		{"module root from root", goRoot, goRoot},
		{"nearest manifest from subdir", filepath.Join(goRoot, "internal", "a"), filepath.Join(goRoot, "internal", "a")},
		{"git root beats nested manifest", filepath.Join(gitRoot, "services", "api"), gitRoot},
		{"bare directory is its own root", bare, bare},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FindRoot(tc.from)
			if err != nil {
				t.Fatalf("FindRoot: %v", err)
			}
			if got != tc.want {
				t.Errorf("FindRoot(%s) = %s, want %s", tc.from, got, tc.want)
			}
		})
	}

	t.Run("missing directory errors", func(t *testing.T) {
		if _, err := FindRoot(filepath.Join(bare, "nope")); err == nil {
			t.Fatal("expected error for missing directory")
		}
	})
}

func TestDiscoverProjectTypes(t *testing.T) {
	tests := []struct {
		name          string
		files         map[string]string
		wantName      string
		wantLang      string
		wantFramework []string
		wantCommands  map[CommandKind]string
		wantMarkers   []string
	}{
		{
			name: "go module",
			files: map[string]string{
				"go.mod":                 goModFixture,
				"main.go":                "package main\n\nfunc main() {}\n",
				"internal/x/x.go":        "package x\n",
				"internal/x/x_test.go":   "package x\n",
				"cmd/widget/main.go":     "package main\n",
				"scripts/build-cross.sh": "#!/bin/sh\n",
			},
			wantName:      "widget",
			wantLang:      "Go",
			wantFramework: []string{"Bubble Tea", "Cobra"},
			wantCommands: map[CommandKind]string{
				KindBuild:  "go build ./...",
				KindTest:   "go test ./...",
				KindLint:   "go vet ./...",
				KindFormat: "gofmt -w .",
			},
			wantMarkers: []string{"go.mod"},
		},
		{
			name: "node project",
			files: map[string]string{
				"package.json":    packageJSONFixture,
				"pnpm-lock.yaml":  "lockfileVersion: 6.0\n",
				"src/index.ts":    "export const x = 1\n",
				"src/app.tsx":     "export default null\n",
				"src/app.test.ts": "test('x', () => {})\n",
			},
			wantName:      "web-widget",
			wantLang:      "TypeScript",
			wantFramework: []string{"Next.js", "React", "Vitest"},
			wantCommands: map[CommandKind]string{
				KindBuild:  "pnpm run build",
				KindTest:   "pnpm run test",
				KindLint:   "pnpm run lint",
				KindFormat: "pnpm run format",
			},
			wantMarkers: []string{"package.json"},
		},
		{
			name: "makefile project wins over inferred",
			files: map[string]string{
				"Makefile":     makefileFixture,
				"go.mod":       goModFixture,
				"main.go":      "package main\n",
				"main_test.go": "package main\n",
			},
			wantName: "widget",
			wantLang: "Go",
			wantCommands: map[CommandKind]string{
				KindBuild:  "make build",
				KindTest:   "make test",
				KindLint:   "make lint",
				KindFormat: "make fmt",
			},
			wantMarkers: []string{"go.mod"},
		},
		{
			name: "rust crate",
			files: map[string]string{
				"Cargo.toml":   "[package]\nname = \"crab\"\nversion = \"0.1.0\"\n\n[dependencies]\naxum = \"0.7\"\ntokio = { version = \"1\", features = [\"full\"] }\n",
				"src/main.rs":  "fn main() {}\n",
				"tests/api.rs": "#[test]\nfn t() {}\n",
			},
			wantName:      "crab",
			wantLang:      "Rust",
			wantFramework: []string{"Axum", "Tokio"},
			wantCommands: map[CommandKind]string{
				KindBuild:  "cargo build",
				KindTest:   "cargo test",
				KindLint:   "cargo clippy",
				KindFormat: "cargo fmt",
			},
			wantMarkers: []string{"Cargo.toml"},
		},
		{
			name: "python project",
			files: map[string]string{
				"pyproject.toml":    "[project]\nname = \"snake\"\ndependencies = [\"django>=5\"]\n\n[build-system]\nrequires = [\"setuptools\"]\n\n[tool.ruff]\nline-length = 100\n",
				"src/app.py":        "print('hi')\n",
				"tests/test_app.py": "def test_x(): pass\n",
			},
			wantName:      "snake",
			wantLang:      "Python",
			wantFramework: []string{"Django", "Ruff"},
			wantCommands: map[CommandKind]string{
				KindTest:   "pytest",
				KindLint:   "ruff check .",
				KindFormat: "ruff format .",
				KindBuild:  "python -m build",
			},
			wantMarkers: []string{"pyproject.toml"},
		},
		{
			name:        "bare directory",
			files:       map[string]string{"notes.txt": "just notes\n"},
			wantMarkers: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := writeTree(t, tc.files)
			info := discover(t, root)

			if tc.wantName != "" && info.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", info.Name, tc.wantName)
			}
			if tc.wantLang != "" && info.PrimaryLanguage() != tc.wantLang {
				t.Errorf("PrimaryLanguage = %q, want %q (languages: %+v)", info.PrimaryLanguage(), tc.wantLang, info.Languages)
			}
			for _, f := range tc.wantFramework {
				if !contains(info.Frameworks, f) {
					t.Errorf("frameworks %v missing %q", info.Frameworks, f)
				}
			}
			for kind, want := range tc.wantCommands {
				got, ok := info.Command(kind)
				if !ok {
					t.Errorf("no %s command detected (commands: %+v)", kind, info.Commands)
					continue
				}
				if got.Line != want {
					t.Errorf("%s command = %q, want %q", kind, got.Line, want)
				}
			}
			if len(tc.wantMarkers) != len(info.Markers) {
				t.Errorf("markers = %v, want %v", info.Markers, tc.wantMarkers)
			}
			if tc.name == "bare directory" {
				if len(info.Commands) != 0 {
					t.Errorf("bare directory should have no commands, got %+v", info.Commands)
				}
				if info.HasTests() {
					t.Error("bare directory should have no tests")
				}
			}
		})
	}
}

func TestDiscoverCommandsPreferDeclared(t *testing.T) {
	root := writeTree(t, map[string]string{
		"Makefile":      makefileFixture,
		"go.mod":        goModFixture,
		"main.go":       "package main\n",
		".golangci.yml": "linters:\n  enable: [govet]\n",
	})
	info := discover(t, root)

	testCmds := info.CommandsFor(KindTest)
	if len(testCmds) < 2 {
		t.Fatalf("expected declared and inferred test commands, got %+v", testCmds)
	}
	if testCmds[0].Inferred {
		t.Errorf("first test command should be declared, got %+v", testCmds[0])
	}
	if testCmds[0].Source != "Makefile" {
		t.Errorf("first test command source = %q, want Makefile", testCmds[0].Source)
	}
	for i := 1; i < len(testCmds); i++ {
		if !testCmds[i].Inferred && testCmds[i-1].Inferred {
			t.Errorf("declared command %+v ranked after inferred %+v", testCmds[i], testCmds[i-1])
		}
	}
	lint, _ := info.Command(KindLint)
	if lint.Line != "make lint" {
		t.Errorf("lint = %q, want make lint", lint.Line)
	}
	if !containsCommand(info.Commands, "golangci-lint run") {
		t.Errorf("expected golangci-lint from config file, got %+v", info.Commands)
	}
}

func TestDiscoverTestDetection(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":        goModFixture,
		"a.go":          "package a\n",
		"a_test.go":     "package a\n",
		"tests/e2e.go":  "package tests\n",
		"src/x.test.ts": "test\n",
		"docs/notes.md": "# notes\n",
	})
	info := discover(t, root)
	if !info.HasTests() {
		t.Fatal("expected tests to be detected")
	}
	if info.TestFiles < 3 {
		t.Errorf("TestFiles = %d, want >= 3", info.TestFiles)
	}
	if !contains(info.Docs, "docs/notes.md") {
		t.Errorf("docs = %v, want docs/notes.md", info.Docs)
	}
}

func TestDiscoverSkipsDependencyDirectories(t *testing.T) {
	root := writeTree(t, map[string]string{
		"package.json":               packageJSONFixture,
		"src/index.ts":               "export const a = 1\n",
		"node_modules/left-pad/i.js": "module.exports = 1\n",
		"node_modules/left-pad/x.ts": "export const b = 2\n",
		"vendor/foo/bar.go":          "package bar\n",
	})
	info := discover(t, root)
	for _, l := range info.Languages {
		if l.Name == "Go" {
			t.Errorf("vendor/ should not be scanned, got languages %+v", info.Languages)
		}
	}
	if info.FileCount > 4 {
		t.Errorf("FileCount = %d, expected node_modules to be skipped", info.FileCount)
	}
}

func TestDetectSensitiveFiles(t *testing.T) {
	const k8sManifest = "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api\n"
	const workflow = "name: ci\njobs:\n  deploy:\n    steps:\n      - run: kubectl apply -f k8s/\n"
	const plainWorkflow = "name: ci\njobs:\n  unit:\n    steps:\n      - run: go test ./...\n"

	root := writeTree(t, map[string]string{
		".env":                         "DB_PASSWORD=hunter2\n",
		".env.example":                 "DB_PASSWORD=\n",
		"docker-compose.prod.yml":      "services: {}\n",
		"docker-compose.yml":           "services: {}\n",
		"k8s/deployment.yaml":          k8sManifest,
		"config/app.yaml":              "debug: true\n",
		"infra/main.tf":                "resource \"aws_instance\" \"a\" {}\n",
		"infra/terraform.tfstate":      "{}\n",
		"scripts/deploy.sh":            "#!/bin/sh\nssh prod\n",
		"scripts/dev.sh":               "#!/bin/sh\n",
		"deploy/systemd/boop.service":  "[Unit]\n",
		"deploy/nginx.conf":            "server {}\n",
		"ansible/playbook.yml":         "- hosts: all\n",
		".github/workflows/deploy.yml": workflow,
		".github/workflows/test.yml":   plainWorkflow,
		"certs/server.pem":             "-----BEGIN CERTIFICATE-----\n",
		"credentials.json":             "{}\n",
		"README.md":                    "# hi\n",
	})
	info := discover(t, root)

	byPath := map[string]SensitiveFile{}
	for _, f := range info.Sensitive {
		byPath[f.Path] = f
	}

	wantFlagged := map[string]struct {
		category string
		sev      Sensitivity
	}{
		".env":                         {CategoryEnvironment, SensitivityCritical},
		"docker-compose.prod.yml":      {CategoryDeployment, SensitivityHigh},
		"k8s/deployment.yaml":          {CategoryKubernetes, SensitivityHigh},
		"infra/main.tf":                {CategoryInfrastructure, SensitivityHigh},
		"infra/terraform.tfstate":      {CategoryInfrastructure, SensitivityCritical},
		"scripts/deploy.sh":            {CategoryDeployment, SensitivityHigh},
		"deploy/systemd/boop.service":  {CategoryService, SensitivityMedium},
		"deploy/nginx.conf":            {CategoryWebServer, SensitivityMedium},
		"ansible/playbook.yml":         {CategoryProvisioning, SensitivityHigh},
		".github/workflows/deploy.yml": {CategoryCI, SensitivityHigh},
		"certs/server.pem":             {CategorySecret, SensitivityCritical},
		"credentials.json":             {CategorySecret, SensitivityCritical},
	}
	for path, want := range wantFlagged {
		got, ok := byPath[path]
		if !ok {
			t.Errorf("%s was not flagged as production-sensitive", path)
			continue
		}
		if got.Category != want.category {
			t.Errorf("%s category = %q, want %q", path, got.Category, want.category)
		}
		if got.Sensitivity != want.sev {
			t.Errorf("%s sensitivity = %q, want %q", path, got.Sensitivity, want.sev)
		}
		if got.Reason == "" {
			t.Errorf("%s has no reason", path)
		}
	}

	for _, path := range []string{".env.example", "README.md", "scripts/dev.sh", "config/app.yaml", ".github/workflows/test.yml", "docker-compose.yml"} {
		if _, ok := byPath[path]; ok {
			t.Errorf("%s should not be flagged as production-sensitive", path)
		}
	}

	// Most dangerous first.
	for i := 1; i < len(info.Sensitive); i++ {
		if sensitivityRank(info.Sensitive[i-1].Sensitivity) < sensitivityRank(info.Sensitive[i].Sensitivity) {
			t.Fatalf("sensitive files not sorted by severity: %+v", info.Sensitive)
		}
	}
}

func TestReadGitStateWithoutBinary(t *testing.T) {
	tests := []struct {
		name        string
		files       map[string]string
		wantPresent bool
		wantBranch  string
		wantDetach  bool
		wantHead    string
		wantRemotes []Remote
	}{
		{
			name:        "branch and remote",
			files:       merge(fakeGit("develop", "git@github.com:example/widget.git"), map[string]string{"go.mod": goModFixture}),
			wantPresent: true,
			wantBranch:  "develop",
			wantHead:    "1111111111111111111111111111111111111111",
			wantRemotes: []Remote{{Name: "origin", URL: "git@github.com:example/widget.git"}},
		},
		{
			name: "packed refs",
			files: map[string]string{
				"go.mod":           goModFixture,
				".git/HEAD":        "ref: refs/heads/main\n",
				".git/config":      "[core]\n",
				".git/packed-refs": "# pack-refs with: peeled fully-peeled sorted \nabc1234abc1234abc1234abc1234abc1234abc12 refs/heads/main\n",
			},
			wantPresent: true,
			wantBranch:  "main",
			wantHead:    "abc1234abc1234abc1234abc1234abc1234abc12",
		},
		{
			name: "detached head",
			files: map[string]string{
				"go.mod":      goModFixture,
				".git/HEAD":   "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef\n",
				".git/config": "[core]\n",
			},
			wantPresent: true,
			wantDetach:  true,
			wantHead:    "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		},
		{
			name:        "no repository",
			files:       map[string]string{"go.mod": goModFixture},
			wantPresent: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := writeTree(t, tc.files)
			info := discover(t, root)
			g := info.Git
			if g.Present != tc.wantPresent {
				t.Fatalf("Present = %v, want %v", g.Present, tc.wantPresent)
			}
			if g.Branch != tc.wantBranch {
				t.Errorf("Branch = %q, want %q", g.Branch, tc.wantBranch)
			}
			if g.Detached != tc.wantDetach {
				t.Errorf("Detached = %v, want %v", g.Detached, tc.wantDetach)
			}
			if g.Head != tc.wantHead {
				t.Errorf("Head = %q, want %q", g.Head, tc.wantHead)
			}
			if len(g.Remotes) != len(tc.wantRemotes) {
				t.Fatalf("Remotes = %+v, want %+v", g.Remotes, tc.wantRemotes)
			}
			for i, r := range tc.wantRemotes {
				if g.Remotes[i] != r {
					t.Errorf("Remotes[%d] = %+v, want %+v", i, g.Remotes[i], r)
				}
			}
			if g.HasRemote() != (len(tc.wantRemotes) > 0) {
				t.Errorf("HasRemote = %v", g.HasRemote())
			}
			// Without the git binary the working-tree state is unknown, never
			// silently reported as clean.
			if g.DirtyKnown {
				t.Error("DirtyKnown should be false when the git binary is disabled")
			}
		})
	}
}

func TestReadGitWorktreeFile(t *testing.T) {
	root := t.TempDir()
	realGit := filepath.Join(root, "gitdir")
	writeInto(t, root, map[string]string{
		"work/go.mod":                 goModFixture,
		"gitdir/HEAD":                 "ref: refs/heads/feature/x\n",
		"gitdir/config":               "[remote \"upstream\"]\n\turl = https://example.invalid/x.git\n",
		"gitdir/refs/heads/feature/x": "cafebabecafebabecafebabecafebabecafebabe\n",
	})
	if err := os.WriteFile(filepath.Join(root, "work", ".git"), []byte("gitdir: "+realGit+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info := discover(t, filepath.Join(root, "work"))
	if !info.Git.Present {
		t.Fatal("expected git to be present via .git file")
	}
	if info.Git.Branch != "feature/x" {
		t.Errorf("Branch = %q, want feature/x", info.Git.Branch)
	}
	if info.Git.Head != "cafebabecafebabecafebabecafebabecafebabe" {
		t.Errorf("Head = %q", info.Git.Head)
	}
	if !info.Git.HasRemote() {
		t.Error("expected upstream remote")
	}
}

func TestDiscoverContextCancelled(t *testing.T) {
	root := writeTree(t, map[string]string{"go.mod": goModFixture, "a.go": "package a\n"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Discover(ctx, root, Options{DisableGitBinary: true}); err == nil {
		t.Fatal("expected cancellation error")
	}
}

func TestParseMakeTargets(t *testing.T) {
	got := parseMakeTargets(makefileFixture)
	for _, want := range []string{"build", "test", "lint", "fmt"} {
		if !contains(got, want) {
			t.Errorf("targets %v missing %q", got, want)
		}
	}
	if contains(got, "VERSION") {
		t.Errorf("variable assignment parsed as target: %v", got)
	}
}

func TestParseGoMod(t *testing.T) {
	module, requires := parseGoMod(goModFixture)
	if module != "github.com/example/widget" {
		t.Errorf("module = %q", module)
	}
	if !contains(requires, "github.com/spf13/cobra") {
		t.Errorf("requires = %v", requires)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func containsCommand(cmds []Command, line string) bool {
	for _, c := range cmds {
		if c.Line == line {
			return true
		}
	}
	return false
}

func containsSubstring(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("expected to find %q in:\n%s", needle, haystack)
	}
}
