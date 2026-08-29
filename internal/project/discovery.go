// Package project turns a directory on disk into something Boop can reason
// about: where the project root is, what it is written in, how it is built and
// tested, what Git thinks its state is, and which of its files can reach
// production.
//
// Everything here is read-only. Discovery never writes to the working tree,
// never follows symlinks out of the project, and never reads the contents of
// files it flags as secrets (§48) — it classifies them by name and location so
// that the permission engine, not this package, decides who may open them.
package project

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// RootMarkers are the files and directories that mark a project root, in
// descending order of authority. A repository root beats a package manifest:
// in a monorepo the Git root is the unit Boop reasons about.
var RootMarkers = []string{
	".git",
	"go.mod",
	"package.json",
	"pyproject.toml",
	"Cargo.toml",
	"composer.json",
}

// Options tunes discovery. The zero value is the documented default.
type Options struct {
	// MaxDepth bounds directory recursion below the root (default 8).
	MaxDepth int
	// MaxFiles bounds how many files are recorded (default 20000). Scanning
	// stops recording beyond it and sets Info.Truncated.
	MaxFiles int
	// DisableGitBinary forbids shelling out to git. Branch, remotes and HEAD
	// are always read straight from .git; only the dirty check needs the
	// binary, and it degrades to "unknown" rather than lying.
	DisableGitBinary bool
	// GitTimeout bounds the dirty check (default 5s).
	GitTimeout time.Duration
}

func (o Options) withDefaults() Options {
	if o.MaxDepth <= 0 {
		o.MaxDepth = 8
	}
	if o.MaxFiles <= 0 {
		o.MaxFiles = 20000
	}
	if o.GitTimeout <= 0 {
		o.GitTimeout = 5 * time.Second
	}
	return o
}

// Language is a programming language detected in the tree.
type Language struct {
	Name  string
	Files int
}

// CommandKind classifies a project command.
type CommandKind string

// The command kinds §17 requires Boop to identify.
const (
	KindBuild  CommandKind = "build"
	KindTest   CommandKind = "test"
	KindLint   CommandKind = "lint"
	KindFormat CommandKind = "format"
)

// Command is a way to build, test, lint or format the project.
type Command struct {
	Kind CommandKind
	// Line is the shell command to run from the project root.
	Line string
	// Source is the file that defines it, relative to the root; empty when
	// the command is inferred from the ecosystem rather than declared.
	Source string
	// Inferred is true for ecosystem defaults. Declared commands rank first:
	// what the project says about itself beats what Boop guesses.
	Inferred bool
}

// Remote is a configured Git remote.
type Remote struct {
	Name string
	URL  string
}

// GitInfo is the repository state, read from .git where possible.
type GitInfo struct {
	// Present is true when the root is (or is inside) a Git repository.
	Present bool
	// Dir is the resolved .git directory.
	Dir string
	// Branch is the checked-out branch, empty when HEAD is detached.
	Branch string
	// Detached reports a detached HEAD.
	Detached bool
	// Head is the commit HEAD resolves to, when it could be read.
	Head string
	// Remotes are the configured remotes, ordered by name.
	Remotes []Remote
	// Dirty reports uncommitted changes; only meaningful when DirtyKnown.
	Dirty bool
	// DirtyKnown is false when the working-tree state could not be
	// determined (no git binary, or the check failed).
	DirtyKnown bool
	// DirtyFiles counts changed paths when DirtyKnown.
	DirtyFiles int
}

// HasRemote reports whether any remote is configured.
func (g GitInfo) HasRemote() bool { return len(g.Remotes) > 0 }

// Sensitivity grades how much damage a file can do. It mirrors the risk
// vocabulary of the permission engine without importing it, so discovery stays
// usable on its own.
type Sensitivity string

// Sensitivity levels, ascending.
const (
	SensitivityLow      Sensitivity = "low"
	SensitivityMedium   Sensitivity = "medium"
	SensitivityHigh     Sensitivity = "high"
	SensitivityCritical Sensitivity = "critical"
)

func sensitivityRank(s Sensitivity) int {
	switch s {
	case SensitivityCritical:
		return 3
	case SensitivityHigh:
		return 2
	case SensitivityMedium:
		return 1
	default:
		return 0
	}
}

// Categories of production-sensitive file.
const (
	CategorySecret         = "secret"
	CategoryEnvironment    = "environment"
	CategoryDeployment     = "deployment"
	CategoryKubernetes     = "kubernetes"
	CategoryInfrastructure = "infrastructure"
	CategoryProvisioning   = "provisioning"
	CategoryCI             = "ci"
	CategoryWebServer      = "webserver"
	CategoryService        = "service"
)

// SensitiveFile is a file that can affect production, or that holds
// credentials. Boop surfaces these so production work stays deliberate
// (§2.9, §15) and so secrets are never read casually (§48).
type SensitiveFile struct {
	// Path is relative to the project root, slash-separated.
	Path        string
	Category    string
	Reason      string
	Sensitivity Sensitivity
}

