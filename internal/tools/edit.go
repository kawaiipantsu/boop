package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"github.com/boop-dev/boop/internal/permissions"
)

// fsMaxEditHunks bounds the diff summary returned to the model.
const fsMaxEditHunks = 3

// fsMaxHunkLines bounds each side of a rendered hunk.
const fsMaxHunkLines = 8

// EditTool performs exact string replacement inside an existing file.
type EditTool struct{ ws *Workspace }

// NewEditTool returns an edit tool confined to ws.
func NewEditTool(ws *Workspace) *EditTool { return &EditTool{ws: ws} }

type editArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

// EditData is the structured payload of an edit result.
type EditData struct {
	Path         string `json:"path"`
	Replacements int    `json:"replacements"`
	BytesBefore  int    `json:"bytes_before"`
	BytesAfter   int    `json:"bytes_after"`
	// MatchLines are the 1-based line numbers where old_string was found.
	// It is populated on ambiguity failures too, so a UI can point at them.
	MatchLines []int `json:"match_lines,omitempty"`
	// Diff is the human-readable summary of what changed.
	Diff string `json:"diff,omitempty"`
}

// Name implements Tool.
func (t *EditTool) Name() string { return "edit" }

// Description implements Tool.
func (t *EditTool) Description() string {
	return "Replace an exact string in an existing file. old_string must appear exactly once " +
		"unless replace_all is true, so include enough surrounding context to make it unique. " +
		"Whitespace and indentation must match the file exactly."
}

// Schema implements Tool.
func (t *EditTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "File path relative to the project root. The file must already exist.",
			},
			"old_string": map[string]any{
				"type":        "string",
				"description": "Exact text to find, including indentation and line breaks.",
			},
			"new_string": map[string]any{
				"type":        "string",
				"description": "Replacement text. Use an empty string to delete the matched text.",
			},
			"replace_all": map[string]any{
				"type":        "boolean",
				"default":     false,
				"description": "Replace every occurrence instead of requiring a unique match.",
			},
		},
		"required":             []string{"path", "old_string", "new_string"},
		"additionalProperties": false,
	}
}

// Permission implements Tool. The occurrence count is not known until the file
// is read, so replace_all is treated as the higher risk case up front.
func (t *EditTool) Permission(call Call) (permissions.Action, error) {
	var a editArgs
	if err := call.Bind(&a); err != nil {
		return permissions.Action{}, err
	}
	abs, display, contained := fsTarget(t.ws, a.Path)
	scope := "one occurrence"
	if a.ReplaceAll {
		scope = "every occurrence"
	}
	detail := fmt.Sprintf("%s\n\n--- old\n%s\n\n+++ new\n%s",
		fsTargetDetail(abs, contained), fsPreview(a.OldString, 20), fsPreview(a.NewString, 20))
	return permissions.Action{
		Category: permissions.CatFilesystemWrite,
		Risk:     fsWriteRisk(display, contained, a.ReplaceAll),
		Tool:     t.Name(),
		Summary:  fmt.Sprintf("Edit %s, replacing %s", display, scope),
		Detail:   detail,
		Paths:    []string{abs},
	}, nil
}

