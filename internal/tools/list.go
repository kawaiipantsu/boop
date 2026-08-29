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

	"github.com/kawaiipantsu/boop/internal/permissions"
)

// Listing limits. Recursion is bounded twice — by depth and by entry count —
// because either alone can still produce an unusable result in a real project.
const (
	fsDefaultListDepth = 3
	fsMaxListDepth     = 10
	fsDefaultListLimit = 300
	fsMaxListLimit     = 2000
)

// fsNoiseDirs are directory names skipped by list, find and search unless the
// caller opts in. They are generated or vendored: including them buries the
// project's own files and wastes context.
var fsNoiseDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	"target":       true,
	"dist":         true,
	"__pycache__":  true,
}

// ListTool enumerates the contents of a directory inside the workspace.
type ListTool struct{ ws *Workspace }

// NewListTool returns a list tool confined to ws.
func NewListTool(ws *Workspace) *ListTool { return &ListTool{ws: ws} }

type listArgs struct {
	Path           string `json:"path"`
	Recursive      bool   `json:"recursive"`
	MaxDepth       int    `json:"max_depth"`
	Limit          int    `json:"limit"`
	IncludeIgnored bool   `json:"include_ignored"`
}

// ListEntry is one row of a directory listing.
type ListEntry struct {
	// Path is relative to the listed directory.
	Path string `json:"path"`
	Name string `json:"name"`
	// Type is one of "file", "dir", "symlink" or "other".
	Type string `json:"type"`
	Size int64  `json:"size"`
	// Target is the destination of a symlink, when it can be read.
	Target string `json:"target,omitempty"`
}

// ListData is the structured payload of a list result.
type ListData struct {
	Path        string      `json:"path"`
	Entries     []ListEntry `json:"entries"`
	Directories int         `json:"directories"`
	Files       int         `json:"files"`
	// Skipped names the noise directories that were not descended into.
	Skipped   []string `json:"skipped,omitempty"`
	Truncated bool     `json:"truncated"`
	MaxDepth  int      `json:"max_depth"`
}

// Name implements Tool.
func (t *ListTool) Name() string { return "list" }

// Description implements Tool.
func (t *ListTool) Description() string {
	return "List the contents of a directory with entry type and size. Optionally recurse, " +
		"bounded by max_depth and limit. Generated directories such as .git, node_modules, " +
		"vendor, target, dist and __pycache__ are skipped unless include_ignored is set."
}

// Schema implements Tool.
func (t *ListTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"default":     ".",
				"description": "Directory relative to the project root. Defaults to the project root.",
			},
			"recursive": map[string]any{
				"type":        "boolean",
				"default":     false,
				"description": "Descend into subdirectories.",
			},
			"max_depth": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"maximum":     fsMaxListDepth,
				"description": "Maximum recursion depth. Defaults to 1, or 3 when recursive is true.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"maximum":     fsMaxListLimit,
				"default":     fsDefaultListLimit,
				"description": "Maximum number of entries to return.",
			},
			"include_ignored": map[string]any{
				"type":        "boolean",
				"default":     false,
				"description": "Include generated and vendored directories that are skipped by default.",
			},
		},
		"additionalProperties": false,
	}
}

// Permission implements Tool.
func (t *ListTool) Permission(call Call) (permissions.Action, error) {
	var a listArgs
	if err := call.Bind(&a); err != nil {
		return permissions.Action{}, err
	}
	abs, display, contained := fsTarget(t.ws, fsOrDot(a.Path))
	summary := fmt.Sprintf("List %s", display)
	if a.Recursive {
		summary = fmt.Sprintf("List %s recursively", display)
	}
	return permissions.Action{
		Category: permissions.CatFilesystemRead,
		Risk:     fsListRisk(contained),
		Tool:     t.Name(),
		Summary:  summary,
		Detail:   fsTargetDetail(abs, contained),
		Paths:    []string{abs},
	}, nil
}

// Execute implements Tool.
func (t *ListTool) Execute(ctx context.Context, call Call) (Result, error) {
	var a listArgs
	if err := call.Bind(&a); err != nil {
		return Errorf(call, "invalid arguments: %v", err), nil
	}
	abs, err := t.ws.Resolve(fsOrDot(a.Path))
	if err != nil {
		return Errorf(call, "cannot list %q: %v", a.Path, err), nil
	}
	rel := fsOrDot(t.ws.Rel(abs))

	info, err := os.Stat(abs)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Errorf(call, "directory not found: %s", rel), nil
	case err != nil:
		return Errorf(call, "cannot list %s: %v", rel, err), nil
	case !info.IsDir():
		return Errorf(call, "%s is a file, not a directory; use the read tool", rel), nil
	}

	depth := 1
	if a.Recursive {
		depth = fsDefaultListDepth
	}
	if a.MaxDepth > 0 {
		depth = a.MaxDepth
	}
	depth = min(depth, fsMaxListDepth)
	limit := fsClamp(a.Limit, fsDefaultListLimit, fsMaxListLimit)

	w := &fsLister{
		root:           abs,
		maxDepth:       depth,
		limit:          limit,
		includeIgnored: a.IncludeIgnored,
	}
	if err := w.walk(ctx, abs, 1); err != nil {
		if ctx.Err() != nil {
			return Result{}, err
		}
		return Errorf(call, "cannot list %s: %v", rel, err), nil
	}

	data := ListData{
		Path:      rel,
		Entries:   w.entries,
		Skipped:   w.skipped,
		Truncated: w.truncated,
		MaxDepth:  depth,
	}
	for _, e := range w.entries {
		if e.Type == "dir" {
			data.Directories++
		} else {
			data.Files++
		}
	}
	return fsResult(call, fsRenderListing(rel, data, depth), data), nil
}