// Info describes a project.
type Info struct {
	// Root is the absolute project root.
	Root string
	// Name is the project's own name where a manifest declares one.
	Name string
	// Markers are the root markers found at Root.
	Markers []string
	// Languages are detected languages, most files first.
	Languages []Language
	// Frameworks are libraries and frameworks worth knowing about.
	Frameworks []string
	// Commands are the project's build/test/lint/format commands, declared
	// ones before inferred ones.
	Commands []Command
	// Git is the repository state.
	Git GitInfo
	// Sensitive lists production-sensitive and secret-bearing files, most
	// dangerous first.
	Sensitive []SensitiveFile
	// Docs are existing documentation files.
	Docs []string
	// ImportantFiles are manifests, entry points and top-level configuration.
	ImportantFiles []string
	// TopLevelDirs are the root's directories with their file counts.
	TopLevelDirs []DirSummary
	// TestFiles counts files that look like tests.
	TestFiles int
	// FileCount is the number of files scanned.
	FileCount int
	// Truncated reports that MaxFiles or MaxDepth stopped the scan, so counts
	// are lower bounds.
	Truncated bool
}

// DirSummary is a top-level directory and how many files it contains.
type DirSummary struct {
	Name  string
	Files int
}

// PrimaryLanguage returns the language with the most files, or "".
func (i *Info) PrimaryLanguage() string {
	if len(i.Languages) == 0 {
		return ""
	}
	return i.Languages[0].Name
}

// CommandsFor returns the commands of a kind, best first.
func (i *Info) CommandsFor(kind CommandKind) []Command {
	var out []Command
	for _, c := range i.Commands {
		if c.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

// Command returns the best command of a kind.
func (i *Info) Command(kind CommandKind) (Command, bool) {
	for _, c := range i.Commands {
		if c.Kind == kind {
			return c, true
		}
	}
	return Command{}, false
}

// HasTests reports whether anything test-shaped was found.
func (i *Info) HasTests() bool { return i.TestFiles > 0 }

// FindRoot walks up from dir looking for a project root marker.
//
// A Git root wins outright; otherwise the nearest directory holding a package
// manifest wins. When nothing is found, dir itself is the root: Boop must work
// in a bare directory.
func FindRoot(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", dir, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("project root %q: %w", dir, err)
	}
	if !info.IsDir() {
		abs = filepath.Dir(abs)
	}

	manifest := ""
	for cur := abs; ; {
		if _, err := os.Lstat(filepath.Join(cur, ".git")); err == nil {
			return cur, nil
		}
		if manifest == "" {
			for _, m := range RootMarkers[1:] {
				if _, err := os.Stat(filepath.Join(cur, m)); err == nil {
					manifest = cur
					break
				}
			}
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	if manifest != "" {
		return manifest, nil
	}
	return abs, nil
}

// Discover inspects dir and describes the project it belongs to. dir may be
// any directory inside the project; the root is located with FindRoot.
func Discover(ctx context.Context, dir string, opts Options) (*Info, error) {
	opts = opts.withDefaults()
	root, err := FindRoot(dir)
	if err != nil {
		return nil, err
	}
	scan, err := scanTree(ctx, root, opts)
	if err != nil {
		return nil, err
	}

	info := &Info{
		Root:         root,
		Name:         filepath.Base(root),
		TopLevelDirs: scan.topLevelDirs(),
		TestFiles:    scan.testFiles,
		FileCount:    scan.total,
		Truncated:    scan.truncated,
	}
	for _, m := range RootMarkers {
		if scan.has(m) {
			info.Markers = append(info.Markers, m)
		}
	}
	info.Languages = scan.languages()

	man := readManifests(root, scan)
	if man.name != "" {
		info.Name = man.name
	}
	info.Frameworks = man.frameworks(scan)
	info.Commands = detectCommands(root, scan, man)
	info.Docs = scan.docs()
	info.ImportantFiles = scan.importantFiles(man)
	info.Sensitive = detectSensitive(root, scan)
	info.Git = readGit(ctx, root, opts)
	return info, nil
}

// ---------------------------------------------------------------------------
// tree scan

// skipDirs are directories that hold dependencies or build output. Scanning
// them would dwarf the project's own code and slow every /prep down.
var skipDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true,
	"node_modules": true, "bower_components": true, "vendor": true,
	".venv": true, "venv": true, "__pycache__": true, ".tox": true,
	"target": true, "dist": true, "build": true, "out": true,
	".next": true, ".nuxt": true, ".svelte-kit": true, ".parcel-cache": true,
	".cache": true, ".gradle": true, ".idea": true, ".vscode": true,
	"coverage": true, ".pytest_cache": true, ".mypy_cache": true,
	".terraform": true, "Pods": true, ".dart_tool": true, ".bundle": true,
}

type treeScan struct {
	root      string
	files     []string // relative, slash-separated
	set       map[string]bool
	dirCount  map[string]int // top-level directory -> files beneath it
	dirs      map[string]bool
	extCount  map[string]int
	testFiles int
	total     int
	truncated bool
}

func scanTree(ctx context.Context, root string, opts Options) (*treeScan, error) {
	s := &treeScan{
		root:     root,
		set:      map[string]bool{},
		dirCount: map[string]int{},
		dirs:     map[string]bool{},
		extCount: map[string]int{},
	}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree is not a reason to fail discovery.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				s.dirs[rel] = true
				return fs.SkipDir
			}
			if strings.Count(rel, "/")+1 > opts.MaxDepth {
				s.truncated = true
				return fs.SkipDir
			}
			s.dirs[rel] = true
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil
		}
		s.total++
		if len(s.files) >= opts.MaxFiles {
			s.truncated = true
			return nil
		}
		s.files = append(s.files, rel)
		s.set[rel] = true
		if i := strings.IndexByte(rel, '/'); i > 0 {
			s.dirCount[rel[:i]]++
		}
		s.extCount[strings.ToLower(filepath.Ext(rel))]++
		if isTestFile(rel) {
			s.testFiles++
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.SkipAll) {
		return nil, fmt.Errorf("scan %q: %w", root, err)
	}
	sort.Strings(s.files)
	return s, nil
}

