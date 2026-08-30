package web

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kawaiipantsu/boop/internal/project"
)

// Bounds on project inspection.
//
// Discovery and Prep both walk the whole tree (§17). A monorepo on a cold page
// cache can make that take a while, and an HTTP handler that waits for it
// without a deadline is a hang with extra steps — so both get one, and the
// result is cached for long enough that a polling Project tab does not rescan
// the repository once a second.
const (
	projectScanTimeout = 20 * time.Second
	projectPrepTimeout = 90 * time.Second
	projectCacheTTL    = 30 * time.Second
)

// projectCacheEntry is a discovery result and when it was taken.
type projectCacheEntry struct {
	info *project.Info
	at   time.Time
}

// ---------------------------------------------------------------------------
// Response shapes
// ---------------------------------------------------------------------------

// languageView is one detected language.
type languageView struct {
	Name  string `json:"name"`
	Files int    `json:"files"`
}

// commandView is a build, test, lint or format command (§17).
type commandView struct {
	Kind string `json:"kind"`
	Line string `json:"line"`
	// Source is the file that declares it, empty for an ecosystem default.
	Source string `json:"source,omitempty"`
	// Inferred marks a command Boop guessed rather than read.
	Inferred bool `json:"inferred"`
}

// remoteView is a configured Git remote.
type remoteView struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// gitView is the repository state.
type gitView struct {
	Present  bool   `json:"present"`
	Branch   string `json:"branch,omitempty"`
	Detached bool   `json:"detached"`
	Head     string `json:"head,omitempty"`
	// Dirty is only meaningful when DirtyKnown; without a git binary the
	// working-tree state cannot be read and Boop says so rather than
	// reporting a clean tree it did not verify.
	Dirty      bool         `json:"dirty"`
	DirtyKnown bool         `json:"dirty_known"`
	DirtyFiles int          `json:"dirty_files"`
	Remotes    []remoteView `json:"remotes"`
}

// sensitiveView is a production-sensitive or secret-bearing file (§15, §48).
type sensitiveView struct {
	Path        string `json:"path"`
	Category    string `json:"category"`
	Reason      string `json:"reason"`
	Sensitivity string `json:"sensitivity"`
}

// dirView is a top-level directory and its file count.
type dirView struct {
	Name  string `json:"name"`
	Files int    `json:"files"`
}

// memoryView is the Boop.md project memory (§16).
type memoryView struct {
	Path   string `json:"path,omitempty"`
	Exists bool   `json:"exists"`
	// Content is the rendered document. It is the human-readable half of
	// Boop's state; the machine half lives in SQLite and is never merged into
	// it (§64.5).
	Content string `json:"content,omitempty"`
}

// projectView is everything discovery found.
type projectView struct {
	Root           string          `json:"root"`
	Name           string          `json:"name,omitempty"`
	Markers        []string        `json:"markers"`
	Languages      []languageView  `json:"languages"`
	PrimaryLang    string          `json:"primary_language,omitempty"`
	Frameworks     []string        `json:"frameworks"`
	Commands       []commandView   `json:"commands"`
	Git            gitView         `json:"git"`
	Sensitive      []sensitiveView `json:"sensitive"`
	Docs           []string        `json:"docs"`
	ImportantFiles []string        `json:"important_files"`
	TopLevelDirs   []dirView       `json:"top_level_dirs"`
	TestFiles      int             `json:"test_files"`
	FileCount      int             `json:"file_count"`
	// Truncated reports that the scan hit its depth or file bound, so the
	// counts are lower bounds.
	Truncated bool `json:"truncated"`
}

// projectResponse is the GET /api/project document.
type projectResponse struct {
	Project projectView `json:"project"`
	Memory  memoryView  `json:"memory"`
	// ScannedAt is when the discovery behind this response ran; Cached
	// reports that it was reused rather than repeated.
	ScannedAt time.Time `json:"scanned_at"`
	Cached    bool      `json:"cached"`
}

