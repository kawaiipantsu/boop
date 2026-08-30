package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/kawaiipantsu/boop/internal/permissions"
)

// MultiEditTool applies multiple string replacements to a file in a single atomic call.
type MultiEditTool struct {
	ws *Workspace
}

// NewMultiEditTool returns a multi_edit tool confined to ws.
func NewMultiEditTool(ws *Workspace) *MultiEditTool {
	return &MultiEditTool{ws: ws}
}

// EditOperation is a single replacement step inside a multi_edit call.
type EditOperation struct {
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

type multiEditArgs struct {
	Path  string          `json:"path"`
	Edits []EditOperation `json:"edits"`
}

// MultiEditData is the structured result payload of a multi_edit call.
type MultiEditData struct {
	Path         string `json:"path"`
	EditsApplied int    `json:"edits_applied"`
	BytesBefore  int    `json:"bytes_before"`
	BytesAfter   int    `json:"bytes_after"`
	Diff         string `json:"diff,omitempty"`
}

// Name implements Tool.
func (t *MultiEditTool) Name() string { return "multi_edit" }

// Description implements Tool.
func (t *MultiEditTool) Description() string {
	return "Apply multiple string replacements to an existing file in a single atomic call. " +
		"Edits are applied sequentially: each edit sees the result of previous edits. " +
		"If any edit fails to match uniquely, no changes are written."
}

// Schema implements Tool.
func (t *MultiEditTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "File path relative to the project root. The file must already exist.",
			},
			"edits": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"old_string": map[string]any{
							"type":        "string",
							"description": "Exact text to find, including indentation and line breaks.",
						},
						"new_string": map[string]any{
							"type":        "string",
							"description": "Replacement text.",
						},
						"replace_all": map[string]any{
							"type":        "boolean",
							"default":     false,
							"description": "Replace every occurrence instead of requiring a unique match.",
						},
					},
					"required":             []string{"old_string", "new_string"},
					"additionalProperties": false,
				},
				"description": "Ordered list of replacements to apply sequentially.",
			},
		},
		"required":             []string{"path", "edits"},
		"additionalProperties": false,
	}
}

// Permission implements Tool.
func (t *MultiEditTool) Permission(call Call) (permissions.Action, error) {
	var a multiEditArgs
	if err := call.Bind(&a); err != nil {
		return permissions.Action{}, err
	}
	abs, display, contained := fsTarget(t.ws, a.Path)

	var details strings.Builder
	details.WriteString(fsTargetDetail(abs, contained))
	for i, e := range a.Edits {
		fmt.Fprintf(&details, "\n\n--- edit #%d (old)\n%s\n\n+++ edit #%d (new)\n%s",
			i+1, fsPreview(e.OldString, 10), i+1, fsPreview(e.NewString, 10))
	}

	return permissions.Action{
		Category: permissions.CatFilesystemWrite,
		Risk:     permissions.RiskMedium,
		Tool:     t.Name(),
		Summary:  fmt.Sprintf("Multi-edit %s (%d operations)", display, len(a.Edits)),
		Detail:   details.String(),
		Paths:    []string{abs},
	}, nil
}

// Execute applies all edits sequentially and writes the file atomically.
func (t *MultiEditTool) Execute(ctx context.Context, call Call) (Result, error) {
	var a multiEditArgs
	if err := call.Bind(&a); err != nil {
		return Errorf(call, "invalid arguments: %v", err), nil
	}
	if strings.TrimSpace(a.Path) == "" {
		return Errorf(call, "the %q argument is required", "path"), nil
	}
	if len(a.Edits) == 0 {
		return Errorf(call, "edits array must contain at least one replacement operation"), nil
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	abs, err := t.ws.Resolve(a.Path)
	if err != nil {
		return Errorf(call, "cannot edit %q: %v", a.Path, err), nil
	}
	rel := t.ws.Rel(abs)

	info, err := os.Stat(abs)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Errorf(call, "file not found: %s; use the write tool to create it", rel), nil
	case err != nil:
		return Errorf(call, "cannot edit %s: %v", rel, err), nil
	case info.IsDir():
		return Errorf(call, "%s is a directory; choose a file path", rel), nil
	case info.Size() > fsMaxScanBytes:
		return Errorf(call, "%s is %s, which is too large to edit safely (limit %s)",
			rel, fsHumanBytes(info.Size()), fsHumanBytes(fsMaxScanBytes)), nil
	}

	raw, err := os.ReadFile(abs)
	if err != nil {
		return Errorf(call, "cannot read %s: %v", rel, err), nil
	}
	sniff := raw
	if len(sniff) > fsSniffBytes {
		sniff = sniff[:fsSniffBytes]
	}
	if fsIsBinary(sniff) {
		return Errorf(call, "%s is a binary file and cannot be edited as text", rel), nil
	}

	currentContent := string(raw)
	var diffSummaries []string

	for i, op := range a.Edits {
		if op.OldString == "" {
			return Errorf(call, "edit #%d: old_string must not be empty", i+1), nil
		}
		if op.OldString == op.NewString {
			continue // No-op, skip
		}

		offsets := fsIndexAll(currentContent, op.OldString)
		lines := fsOffsetLines(currentContent, offsets)

		switch {
		case len(offsets) == 0:
			return fsFailure(call, MultiEditData{Path: rel, BytesBefore: len(raw)},
				"edit #%d of %d failed: old_string was not found in %s, so nothing was changed.%s",
				i+1, len(a.Edits), rel, fsEditHint(currentContent, op.OldString)), nil
		case len(offsets) > 1 && !op.ReplaceAll:
			return fsFailure(call, MultiEditData{Path: rel, BytesBefore: len(raw)},
				"edit #%d of %d failed: old_string is ambiguous (%d matches in %s on line%s %s), so nothing was changed. "+
					"Add surrounding context or set replace_all=true.",
				i+1, len(a.Edits), len(offsets), rel, fsPlural(len(lines)), fsJoinInts(lines)), nil
		}

		count := len(offsets)
		if !op.ReplaceAll {
			count = 1
			offsets = offsets[:1]
		}

		hunkDiff := fsRenderHunks(rel, currentContent, offsets, op.OldString, op.NewString)
		diffSummaries = append(diffSummaries, hunkDiff)

		currentContent = strings.Replace(currentContent, op.OldString, op.NewString, count)
	}

	perm := info.Mode().Perm()
	if err := fsAtomicWrite(abs, []byte(currentContent), perm); err != nil {
		return Errorf(call, "cannot write %s: %v", rel, err), nil
	}

	fullDiff := strings.Join(diffSummaries, "\n")
	content := fmt.Sprintf("Multi-edited %s: applied %d edit operation(s) (%s → %s).\n\n%s",
		rel, len(a.Edits), fsHumanBytes(int64(len(raw))), fsHumanBytes(int64(len(currentContent))), fullDiff)

	return fsResult(call, content, MultiEditData{
		Path:         rel,
		EditsApplied: len(a.Edits),
		BytesBefore:  len(raw),
		BytesAfter:   len(currentContent),
		Diff:         fullDiff,
	}), nil
}