func (s *treeScan) has(rel string) bool { return s.set[rel] }

func (s *treeScan) hasDir(rel string) bool { return s.dirs[rel] }

func (s *treeScan) read(rel string, limit int64) []byte {
	f, err := os.Open(filepath.Join(s.root, filepath.FromSlash(rel)))
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, limit)
	n, _ := f.Read(buf)
	if n <= 0 {
		return nil
	}
	return buf[:n]
}

func (s *treeScan) topLevelDirs() []DirSummary {
	out := make([]DirSummary, 0, len(s.dirCount))
	for name, n := range s.dirCount {
		out = append(out, DirSummary{Name: name, Files: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Files != out[j].Files {
			return out[i].Files > out[j].Files
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// extLanguages maps file extensions to language names. Data and markup formats
// are deliberately absent: a repository of YAML is not "a YAML project".
var extLanguages = map[string]string{
	".go": "Go", ".rs": "Rust", ".py": "Python", ".rb": "Ruby", ".php": "PHP",
	".js": "JavaScript", ".mjs": "JavaScript", ".cjs": "JavaScript", ".jsx": "JavaScript",
	".ts": "TypeScript", ".tsx": "TypeScript",
	".java": "Java", ".kt": "Kotlin", ".kts": "Kotlin", ".scala": "Scala",
	".swift": "Swift", ".m": "Objective-C", ".cs": "C#",
	".c": "C", ".h": "C", ".cc": "C++", ".cpp": "C++", ".cxx": "C++", ".hpp": "C++", ".hh": "C++",
	".sh": "Shell", ".bash": "Shell", ".zsh": "Shell", ".ps1": "PowerShell",
	".sql": "SQL", ".html": "HTML", ".htm": "HTML", ".css": "CSS", ".scss": "SCSS", ".sass": "SCSS",
	".vue": "Vue", ".svelte": "Svelte", ".ex": "Elixir", ".exs": "Elixir", ".erl": "Erlang",
	".hs": "Haskell", ".lua": "Lua", ".dart": "Dart", ".clj": "Clojure", ".zig": "Zig",
	".pl": "Perl", ".r": "R",
}

func (s *treeScan) languages() []Language {
	counts := map[string]int{}
	for ext, n := range s.extCount {
		if name, ok := extLanguages[ext]; ok {
			counts[name] += n
		}
	}
	out := make([]Language, 0, len(counts))
	for name, n := range counts {
		out = append(out, Language{Name: name, Files: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Files != out[j].Files {
			return out[i].Files > out[j].Files
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func isTestFile(rel string) bool {
	base := path.Base(rel)
	lower := strings.ToLower(rel)
	switch {
	case strings.HasSuffix(base, "_test.go"),
		strings.HasSuffix(base, "_test.py"), strings.HasPrefix(base, "test_"),
		strings.HasSuffix(base, "_test.rb"), strings.HasSuffix(base, "_spec.rb"),
		strings.HasSuffix(base, "Test.java"), strings.HasSuffix(base, "Test.php"),
		strings.HasSuffix(base, "Test.cs"):
		return true
	}
	for _, suffix := range []string{".test.js", ".test.ts", ".test.tsx", ".test.jsx", ".spec.js", ".spec.ts", ".spec.tsx"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return strings.HasPrefix(lower, "tests/") || strings.HasPrefix(lower, "test/") || strings.Contains(lower, "/tests/")
}

var docNames = []string{
	"readme", "readme.md", "readme.rst", "readme.txt",
	"contributing.md", "architecture.md", "changelog.md", "license",
	"agents.md", "claude.md", "project.md", "docs/architecture.md",
}

func (s *treeScan) docs() []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, f := range s.files {
		lower := strings.ToLower(f)
		if !strings.Contains(f, "/") {
			for _, d := range docNames {
				if lower == d || strings.HasPrefix(lower, strings.TrimSuffix(d, ".md")+".") {
					add(f)
				}
			}
			continue
		}
		if strings.HasPrefix(lower, "docs/") && (strings.HasSuffix(lower, ".md") || strings.HasSuffix(lower, ".rst")) {
			add(f)
		}
	}
	sort.Strings(out)
	if len(out) > 25 {
		out = out[:25]
	}
	return out
}

// entryPoints are conventional program entry points, checked at the root and
// one level down.
var entryPoints = []string{
	"main.go", "src/main.rs", "main.py", "app.py", "manage.py", "wsgi.py",
	"src/index.ts", "src/index.js", "src/main.ts", "src/main.js", "index.js", "index.ts",
	"index.php", "public/index.php", "artisan", "src/App.tsx", "cmd",
}

// rootConfigFiles are top-level files worth remembering in Boop.md.
var rootConfigFiles = []string{
	"Makefile", "makefile", "GNUmakefile", "Justfile", "justfile", "Taskfile.yml",
	"go.mod", "go.work", "package.json", "tsconfig.json", "pyproject.toml",
	"setup.py", "setup.cfg", "requirements.txt", "Pipfile", "poetry.lock",
	"Cargo.toml", "composer.json", "Gemfile", "pom.xml", "build.gradle",
	"build.gradle.kts", "Dockerfile", "docker-compose.yml", "docker-compose.yaml",
	"compose.yaml", ".golangci.yml", ".golangci.yaml", ".editorconfig",
	"Boop.md", "PROJECT.md",
}

func (s *treeScan) importantFiles(man manifests) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] && s.has(p) {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, f := range rootConfigFiles {
		add(f)
	}
	for _, e := range entryPoints {
		if e == "cmd" {
			for _, f := range s.files {
				if strings.HasPrefix(f, "cmd/") && strings.HasSuffix(f, "/main.go") {
					add(f)
				}
			}
			continue
		}
		add(e)
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

// ---------------------------------------------------------------------------
// manifests

type manifests struct {
	name string

	goModule   string
	goRequires []string

	pkg        packageJSON
	hasPkg     bool
	pkgManager string

	cargo    string // raw Cargo.toml
	pyproj   string // raw pyproject.toml
	composer composerJSON
	hasComp  bool

	makeTargets []string
	reqs        string // requirements.txt and friends, concatenated
}

type packageJSON struct {
	Name            string            `json:"name"`
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

type composerJSON struct {
	Name       string                     `json:"name"`
	Scripts    map[string]json.RawMessage `json:"scripts"`
	Require    map[string]string          `json:"require"`
	RequireDev map[string]string          `json:"require-dev"`
}

func readManifests(root string, s *treeScan) manifests {
	m := manifests{}
	if b := s.read("go.mod", 256<<10); b != nil {
		m.goModule, m.goRequires = parseGoMod(string(b))
		if m.goModule != "" {
			m.name = path.Base(m.goModule)
		}
	}
	if b := s.read("package.json", 1<<20); b != nil {
		if err := json.Unmarshal(b, &m.pkg); err == nil {
			m.hasPkg = true
			if m.pkg.Name != "" {
				m.name = m.pkg.Name
			}
		}
	}
	m.pkgManager = "npm"
	switch {
	case s.has("pnpm-lock.yaml"):
		m.pkgManager = "pnpm"
	case s.has("yarn.lock"):
		m.pkgManager = "yarn"
	case s.has("bun.lockb"), s.has("bun.lock"):
		m.pkgManager = "bun"
	}
	if b := s.read("Cargo.toml", 256<<10); b != nil {
		m.cargo = string(b)
		if n := tomlValue(m.cargo, "package", "name"); n != "" {
			m.name = n
		}
	}
	if b := s.read("pyproject.toml", 256<<10); b != nil {
		m.pyproj = string(b)
		if n := tomlValue(m.pyproj, "project", "name"); n != "" {
			m.name = n
		} else if n := tomlValue(m.pyproj, "tool.poetry", "name"); n != "" {
			m.name = n
		}
	}
	if b := s.read("composer.json", 1<<20); b != nil {
		if err := json.Unmarshal(b, &m.composer); err == nil {
			m.hasComp = true
			if m.composer.Name != "" {
				m.name = path.Base(m.composer.Name)
			}
		}
	}
	for _, f := range []string{"Makefile", "makefile", "GNUmakefile"} {
		if b := s.read(f, 512<<10); b != nil {
			m.makeTargets = parseMakeTargets(string(b))
			break
		}
	}
	for _, f := range []string{"requirements.txt", "requirements-dev.txt", "Pipfile"} {
		if b := s.read(f, 128<<10); b != nil {
			m.reqs += string(b) + "\n"
		}
	}
	_ = root
	return m
}

var goRequireRe = regexp.MustCompile(`(?m)^\s*(?:require\s+)?([a-zA-Z0-9._~-]+(?:\.[a-zA-Z]{2,})[^\s]*)\s+v[0-9]`)

func parseGoMod(src string) (module string, requires []string) {
	sc := bufio.NewScanner(strings.NewReader(src))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "module ") {
			module = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "module ")), `"`)
		}
	}
	for _, mt := range goRequireRe.FindAllStringSubmatch(src, -1) {
		requires = append(requires, mt[1])
	}
	return module, requires
}

var makeTargetRe = regexp.MustCompile(`(?m)^([A-Za-z0-9][A-Za-z0-9_./-]*)\s*:(?:[^=]|$)`)

func parseMakeTargets(src string) []string {
	var out []string
	seen := map[string]bool{}
	for _, mt := range makeTargetRe.FindAllStringSubmatch(src, -1) {
		t := mt[1]
		if seen[t] || strings.HasPrefix(t, ".") {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// tomlValue extracts a simple `key = "value"` from a [section] of a TOML file.
// Boop pins no TOML dependency (§40), and manifest lookups of this shape do
// not justify one; anything more structural is done by substring matching.
func tomlValue(src, section, key string) string {
	cur := ""
	sc := bufio.NewScanner(strings.NewReader(src))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			cur = strings.Trim(line, "[]")
			continue
		}
		if cur != section {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		return strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return ""
}

// tomlSection returns the raw body of a [section].
func tomlSection(src, section string) string {
	var b strings.Builder
	cur := ""
	sc := bufio.NewScanner(strings.NewReader(src))
	for sc.Scan() {
		line := sc.Text()
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			cur = strings.Trim(t, "[]")
			continue
		}
		if cur == section {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// frameworks

type frameworkRule struct {
	match string // substring of a dependency name
	name  string
}

var jsFrameworks = []frameworkRule{
	{"next", "Next.js"}, {"nuxt", "Nuxt"}, {"@sveltejs/kit", "SvelteKit"},
	{"react", "React"}, {"vue", "Vue"}, {"svelte", "Svelte"}, {"@angular/core", "Angular"},
	{"express", "Express"}, {"fastify", "Fastify"}, {"@nestjs/core", "NestJS"},
	{"electron", "Electron"}, {"vite", "Vite"}, {"webpack", "Webpack"},
	{"tailwindcss", "Tailwind CSS"}, {"jest", "Jest"}, {"vitest", "Vitest"},
	{"@playwright/test", "Playwright"}, {"eslint", "ESLint"}, {"prettier", "Prettier"},
	{"typescript", "TypeScript"},
}

var goFrameworks = []frameworkRule{
	{"gin-gonic/gin", "Gin"}, {"labstack/echo", "Echo"}, {"gofiber/fiber", "Fiber"},
	{"go-chi/chi", "chi"}, {"gorilla/mux", "gorilla/mux"}, {"spf13/cobra", "Cobra"},
	{"charmbracelet/bubbletea", "Bubble Tea"}, {"gorm.io/gorm", "GORM"},
	{"google.golang.org/grpc", "gRPC"}, {"stretchr/testify", "testify"},
	{"modernc.org/sqlite", "SQLite (modernc)"},
}

var pyFrameworks = []frameworkRule{
	{"django", "Django"}, {"flask", "Flask"}, {"fastapi", "FastAPI"},
	{"sqlalchemy", "SQLAlchemy"}, {"celery", "Celery"}, {"pytest", "pytest"},
	{"ruff", "Ruff"}, {"black", "Black"}, {"mypy", "mypy"},
}

var rustFrameworks = []frameworkRule{
	{"axum", "Axum"}, {"actix-web", "Actix Web"}, {"rocket", "Rocket"},
	{"tokio", "Tokio"}, {"clap", "clap"}, {"serde", "Serde"}, {"bevy", "Bevy"},
}

var phpFrameworks = []frameworkRule{
	{"laravel/framework", "Laravel"}, {"symfony/", "Symfony"}, {"slim/slim", "Slim"},
	{"phpunit/phpunit", "PHPUnit"},
}

func (m manifests) frameworks(s *treeScan) []string {
	set := map[string]bool{}
	addMatches := func(rules []frameworkRule, haystack ...string) {
		for _, r := range rules {
			for _, h := range haystack {
				if h != "" && strings.Contains(strings.ToLower(h), r.match) {
					set[r.name] = true
					break
				}
			}
		}
	}
	if m.hasPkg {
		deps := make([]string, 0, len(m.pkg.Dependencies)+len(m.pkg.DevDependencies))
		for k := range m.pkg.Dependencies {
			deps = append(deps, k)
		}
		for k := range m.pkg.DevDependencies {
			deps = append(deps, k)
		}
		for _, r := range jsFrameworks {
			for _, d := range deps {
				if strings.EqualFold(d, r.match) || strings.HasPrefix(strings.ToLower(d), r.match+"-") || strings.HasPrefix(strings.ToLower(d), r.match+"/") {
					set[r.name] = true
				}
			}
		}
	}
	addMatches(goFrameworks, strings.Join(m.goRequires, "\n"))
	addMatches(pyFrameworks, m.pyproj, m.reqs)
	if m.cargo != "" {
		addMatches(rustFrameworks, tomlSection(m.cargo, "dependencies"), tomlSection(m.cargo, "dev-dependencies"))
	}
	if m.hasComp {
		var deps strings.Builder
		for k := range m.composer.Require {
			deps.WriteString(k + "\n")
		}
		for k := range m.composer.RequireDev {
			deps.WriteString(k + "\n")
		}
		addMatches(phpFrameworks, deps.String())
	}
	if s.has("Dockerfile") || s.has("dockerfile") {
		set["Docker"] = true
	}
	if s.has("docker-compose.yml") || s.has("docker-compose.yaml") || s.has("compose.yaml") || s.has("compose.yml") {
		set["Docker Compose"] = true
	}
	if s.hasDir(".github/workflows") {
		set["GitHub Actions"] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// commands

// makeTargetKinds maps conventional Make target names to command kinds.
var makeTargetKinds = map[string]CommandKind{
	"build": KindBuild, "compile": KindBuild, "all": KindBuild,
	"test": KindTest, "tests": KindTest, "check": KindTest, "test-unit": KindTest,
	"lint": KindLint, "vet": KindLint, "staticcheck": KindLint,
	"fmt": KindFormat, "format": KindFormat, "gofmt": KindFormat,
}

// scriptKinds maps conventional package.json / composer script names.
var scriptKinds = map[string]CommandKind{
	"build": KindBuild, "compile": KindBuild, "bundle": KindBuild,
	"test": KindTest, "test:unit": KindTest, "typecheck": KindLint,
	"lint": KindLint, "lint:fix": KindLint, "eslint": KindLint,
	"format": KindFormat, "fmt": KindFormat, "prettier": KindFormat,
	"cs-fix": KindFormat, "phpstan": KindLint, "analyse": KindLint,
}

func detectCommands(root string, s *treeScan, m manifests) []Command {
	var out []Command
	add := func(kind CommandKind, line, source string, inferred bool) {
		for _, c := range out {
			if c.Kind == kind && c.Line == line {
				return
			}
		}
		out = append(out, Command{Kind: kind, Line: line, Source: source, Inferred: inferred})
	}

	// 1. The project's own Makefile is the most authoritative source.
	makefile := ""
	for _, f := range []string{"Makefile", "makefile", "GNUmakefile"} {
		if s.has(f) {
			makefile = f
			break
		}
	}
	for _, t := range m.makeTargets {
		if kind, ok := makeTargetKinds[t]; ok {
			add(kind, "make "+t, makefile, false)
		}
	}

	// 2. Declared manifest scripts.
	if m.hasPkg {
		run := m.pkgManager + " run "
		names := make([]string, 0, len(m.pkg.Scripts))
		for k := range m.pkg.Scripts {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			if kind, ok := scriptKinds[strings.ToLower(k)]; ok {
				add(kind, run+k, "package.json", false)
			}
		}
	}
	if m.hasComp {
		names := make([]string, 0, len(m.composer.Scripts))
		for k := range m.composer.Scripts {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			if kind, ok := scriptKinds[strings.ToLower(k)]; ok {
				add(kind, "composer run "+k, "composer.json", false)
			}
		}
	}

	// 3. Tool configuration that names an exact tool.
	for _, f := range []string{".golangci.yml", ".golangci.yaml", ".golangci.toml", ".golangci.json"} {
		if s.has(f) {
			add(KindLint, "golangci-lint run", f, false)
			break
		}
	}
	if s.has("phpunit.xml") || s.has("phpunit.xml.dist") {
		add(KindTest, "vendor/bin/phpunit", "phpunit.xml", false)
	}
	if s.has("phpstan.neon") || s.has("phpstan.neon.dist") {
		add(KindLint, "vendor/bin/phpstan analyse", "phpstan.neon", false)
	}
	if s.has("tox.ini") {
		add(KindTest, "tox", "tox.ini", false)
	}

	// 4. Ecosystem defaults, last: a guess only helps when nothing is declared.
	if s.has("go.mod") {
		add(KindBuild, "go build ./...", "go.mod", true)
		add(KindTest, "go test ./...", "go.mod", true)
		add(KindLint, "go vet ./...", "go.mod", true)
		add(KindFormat, "gofmt -w .", "go.mod", true)
	}
	if m.hasPkg {
		add(KindTest, m.pkgManager+" test", "package.json", true)
	}
	if s.has("Cargo.toml") {
		add(KindBuild, "cargo build", "Cargo.toml", true)
		add(KindTest, "cargo test", "Cargo.toml", true)
		add(KindLint, "cargo clippy", "Cargo.toml", true)
		add(KindFormat, "cargo fmt", "Cargo.toml", true)
	}
	if s.has("pyproject.toml") || s.has("setup.py") || s.has("pytest.ini") || s.has("tox.ini") || s.has("requirements.txt") {
		src := "pyproject.toml"
		if !s.has(src) {
			src = ""
		}
		add(KindTest, "pytest", src, true)
		if strings.Contains(m.pyproj, "[tool.ruff") {
			add(KindLint, "ruff check .", "pyproject.toml", false)
			add(KindFormat, "ruff format .", "pyproject.toml", false)
		}
		if strings.Contains(m.pyproj, "[tool.black") {
			add(KindFormat, "black .", "pyproject.toml", false)
		}
		if strings.Contains(m.pyproj, "[tool.mypy") {
			add(KindLint, "mypy .", "pyproject.toml", false)
		}
		if strings.Contains(m.pyproj, "[build-system") {
			add(KindBuild, "python -m build", "pyproject.toml", true)
		}
	}
	if s.has("pom.xml") {
		add(KindBuild, "mvn package", "pom.xml", true)
		add(KindTest, "mvn test", "pom.xml", true)
	}
	if s.has("build.gradle") || s.has("build.gradle.kts") {
		add(KindBuild, "gradle build", "build.gradle", true)
		add(KindTest, "gradle test", "build.gradle", true)
	}
	if s.has("Gemfile") {
		add(KindTest, "bundle exec rspec", "Gemfile", true)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Inferred != out[j].Inferred {
			return !out[i].Inferred
		}
		return false
	})
	_ = root
	return out
}

// ---------------------------------------------------------------------------
// production-sensitive files

var (
	envExampleRe = regexp.MustCompile(`(?i)\.(example|sample|template|dist)$`)
	deployNameRe = regexp.MustCompile(`(?i)^(deploy|release|provision|rollout|publish)[a-z0-9_.-]*\.(sh|bash|zsh|py|rb|ps1)$`)
	k8sKindRe    = regexp.MustCompile(`(?m)^kind:\s*(Deployment|StatefulSet|DaemonSet|Service|Ingress|CronJob|Job|Namespace|Secret|ConfigMap|HelmRelease|PersistentVolumeClaim)`)
	ciDeployRe   = regexp.MustCompile(`(?i)(deploy|kubectl|helm |terraform |aws |gcloud |az |ssh |rsync|docker push|npm publish|goreleaser|environment:)`)
)

func detectSensitive(root string, s *treeScan) []SensitiveFile {
	var out []SensitiveFile
	add := func(p, cat, reason string, sev Sensitivity) {
		out = append(out, SensitiveFile{Path: p, Category: cat, Reason: reason, Sensitivity: sev})
	}
	yamlSniffed := 0
	for _, f := range s.files {
		base := path.Base(f)
		lower := strings.ToLower(base)
		lowerPath := strings.ToLower(f)
		dir := path.Dir(f)
		ext := strings.ToLower(path.Ext(base))

		switch {
		// Credentials and keys (§48): flagged by name, never opened.
		case lower == ".env" || (strings.HasPrefix(lower, ".env") && !envExampleRe.MatchString(lower)):
			add(f, CategoryEnvironment, "environment file may hold production credentials", SensitivityCritical)
		case ext == ".pem", ext == ".key", ext == ".p12", ext == ".pfx", ext == ".jks", ext == ".keystore",
			strings.HasPrefix(lower, "id_rsa"), strings.HasPrefix(lower, "id_ed25519"),
			strings.HasPrefix(lower, "credentials"), strings.HasPrefix(lower, "secrets"),
			strings.HasPrefix(lower, "secret."), lower == "kubeconfig", ext == ".kubeconfig",
			strings.HasPrefix(lower, "service-account") && ext == ".json":
			add(f, CategorySecret, "may contain private keys or credentials", SensitivityCritical)

		// Infrastructure as code.
		case ext == ".tfstate", strings.HasSuffix(lower, ".tfstate.backup"):
			add(f, CategoryInfrastructure, "Terraform state describes live infrastructure and may embed secrets", SensitivityCritical)
		case ext == ".tf", ext == ".tfvars", lower == "terragrunt.hcl":
			add(f, CategoryInfrastructure, "Terraform definition changes real infrastructure", SensitivityHigh)

		// Deployment topology.
		case isProdCompose(lower):
			add(f, CategoryDeployment, "production container topology", SensitivityHigh)
		case deployNameRe.MatchString(lower),
			(dir == "deploy" || dir == "deployment" || strings.HasPrefix(lowerPath, "scripts/deploy")) && (ext == ".sh" || ext == ".py" || ext == ".ps1"):
			add(f, CategoryDeployment, "deployment script", SensitivityHigh)
		case dir == "." && isHostingConfig(lower):
			add(f, CategoryDeployment, "hosting platform deployment configuration", SensitivityHigh)

		// Service and web server configuration.
		case ext == ".service", ext == ".timer", ext == ".socket", ext == ".mount":
			add(f, CategoryService, "systemd unit controls a running service", SensitivityMedium)
		case lower == "nginx.conf", strings.HasSuffix(lower, ".nginx"), strings.HasSuffix(lower, ".vhost"),
			lower == "apache2.conf", lower == "httpd.conf", lower == ".htaccess",
			dir == "sites-available" || dir == "sites-enabled" || strings.HasSuffix(dir, "/sites-available") || strings.HasSuffix(dir, "/sites-enabled"):
			add(f, CategoryWebServer, "web server configuration affects live traffic", SensitivityMedium)

		// Configuration management.
		case lower == "ansible.cfg",
			dir == "." && (lower == "site.yml" || lower == "site.yaml" || strings.HasPrefix(lower, "inventory") || lower == "hosts.ini"),
			strings.HasPrefix(lower, "playbook"),
			strings.HasPrefix(lowerPath, "ansible/"),
			strings.HasPrefix(lowerPath, "roles/") && (ext == ".yml" || ext == ".yaml"):
			add(f, CategoryProvisioning, "Ansible configuration management targets real hosts", SensitivityHigh)

		// CI that can deploy.
		case strings.HasPrefix(lowerPath, ".github/workflows/") && (ext == ".yml" || ext == ".yaml"),
			lower == ".gitlab-ci.yml", lower == "jenkinsfile", strings.HasPrefix(lowerPath, ".circleci/"),
			lower == "azure-pipelines.yml", lower == ".drone.yml":
			if b := s.read(f, 64<<10); b != nil && ciDeployRe.Match(b) {
				add(f, CategoryCI, "CI pipeline performs deployments or publishes artifacts", SensitivityHigh)
			}

		// Kubernetes manifests, identified by content rather than location.
		case ext == ".yaml" || ext == ".yml":
			if yamlSniffed >= 300 {
				break
			}
			yamlSniffed++
			b := s.read(f, 8<<10)
			if b != nil && strings.Contains(string(b), "apiVersion:") && k8sKindRe.Match(b) {
				add(f, CategoryKubernetes, "Kubernetes manifest describes cluster workloads", SensitivityHigh)
			}
		}
	}
	if s.hasDir(".terraform") {
		add(".terraform", CategoryInfrastructure, "initialised Terraform working directory", SensitivityMedium)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := sensitivityRank(out[i].Sensitivity), sensitivityRank(out[j].Sensitivity)
		if ri != rj {
			return ri > rj
		}
		return out[i].Path < out[j].Path
	})
	_ = root
	return out
}

// hostingConfigs are root-level files that describe how a hosting platform
// deploys the project.
var hostingConfigs = map[string]bool{
	"procfile": true, "fly.toml": true, "serverless.yml": true, "serverless.yaml": true,
	"vercel.json": true, "netlify.toml": true, "app.yaml": true, "captain-definition": true,
	"dockerrun.aws.json": true, "render.yaml": true,
}

func isHostingConfig(lower string) bool { return hostingConfigs[lower] }

func isProdCompose(lower string) bool {
	if !strings.HasPrefix(lower, "docker-compose") && !strings.HasPrefix(lower, "compose") {
		return false
	}
	if !strings.HasSuffix(lower, ".yml") && !strings.HasSuffix(lower, ".yaml") {
		return false
	}
	return strings.Contains(lower, "prod") || strings.Contains(lower, "staging") || strings.Contains(lower, "live")
}

// ---------------------------------------------------------------------------
// git

// readGit reads repository state. Branch, HEAD and remotes come straight from
// the .git directory so Boop works where the git binary is absent; only the
// dirty check needs git, and its absence is reported rather than guessed.
func readGit(ctx context.Context, root string, opts Options) GitInfo {
	g := GitInfo{}
	gitPath := filepath.Join(root, ".git")
	st, err := os.Stat(gitPath)
	if err != nil {
		return g
	}
	dir := gitPath
	if !st.IsDir() {
		// Worktrees and submodules use a ".git" file pointing elsewhere.
		b, err := os.ReadFile(gitPath)
		if err != nil {
			return g
		}
		line := strings.TrimSpace(string(b))
		target := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
		if target == "" || target == line {
			return g
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(root, target)
		}
		dir = filepath.Clean(target)
	}
	g.Present = true
	g.Dir = dir

	if b, err := os.ReadFile(filepath.Join(dir, "HEAD")); err == nil {
		head := strings.TrimSpace(string(b))
		if ref, ok := strings.CutPrefix(head, "ref:"); ok {
			ref = strings.TrimSpace(ref)
			g.Branch = strings.TrimPrefix(ref, "refs/heads/")
			g.Head = resolveRef(dir, ref)
		} else {
			g.Detached = true
			g.Head = head
		}
	}
	g.Remotes = parseGitRemotes(filepath.Join(dir, "config"))
	if !opts.DisableGitBinary {
		g.Dirty, g.DirtyFiles, g.DirtyKnown = gitDirty(ctx, root, opts.GitTimeout)
	}
	return g
}

// resolveRef resolves a ref to a commit via loose refs then packed-refs.
func resolveRef(gitDir, ref string) string {
	if b, err := os.ReadFile(filepath.Join(gitDir, filepath.FromSlash(ref))); err == nil {
		return strings.TrimSpace(string(b))
	}
	b, err := os.ReadFile(filepath.Join(gitDir, "packed-refs"))
	if err != nil {
		return ""
	}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		sha, name, ok := strings.Cut(line, " ")
		if ok && name == ref {
			return sha
		}
	}
	return ""
}

// parseGitRemotes reads remote URLs from .git/config without running git.
func parseGitRemotes(configPath string) []Remote {
	b, err := os.ReadFile(configPath)
	if err != nil {
		return nil
	}
	var out []Remote
	cur := ""
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			cur = ""
			inner := strings.Trim(line, "[]")
			if name, ok := strings.CutPrefix(inner, "remote "); ok {
				cur = strings.Trim(strings.TrimSpace(name), `"`)
			}
			continue
		}
		if cur == "" {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == "url" {
			out = append(out, Remote{Name: cur, URL: strings.TrimSpace(v)})
			cur = ""
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// gitDirty asks git whether the working tree is clean. It is the one place
// discovery needs the binary; failure is reported as "unknown", never as clean.
func gitDirty(ctx context.Context, root string, timeout time.Duration) (dirty bool, files int, known bool) {
	bin, err := exec.LookPath("git")
	if err != nil {
		return false, 0, false
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "status", "--porcelain")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return false, 0, false
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n > 0, n, true
}