// Execute implements Tool.
func (t *EditTool) Execute(ctx context.Context, call Call) (Result, error) {
	var a editArgs
	if err := call.Bind(&a); err != nil {
		return Errorf(call, "invalid arguments: %v", err), nil
	}
	if strings.TrimSpace(a.Path) == "" {
		return Errorf(call, "the %q argument is required", "path"), nil
	}
	if a.OldString == "" {
		return Errorf(call, "old_string must not be empty; use the write tool to create or replace a whole file"), nil
	}
	if a.OldString == a.NewString {
		return Errorf(call, "old_string and new_string are identical, so there is nothing to change"), nil
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

	before := string(raw)
	offsets := fsIndexAll(before, a.OldString)
	lines := fsOffsetLines(before, offsets)

	switch {
	case len(offsets) == 0:
		return fsFailure(call, EditData{Path: rel, BytesBefore: len(raw)},
			"old_string was not found in %s, so nothing was changed.%s",
			rel, fsEditHint(before, a.OldString)), nil
	case len(offsets) > 1 && !a.ReplaceAll:
		return fsFailure(call, EditData{Path: rel, BytesBefore: len(raw), MatchLines: lines},
			"old_string is ambiguous: it matches %d times in %s (line%s %s), so nothing was changed. "+
				"Add surrounding context to make the match unique, or set replace_all=true to change all of them.",
			len(offsets), rel, fsPlural(len(lines)), fsJoinInts(lines)), nil
	}

	count := len(offsets)
	if !a.ReplaceAll {
		count = 1
		offsets = offsets[:1]
	}
	after := strings.Replace(before, a.OldString, a.NewString, count)

	perm := info.Mode().Perm()
	if err := fsAtomicWrite(abs, []byte(after), perm); err != nil {
		return Errorf(call, "cannot write %s: %v", rel, err), nil
	}

	diff := fsRenderHunks(rel, before, offsets, a.OldString, a.NewString)
	content := fmt.Sprintf("Edited %s: replaced %d occurrence%s (%s → %s).\n\n%s",
		rel, count, fsPlural(count), fsHumanBytes(int64(len(before))), fsHumanBytes(int64(len(after))), diff)
	return fsResult(call, content, EditData{
		Path:         rel,
		Replacements: count,
		BytesBefore:  len(before),
		BytesAfter:   len(after),
		MatchLines:   lines[:count],
		Diff:         diff,
	}), nil
}

// fsIndexAll returns the byte offsets of every non-overlapping occurrence of
// sub in s.
func fsIndexAll(s, sub string) []int {
	if sub == "" {
		return nil
	}
	var offsets []int
	for base := 0; ; {
		i := strings.Index(s[base:], sub)
		if i < 0 {
			return offsets
		}
		offsets = append(offsets, base+i)
		base += i + len(sub)
	}
}

// fsOffsetLines converts byte offsets into 1-based line numbers.
func fsOffsetLines(s string, offsets []int) []int {
	lines := make([]int, 0, len(offsets))
	for _, off := range offsets {
		lines = append(lines, 1+strings.Count(s[:off], "\n"))
	}
	return lines
}

// fsRenderHunks summarises the replacements as a compact diff.
func fsRenderHunks(rel, before string, offsets []int, oldText, newText string) string {
	var b strings.Builder
	shown := offsets
	if len(shown) > fsMaxEditHunks {
		shown = shown[:fsMaxEditHunks]
	}
	for i, off := range shown {
		if i > 0 {
			b.WriteString("\n")
		}
		line := 1 + strings.Count(before[:off], "\n")
		fmt.Fprintf(&b, "@@ %s:%d @@\n", rel, line)
		fsWriteHunkSide(&b, "-", oldText)
		fsWriteHunkSide(&b, "+", newText)
	}
	if len(offsets) > len(shown) {
		fmt.Fprintf(&b, "\n… and %d more identical replacement%s",
			len(offsets)-len(shown), fsPlural(len(offsets)-len(shown)))
	}
	return strings.TrimRight(b.String(), "\n")
}

// fsWriteHunkSide writes one side of a hunk, capped at fsMaxHunkLines.
func fsWriteHunkSide(b *strings.Builder, marker, text string) {
	if text == "" {
		fmt.Fprintf(b, "%s (nothing)\n", marker)
		return
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	for i, line := range lines {
		if i == fsMaxHunkLines {
			fmt.Fprintf(b, "%s … %d more line%s\n", marker, len(lines)-i, fsPlural(len(lines)-i))
			return
		}
		fmt.Fprintf(b, "%s %s\n", marker, line)
	}
}

// fsEditHint explains a near miss so the model can correct its next attempt
// instead of guessing.
func fsEditHint(content, old string) string {
	switch {
	case strings.Contains(strings.ReplaceAll(content, "\r\n", "\n"), strings.ReplaceAll(old, "\r\n", "\n")):
		return " The text matches once line endings are normalised: the file uses different line endings (CRLF vs LF)."
	case strings.Contains(fsSquashSpace(content), fsSquashSpace(old)) && strings.TrimSpace(old) != "":
		return " Similar text exists but the whitespace or indentation differs; read the file and copy the exact bytes."
	case strings.Contains(strings.ToLower(content), strings.ToLower(old)):
		return " Similar text exists but the letter case differs; matching is case-sensitive."
	default:
		return " Read the file first and copy the target text exactly, including indentation."
	}
}

// fsSquashSpace collapses runs of spaces and tabs so near-miss detection can
// ignore indentation differences.
func fsSquashSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if r == ' ' || r == '\t' {
			space = true
			continue
		}
		if space {
			if b.Len() > 0 && r != '\n' {
				b.WriteByte(' ')
			}
			space = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// fsJoinInts renders a list of line numbers for a message.
func fsJoinInts(values []int) string {
	const max = 10
	parts := make([]string, 0, min(len(values), max+1))
	for i, v := range values {
		if i == max {
			parts = append(parts, fmt.Sprintf("… and %d more", len(values)-i))
			break
		}
		parts = append(parts, strconv.Itoa(v))
	}
	return strings.Join(parts, ", ")
}
