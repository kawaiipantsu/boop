package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Workspace confines filesystem tools to a project directory.
//
// Confinement is a security boundary, not a convenience: it is enforced after
// symlink resolution so a link inside the project cannot be used to reach out
// of it.
type Workspace struct {
	root string
}

// NewWorkspace returns a Workspace rooted at dir, which must exist.
func NewWorkspace(dir string) (*Workspace, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("workspace root %q: %w", dir, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("workspace root %q: %w", dir, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("workspace root %q: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace root %q is not a directory", dir)
	}
	return &Workspace{root: resolved}, nil
}

// Root returns the resolved absolute workspace root.
func (w *Workspace) Root() string { return w.root }

// Resolve turns a tool-supplied path into an absolute path inside the
// workspace, rejecting anything that escapes it.
//
// The path may be relative to the root or absolute. Existing paths are
// symlink-resolved before the containment check; for a path that does not yet
// exist, the nearest existing parent is resolved instead so that creating a
// file through an escaping symlink is also rejected.
func (w *Workspace) Resolve(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty path")
	}
	candidate := path
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(w.root, candidate)
	}
	candidate = filepath.Clean(candidate)

	resolved, err := resolveNearest(candidate)
	if err != nil {
		return "", err
	}
	if !w.contains(resolved) {
		return "", fmt.Errorf("path %q escapes the workspace root %q", path, w.root)
	}
	return candidate, nil
}

// contains reports whether abs lies at or beneath the workspace root.
func (w *Workspace) contains(abs string) bool {
	if abs == w.root {
		return true
	}
	rel, err := filepath.Rel(w.root, abs)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Rel renders abs relative to the workspace root for display, falling back to
// the absolute path when it lies outside.
func (w *Workspace) Rel(abs string) string {
	rel, err := filepath.Rel(w.root, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return abs
	}
	return rel
}

// resolveNearest resolves symlinks for path, walking up to the nearest
// existing ancestor when path itself does not exist yet.
func resolveNearest(path string) (string, error) {
	remainder := ""
	current := path
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			if remainder == "" {
				return resolved, nil
			}
			return filepath.Join(resolved, remainder), nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("resolve %q: %w", path, err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			// Reached the filesystem root without finding anything.
			return path, nil
		}
		remainder = filepath.Join(filepath.Base(current), remainder)
		current = parent
	}
}
