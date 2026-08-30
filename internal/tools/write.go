package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/kawaiipantsu/boop/internal/permissions"
)

// fsDefaultFileMode is used for files the tools create. Existing permissions
// are always preserved instead.
const fsDefaultFileMode os.FileMode = 0o644

// fsDefaultDirMode is used for parent directories the tools create.
const fsDefaultDirMode os.FileMode = 0o755

// WriteTool creates or replaces a file inside the workspace.
type WriteTool struct{ ws *Workspace }

// NewWriteTool returns a write tool confined to ws.
func NewWriteTool(ws *Workspace) *WriteTool { return &WriteTool{ws: ws} }

type writeArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// WriteData is the structured payload of a write result.
type WriteData struct {
	Path          string `json:"path"`
	Bytes         int    `json:"bytes"`
	Lines         int    `json:"lines"`
	Created       bool   `json:"created"`
	PreviousBytes int64  `json:"previous_bytes"`
}

// Name implements Tool.
func (t *WriteTool) Name() string { return "write" }

// Description implements Tool.
func (t *WriteTool) Description() string {
	return "Write a file, creating it and any missing parent directories, or replacing it " +
		"entirely if it exists. The write is atomic. To change part of an existing file, prefer " +
		"the edit tool so unrelated content cannot be lost."
}

// Schema implements Tool.
func (t *WriteTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "File path relative to the project root. Parent directories are created as needed.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "Full contents of the file. Any previous contents are replaced.",
			},
		},
		"required":             []string{"path", "content"},
		"additionalProperties": false,
	}
}

// Permission implements Tool.
func (t *WriteTool) Permission(call Call) (permissions.Action, error) {
	var a writeArgs
	if err := call.Bind(&a); err != nil {
		return permissions.Action{}, err
	}
	abs, display, contained := fsTarget(t.ws, a.Path)
	size := int64(len(a.Content))

	var (
		summary     string
		destructive bool
	)
	if info, err := os.Stat(abs); err == nil && !info.IsDir() {
		destructive = true
		summary = fmt.Sprintf("Overwrite %s with %s, replacing %s",
			display, fsHumanBytes(size), fsHumanBytes(info.Size()))
	} else {
		summary = fmt.Sprintf("Write %s to %s", fsHumanBytes(size), display)
	}
	return permissions.Action{
		Category: permissions.CatFilesystemWrite,
		Risk:     fsWriteRisk(display, contained, destructive),
		Tool:     t.Name(),
		Summary:  summary,
		Detail:   fsTargetDetail(abs, contained) + "\n\n" + fsPreview(a.Content, 20),
		Paths:    []string{abs},
	}, nil
}

// Execute implements Tool.
func (t *WriteTool) Execute(ctx context.Context, call Call) (Result, error) {
	var a writeArgs
	if err := call.Bind(&a); err != nil {
		return Errorf(call, "invalid arguments: %v", err), nil
	}
	if strings.TrimSpace(a.Path) == "" {
		return Errorf(call, "the %q argument is required", "path"), nil
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	abs, err := t.ws.Resolve(a.Path)
	if err != nil {
		return Errorf(call, "cannot write %q: %v", a.Path, err), nil
	}
	rel := t.ws.Rel(abs)

	perm := fsDefaultFileMode
	created := true
	var previous int64
	info, err := os.Stat(abs)
	switch {
	case err == nil && info.IsDir():
		return Errorf(call, "%s is a directory; choose a file path", rel), nil
	case err == nil:
		created = false
		previous = info.Size()
		perm = info.Mode().Perm()
	case !errors.Is(err, fs.ErrNotExist):
		return Errorf(call, "cannot write %s: %v", rel, err), nil
	}

	data := []byte(a.Content)
	if err := fsAtomicWrite(abs, data, perm); err != nil {
		return Errorf(call, "cannot write %s: %v", rel, err), nil
	}

	verb := "Created"
	suffix := ""
	if !created {
		verb = "Replaced"
		suffix = fmt.Sprintf(" (was %s)", fsHumanBytes(previous))
	}
	lines := fsCountLines(a.Content)
	content := fmt.Sprintf("%s %s: wrote %s, %d line%s%s.",
		verb, rel, fsHumanBytes(int64(len(data))), lines, fsPlural(lines), suffix)
	return fsResult(call, content, WriteData{
		Path: rel, Bytes: len(data), Lines: lines, Created: created, PreviousBytes: previous,
	}), nil
}

// fsAtomicWrite writes data to abs through a temporary file in the same
// directory followed by a rename.
//
// The rename is what makes this safe: a concurrent reader sees either the old
// file or the new one, and a failure part-way through leaves the original
// intact rather than truncated.
func fsAtomicWrite(abs string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, fsDefaultDirMode); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".boop-"+filepath.Base(abs)+".*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // no-op once the rename has succeeded

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, perm); err != nil {
		return err
	}
	return os.Rename(name, abs)
}

// fsCountLines counts the lines in s, treating a trailing newline as a
// terminator rather than the start of an empty final line.
func fsCountLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// fsPreview renders at most max lines of s for an approval dialog.
func fsPreview(s string, max int) string {
	if s == "" {
		return "(empty file)"
	}
	lines := strings.Split(s, "\n")
	if len(lines) > max {
		kept := strings.Join(lines[:max], "\n")
		return fmt.Sprintf("%s\n… %d more line%s", kept, len(lines)-max, fsPlural(len(lines)-max))
	}
	return s
}
