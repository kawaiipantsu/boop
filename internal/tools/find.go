package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/boop-dev/boop/internal/permissions"
)

// Find limits.
const (
	fsDefaultFindLimit = 200
	fsMaxFindLimit     = 2000
)

// FindTool locates files by name or glob pattern.
type FindTool struct{ ws *Workspace }

// NewFindTool returns a find tool confined to ws.
func NewFindTool(ws *Workspace) *FindTool { return &FindTool{ws: ws} }

type findArgs struct {
	Pattern        string `json:"pattern"`
	Path           string `json:"path"`
	Type           string `json:"type"`
	Limit          int    `json:"limit"`
	IncludeIgnored bool   `json:"include_ignored"`
}

// FindData is the structured payload of a find result.
type FindData struct {
	Pattern string `json:"pattern"`
	Root    string `json:"root"`
	// Matches are paths relative to the project root.
	Matches   []string `json:"matches"`
	Scanned   int      `json:"scanned"`
	Truncated bool     `json:"truncated"`
}

// Name implements Tool.
func (t *FindTool) Name() string { return "find" }

// Description implements Tool.
func (t *FindTool) Description() string {
	return "Find files and directories by name or glob pattern. A pattern without a slash is " +
		"matched against the base name (\"*_test.go\"); a pattern with a slash is matched against " +
		"the path relative to the search root, where ** spans directories (\"internal/**/*.go\"). " +
		"Generated directories are skipped unless include_ignored is set."
}

// Schema implements Tool.
func (t *FindTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{
				"type":        "string",
				"description": "Glob pattern, e.g. \"*.go\", \"Makefile\" or \"cmd/**/main.go\".",
			},
			"path": map[string]any{
				"type":        "string",
				"default":     ".",
				"description": "Directory to search under, relative to the project root.",
			},
			"type": map[string]any{
				"type":        "string",
				"enum":        []string{"any", "file", "dir"},
				"default":     "any",
				"description": "Restrict results to files or directories.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"maximum":     fsMaxFindLimit,
				"default":     fsDefaultFindLimit,
				"description": "Maximum number of matches to return.",
			},
			"include_ignored": map[string]any{
				"type":        "boolean",
				"default":     false,
				"description": "Search inside generated and vendored directories too.",
			},
		},
		"required":             []string{"pattern"},
		"additionalProperties": false,
	}
}

// Permission implements Tool.
func (t *FindTool) Permission(call Call) (permissions.Action, error) {
	var a findArgs
	if err := call.Bind(&a); err != nil {
		return permissions.Action{}, err
	}
	abs, display, contained := fsTarget(t.ws, fsOrDot(a.Path))
	return permissions.Action{
		Category: permissions.CatFilesystemRead,
		Risk:     fsListRisk(contained),
		Tool:     t.Name(),
		Summary:  fmt.Sprintf("Find files matching %q under %s", a.Pattern, display),
		Detail:   fsTargetDetail(abs, contained),
		Paths:    []string{abs},
	}, nil
}

// Execute implements Tool.
func (t *FindTool) Execute(ctx context.Context, call Call) (Result, error) {
	var a findArgs
	if err := call.Bind(&a); err != nil {
		return Errorf(call, "invalid arguments: %v", err), nil
	}
	if strings.TrimSpace(a.Pattern) == "" {
		return Errorf(call, "the %q argument is required", "pattern"), nil
	}
	if err := fsValidateGlob(a.Pattern); err != nil {
		return Errorf(call, "invalid pattern %q: %v", a.Pattern, err), nil
	}
	switch a.Type {
	case "", "any", "file", "dir":
	default:
		return Errorf(call, "invalid type %q: expected \"any\", \"file\" or \"dir\"", a.Type), nil
	}

	abs, err := t.ws.Resolve(fsOrDot(a.Path))
	if err != nil {
		return Errorf(call, "cannot search %q: %v", a.Path, err), nil
	}
	rel := fsOrDot(t.ws.Rel(abs))
	info, err := os.Stat(abs)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Errorf(call, "directory not found: %s", rel), nil
	case err != nil:
		return Errorf(call, "cannot search %s: %v", rel, err), nil
	case !info.IsDir():
		return Errorf(call, "%s is a file, not a directory", rel), nil
	}

	limit := fsClamp(a.Limit, fsDefaultFindLimit, fsMaxFindLimit)
	data := FindData{Pattern: a.Pattern, Root: rel}

	err = fsWalk(ctx, abs, a.IncludeIgnored, func(full string, entry os.DirEntry) error {
		data.Scanned++
		if len(data.Matches) >= limit {
			data.Truncated = true
			return fsErrStopWalk
		}
		isDir := entry.IsDir()
		if (a.Type == "file" && isDir) || (a.Type == "dir" && !isDir) {
			return nil
		}
		relPath := fsRelSlash(abs, full)
		if fsGlobMatch(a.Pattern, relPath) {
			data.Matches = append(data.Matches, fsJoinSlash(rel, relPath))
		}
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, err
		}
		return Errorf(call, "cannot search %s: %v", rel, err), nil
	}
	sort.Strings(data.Matches)

	if len(data.Matches) == 0 {
		return fsResult(call, fmt.Sprintf(
			"No files matching %q under %s (%d entries scanned).", a.Pattern, rel, data.Scanned), data), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d match%s for %q under %s\n\n",
		len(data.Matches), map[bool]string{true: "", false: "es"}[len(data.Matches) == 1], a.Pattern, rel)
	b.WriteString(strings.Join(data.Matches, "\n"))
	if data.Truncated {
		fmt.Fprintf(&b, "\n\n[truncated at %d matches; narrow the pattern or raise limit]", limit)
	}
	return fsResult(call, b.String(), data), nil
}