// fsLister accumulates a bounded directory listing.
type fsLister struct {
	root           string
	maxDepth       int
	limit          int
	includeIgnored bool

	entries   []ListEntry
	skipped   []string
	truncated bool
}

// walk lists dir at the given depth, recursing while budget allows.
//
// Symlinked directories are recorded but never descended into: following them
// risks both cycles and leaving the workspace.
func (l *fsLister) walk(ctx context.Context, dir string, depth int) error {
	if l.truncated {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	items, err := os.ReadDir(dir)
	if err != nil {
		if depth > 1 && (errors.Is(err, fs.ErrPermission) || errors.Is(err, fs.ErrNotExist)) {
			return nil // an unreadable subdirectory must not fail the whole listing
		}
		return err
	}
	sort.Slice(items, func(i, j int) bool {
		di, dj := items[i].IsDir(), items[j].IsDir()
		if di != dj {
			return di
		}
		return items[i].Name() < items[j].Name()
	})

	for _, item := range items {
		if l.truncated {
			return nil
		}
		name := item.Name()
		if item.IsDir() && !l.includeIgnored && fsNoiseDirs[name] {
			l.skipped = append(l.skipped, fsRelSlash(l.root, filepath.Join(dir, name)))
			continue
		}
		full := filepath.Join(dir, name)
		entry := ListEntry{
			Path: fsRelSlash(l.root, full),
			Name: name,
			Type: fsEntryType(item),
		}
		if entry.Type == "symlink" {
			if target, err := os.Readlink(full); err == nil {
				entry.Target = target
			}
		}
		if info, err := item.Info(); err == nil && entry.Type == "file" {
			entry.Size = info.Size()
		}
		if len(l.entries) >= l.limit {
			l.truncated = true
			return nil
		}
		l.entries = append(l.entries, entry)

		if item.IsDir() && depth < l.maxDepth {
			if err := l.walk(ctx, full, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// fsRenderListing formats a listing for the model.
func fsRenderListing(rel string, data ListData, depth int) string {
	if len(data.Entries) == 0 {
		msg := fmt.Sprintf("%s is empty.", rel)
		if len(data.Skipped) > 0 {
			msg += fmt.Sprintf(" (%s skipped; pass include_ignored=true to see %s)",
				strings.Join(data.Skipped, ", "), map[bool]string{true: "it", false: "them"}[len(data.Skipped) == 1])
		}
		return msg
	}
	width := 0
	for _, e := range data.Entries {
		if n := len(e.Path) + 1; n > width {
			width = n
		}
	}
	width = min(width, 60)

	var b strings.Builder
	fmt.Fprintf(&b, "%s — %d director%s, %d file%s (depth %d)\n\n",
		rel, data.Directories, map[bool]string{true: "y", false: "ies"}[data.Directories == 1],
		data.Files, fsPlural(data.Files), depth)
	for _, e := range data.Entries {
		label := e.Path
		switch e.Type {
		case "dir":
			label += "/"
			fmt.Fprintf(&b, "%s\n", label)
		case "symlink":
			if e.Target != "" {
				fmt.Fprintf(&b, "%-*s -> %s\n", width, label, e.Target)
			} else {
				fmt.Fprintf(&b, "%-*s (symlink)\n", width, label)
			}
		default:
			fmt.Fprintf(&b, "%-*s %s\n", width, label, fsHumanBytes(e.Size))
		}
	}
	if len(data.Skipped) > 0 {
		fmt.Fprintf(&b, "\n[skipped %s; pass include_ignored=true to include them]",
			strings.Join(data.Skipped, ", "))
	}
	if data.Truncated {
		fmt.Fprintf(&b, "\n[truncated at %d entries; narrow the path or raise limit]", len(data.Entries))
	}
	return strings.TrimRight(b.String(), "\n")
}

// fsEntryType classifies a directory entry.
func fsEntryType(e os.DirEntry) string {
	switch {
	case e.Type()&os.ModeSymlink != 0:
		return "symlink"
	case e.IsDir():
		return "dir"
	case e.Type().IsRegular():
		return "file"
	default:
		return "other"
	}
}

// fsRelSlash renders full relative to root using forward slashes, so results
// look the same on every platform.
func fsRelSlash(root, full string) string {
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return filepath.ToSlash(full)
	}
	return path.Clean(filepath.ToSlash(rel))
}

// fsOrDot substitutes "." for an empty path.
func fsOrDot(p string) string {
	if strings.TrimSpace(p) == "" {
		return "."
	}
	return p
}

// fsClamp applies a default for non-positive values and an upper bound.
func fsClamp(v, def, max int) int {
	if v <= 0 {
		return def
	}
	return min(v, max)
}

// fsListRisk grades a directory traversal.
func fsListRisk(contained bool) permissions.Risk {
	if !contained {
		return permissions.RiskCritical
	}
	return permissions.RiskLow
}