// prepResponse is the POST /api/project/prep document (§17).
type prepResponse struct {
	Project       projectView     `json:"project"`
	Memory        memoryView      `json:"memory"`
	MemoryPath    string          `json:"memory_path"`
	MemoryCreated bool            `json:"memory_created"`
	Sensitive     []sensitiveView `json:"sensitive"`
	Warnings      []string        `json:"warnings"`
	// Summary is the same text `/prep` prints in the CLI, so the WebUI and
	// the terminal report the same thing (§2.3).
	Summary string `json:"summary"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// handleProject serves GET /api/project.
func (s *Server) handleProject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if !s.requireApp(w) {
		return
	}
	root, ok := s.projectRoot(w)
	if !ok {
		return
	}

	refresh := r.URL.Query().Get("refresh") == "true"
	info, at, cached, err := s.discover(r.Context(), root, refresh)
	if err != nil {
		writeProjectError(w, "inspect the project", err)
		return
	}
	writeJSON(w, http.StatusOK, projectResponse{
		Project:   projectViewOf(info),
		Memory:    s.memoryView(),
		ScannedAt: at,
		Cached:    cached,
	})
}

// handleProjectSub serves everything under /api/project/.
func (s *Server) handleProjectSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/project/"), "/")
	if rest != "prep" {
		writeError(w, http.StatusNotFound, codeNotFound, "no such API endpoint: "+r.URL.Path)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	s.runPrep(w, r)
}

// runPrep executes the §17 preparation sequence.
//
// Prep writes Boop.md — and only Boop.md, only the blocks it owns — so it is a
// POST. After it writes, App.ReloadMemory swaps the freshly written file into
// the running runtime (the swap is atomic, so an in-flight turn keeps its own
// snapshot and the next turn sees the new one), which is what lets a `/prep`
// from the WebUI reach the model without a restart (issue #7). The response
// still reports the file read back from disk.
func (s *Server) runPrep(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	root, ok := s.projectRoot(w)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), projectPrepTimeout)
	defer cancel()

	report, err := project.Prep(ctx, root)
	if err != nil {
		writeProjectError(w, "prepare the project", err)
		return
	}
	// Prep rescanned the tree, so the cache is both stale and replaceable.
	s.storeProjectInfo(report.Info)
	// Swap the rewritten Boop.md into the runtime so the next turn's system
	// prompt carries it. Best-effort: the file on disk is the source of truth
	// and the response reads it back regardless.
	if s.app != nil {
		_ = s.app.ReloadMemory()
	}

	writeJSON(w, http.StatusOK, prepResponse{
		Project:       projectViewOf(report.Info),
		Memory:        memoryFromFile(report.MemoryPath),
		MemoryPath:    report.MemoryPath,
		MemoryCreated: report.MemoryCreated,
		Sensitive:     sensitiveViews(report.Sensitive),
		Warnings:      orEmpty(report.Warnings),
		Summary:       report.Summary(),
	})
}

// projectRoot resolves the workspace root, answering the request when there is
// none.
func (s *Server) projectRoot(w http.ResponseWriter) (string, bool) {
	if s.app.Workspace == nil {
		writeError(w, http.StatusServiceUnavailable, codeUnavailable,
			"this server has no workspace attached, so there is no project to inspect")
		return "", false
	}
	return s.app.Workspace.Root(), true
}

// writeProjectError maps a scan failure onto the error envelope. A deadline is
// reported as a timeout rather than as an internal error, because the caller
// can usefully retry it.
func writeProjectError(w http.ResponseWriter, what string, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(w, http.StatusGatewayTimeout, codeUnavailable,
			"the project scan did not finish in time; the tree may be very large")
		return
	}
	if errors.Is(err, context.Canceled) {
		writeError(w, http.StatusRequestTimeout, codeUnavailable, "the request was cancelled")
		return
	}
	writeError(w, http.StatusInternalServerError, codeInternal, "cannot "+what+": "+err.Error())
}

// discover returns the project description, reusing a recent scan.
func (s *Server) discover(ctx context.Context, root string, refresh bool) (*project.Info, time.Time, bool, error) {
	if !refresh {
		s.projectMu.Lock()
		entry := s.projectInfo
		s.projectMu.Unlock()
		if entry != nil && entry.info != nil &&
			entry.info.Root == root && s.now().Sub(entry.at) < projectCacheTTL {
			return entry.info, entry.at, true, nil
		}
	}

	scanCtx, cancel := context.WithTimeout(ctx, projectScanTimeout)
	defer cancel()
	info, err := project.Discover(scanCtx, root, project.Options{})
	if err != nil {
		return nil, time.Time{}, false, err
	}
	at := s.storeProjectInfo(info)
	return info, at, false, nil
}

// storeProjectInfo caches a discovery result and returns its timestamp.
func (s *Server) storeProjectInfo(info *project.Info) time.Time {
	at := s.now().UTC()
	if info == nil {
		return at
	}
	s.projectMu.Lock()
	s.projectInfo = &projectCacheEntry{info: info, at: at}
	s.projectMu.Unlock()
	return at
}

// memoryView renders the runtime's project memory.
func (s *Server) memoryView() memoryView {
	if s.app == nil {
		return memoryView{}
	}
	mem := s.app.Memory()
	if mem == nil {
		return memoryView{}
	}
	return memoryView{
		Path:    mem.Path(),
		Exists:  true,
		Content: string(mem.Render()),
	}
}

// memoryFromFile reads Boop.md from disk, which is what Prep just wrote.
func memoryFromFile(path string) memoryView {
	if path == "" {
		return memoryView{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return memoryView{Path: path}
	}
	return memoryView{Path: path, Exists: true, Content: string(data)}
}

// ---------------------------------------------------------------------------
// Projection
// ---------------------------------------------------------------------------

// projectViewOf converts discovery output into the API document.
//
// The internal types carry no JSON tags on purpose — they are not a wire
// format — so the mapping is explicit here rather than leaking Go field names
// into the frontend contract.
func projectViewOf(info *project.Info) projectView {
	view := projectView{
		Markers:        []string{},
		Languages:      []languageView{},
		Frameworks:     []string{},
		Commands:       []commandView{},
		Sensitive:      []sensitiveView{},
		Docs:           []string{},
		ImportantFiles: []string{},
		TopLevelDirs:   []dirView{},
		Git:            gitView{Remotes: []remoteView{}},
	}
	if info == nil {
		return view
	}
	view.Root = info.Root
	view.Name = info.Name
	view.Markers = orEmpty(info.Markers)
	view.Frameworks = orEmpty(info.Frameworks)
	view.Docs = orEmpty(info.Docs)
	view.ImportantFiles = orEmpty(info.ImportantFiles)
	view.PrimaryLang = info.PrimaryLanguage()
	view.TestFiles = info.TestFiles
	view.FileCount = info.FileCount
	view.Truncated = info.Truncated

	for _, l := range info.Languages {
		view.Languages = append(view.Languages, languageView{Name: l.Name, Files: l.Files})
	}
	for _, c := range info.Commands {
		view.Commands = append(view.Commands, commandView{
			Kind: string(c.Kind), Line: c.Line, Source: c.Source, Inferred: c.Inferred,
		})
	}
	for _, d := range info.TopLevelDirs {
		view.TopLevelDirs = append(view.TopLevelDirs, dirView{Name: d.Name, Files: d.Files})
	}
	view.Sensitive = sensitiveViews(info.Sensitive)
	view.Git = gitViewOf(info.Git)
	return view
}

func gitViewOf(g project.GitInfo) gitView {
	out := gitView{
		Present:    g.Present,
		Branch:     g.Branch,
		Detached:   g.Detached,
		Head:       g.Head,
		Dirty:      g.Dirty,
		DirtyKnown: g.DirtyKnown,
		DirtyFiles: g.DirtyFiles,
		Remotes:    []remoteView{},
	}
	for _, r := range g.Remotes {
		out.Remotes = append(out.Remotes, remoteView{Name: r.Name, URL: r.URL})
	}
	return out
}

func sensitiveViews(files []project.SensitiveFile) []sensitiveView {
	out := make([]sensitiveView, 0, len(files))
	for _, f := range files {
		out = append(out, sensitiveView{
			Path:        f.Path,
			Category:    f.Category,
			Reason:      f.Reason,
			Sensitivity: string(f.Sensitivity),
		})
	}
	return out
}

// orEmpty replaces a nil slice with an empty one, so the frontend never has to
// distinguish `null` from `[]`.
func orEmpty(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