// fsErrStopWalk halts fsWalk without being an error condition.
var fsErrStopWalk = errors.New("stop walk")

// fsWalk visits every entry beneath root in a deterministic order, skipping
// noise directories and never following symlinked directories.
//
// It is shared by find and search so both agree on what "the project tree"
// means. Returning fsErrStopWalk from fn stops the walk cleanly.
func fsWalk(ctx context.Context, root string, includeIgnored bool, fn func(full string, entry os.DirEntry) error) error {
	visited := 0
	var walk func(dir string) error
	walk = func(dir string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		items, err := os.ReadDir(dir)
		if err != nil {
			if dir != root && (errors.Is(err, fs.ErrPermission) || errors.Is(err, fs.ErrNotExist)) {
				return nil
			}
			return err
		}
		sort.Slice(items, func(i, j int) bool { return items[i].Name() < items[j].Name() })
		for _, item := range items {
			name := item.Name()
			if item.IsDir() && !includeIgnored && fsNoiseDirs[name] {
				continue
			}
			full := filepath.Join(dir, name)
			visited++
			if visited%fsCancelInterval == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			if err := fn(full, item); err != nil {
				return err
			}
			// Symlinked directories are not descended: they can form cycles
			// and can point outside the workspace.
			if item.IsDir() && item.Type()&os.ModeSymlink == 0 {
				if err := walk(full); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(root); err != nil && !errors.Is(err, fsErrStopWalk) {
		return err
	}
	return nil
}

// fsValidateGlob reports whether pattern is syntactically usable.
func fsValidateGlob(pattern string) error {
	for _, seg := range strings.Split(filepath.ToSlash(pattern), "/") {
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, ""); err != nil {
			return err
		}
	}
	return nil
}

// fsGlobMatch matches a slash-separated relative path against pattern.
//
// A pattern without a separator matches the base name only, which is what a
// caller asking for "*_test.go" means. A pattern with a separator matches the
// whole relative path, with "**" spanning any number of segments.
func fsGlobMatch(pattern, relPath string) bool {
	pattern = filepath.ToSlash(pattern)
	if !strings.Contains(pattern, "/") {
		ok, err := path.Match(pattern, path.Base(relPath))
		return err == nil && ok
	}
	pattern = strings.TrimPrefix(pattern, "./")
	return fsMatchSegments(strings.Split(pattern, "/"), strings.Split(relPath, "/"))
}

// fsMatchSegments matches path segments against pattern segments, treating
// "**" as zero or more segments.
func fsMatchSegments(pat, seg []string) bool {
	switch {
	case len(pat) == 0:
		return len(seg) == 0
	case pat[0] == "**":
		for i := 0; i <= len(seg); i++ {
			if fsMatchSegments(pat[1:], seg[i:]) {
				return true
			}
		}
		return false
	case len(seg) == 0:
		return false
	default:
		ok, err := path.Match(pat[0], seg[0])
		if err != nil || !ok {
			return false
		}
		return fsMatchSegments(pat[1:], seg[1:])
	}
}

// fsJoinSlash joins a search root and a relative match for display.
func fsJoinSlash(root, rel string) string {
	if root == "." || root == "" {
		return rel
	}
	return path.Join(filepath.ToSlash(root), rel)
}
