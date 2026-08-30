package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/kawaiipantsu/boop/internal/permissions"
)

// ApplyPatchTool applies a unified diff patch to one or more files in the workspace.
type ApplyPatchTool struct {
	ws *Workspace
}

// NewApplyPatchTool returns an apply_patch tool confined to ws.
func NewApplyPatchTool(ws *Workspace) *ApplyPatchTool {
	return &ApplyPatchTool{ws: ws}
}

type applyPatchArgs struct {
	Path  string `json:"path,omitempty"`
	Patch string `json:"patch"`
}

// ApplyPatchData is the structured result payload of an apply_patch call.
type ApplyPatchData struct {
	FilesPatched []string `json:"files_patched"`
	HunksApplied int      `json:"hunks_applied"`
	Summary      string   `json:"summary"`
}

// Name implements Tool.
func (t *ApplyPatchTool) Name() string { return "apply_patch" }

// Description implements Tool.
func (t *ApplyPatchTool) Description() string {
	return "Apply a unified diff patch to modify existing files in the workspace. " +
		"The patch must follow standard unified diff format (with @@ -old,count +new,count @@ hunk headers). " +
		"Hunks are matched using surrounding context lines."
}

// Schema implements Tool.
func (t *ApplyPatchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Optional file path if not specified in the patch header (--- a/file +++ b/file).",
			},
			"patch": map[string]any{
				"type":        "string",
				"description": "Unified diff content to apply.",
			},
		},
		"required":             []string{"patch"},
		"additionalProperties": false,
	}
}

// Permission implements Tool.
func (t *ApplyPatchTool) Permission(call Call) (permissions.Action, error) {
	var a applyPatchArgs
	if err := call.Bind(&a); err != nil {
		return permissions.Action{}, err
	}
	patches, err := parseUnifiedDiff(a.Patch, a.Path)
	if err != nil {
		return permissions.Action{}, fmt.Errorf("invalid patch format: %w", err)
	}

	paths := make([]string, 0, len(patches))
	var summaries []string
	for _, p := range patches {
		abs, display, contained := fsTarget(t.ws, p.TargetFile)
		paths = append(paths, abs)
		if !contained {
			return permissions.Action{}, fmt.Errorf("path %q escapes the workspace", p.TargetFile)
		}
		summaries = append(summaries, fmt.Sprintf("patch %s (%d hunks)", display, len(p.Hunks)))
	}

	return permissions.Action{
		Category: permissions.CatFilesystemWrite,
		Risk:     permissions.RiskMedium,
		Tool:     t.Name(),
		Summary:  strings.Join(summaries, ", "),
		Detail:   a.Patch,
		Paths:    paths,
	}, nil
}

// Execute parses and applies the patch to files in the workspace.
func (t *ApplyPatchTool) Execute(ctx context.Context, call Call) (Result, error) {
	var a applyPatchArgs
	if err := call.Bind(&a); err != nil {
		return Errorf(call, "apply_patch: %v", err), nil
	}
	if strings.TrimSpace(a.Patch) == "" {
		return Errorf(call, "apply_patch: patch must not be empty"), nil
	}

	filePatches, err := parseUnifiedDiff(a.Patch, a.Path)
	if err != nil {
		return Errorf(call, "apply_patch: invalid diff: %v", err), nil
	}
	if len(filePatches) == 0 {
		return Errorf(call, "apply_patch: no valid file patches found in diff"), nil
	}

	var patchedFiles []string
	totalHunks := 0

	for _, fp := range filePatches {
		abs, display, contained := fsTarget(t.ws, fp.TargetFile)
		if !contained {
			return Errorf(call, "apply_patch: path %q escapes the workspace", fp.TargetFile), nil
		}

		origBytes, err := os.ReadFile(abs)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return Errorf(call, "apply_patch: file %q does not exist", display), nil
			}
			return Errorf(call, "apply_patch: cannot read %s: %v", display, err), nil
		}

		patchedContent, hunksApplied, err := applyFilePatch(string(origBytes), fp)
		if err != nil {
			return Errorf(call, "apply_patch on %s: %v", display, err), nil
		}

		info, _ := os.Stat(abs)
		perm := os.FileMode(0644)
		if info != nil {
			perm = info.Mode().Perm()
		}

		if err := os.WriteFile(abs, []byte(patchedContent), perm); err != nil {
			return Errorf(call, "apply_patch: cannot write %s: %v", display, err), nil
		}

		patchedFiles = append(patchedFiles, display)
		totalHunks += hunksApplied
	}

	summary := fmt.Sprintf("Applied %d hunk(s) across %d file(s): %s", totalHunks, len(patchedFiles), strings.Join(patchedFiles, ", "))
	return Result{
		CallID:  call.ID,
		Tool:    t.Name(),
		Content: summary,
		Data: ApplyPatchData{
			FilesPatched: patchedFiles,
			HunksApplied: totalHunks,
			Summary:      summary,
		},
		Display: fmt.Sprintf("%d hunk(s) applied", totalHunks),
	}, nil
}

type filePatch struct {
	TargetFile string
	Hunks      []diffHunk
}

type diffHunk struct {
	OldStart int
	OldCount int
	NewStart int
	NewCount int
	Lines    []string
}

func parseUnifiedDiff(raw, defaultPath string) ([]filePatch, error) {
	lines := strings.Split(raw, "\n")
	var patches []filePatch
	var cur *filePatch
	var curHunk *diffHunk

	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		if strings.HasPrefix(line, "--- ") {
			if curHunk != nil && cur != nil {
				cur.Hunks = append(cur.Hunks, *curHunk)
				curHunk = nil
			}
			if cur != nil && len(cur.Hunks) > 0 {
				patches = append(patches, *cur)
			}
			cur = &filePatch{}
			continue
		}
		if strings.HasPrefix(line, "+++ ") {
			target := strings.TrimPrefix(line, "+++ ")
			target = strings.TrimSpace(target)
			target = strings.TrimPrefix(target, "b/")
			target = strings.TrimPrefix(target, "a/")
			if cur == nil {
				cur = &filePatch{}
			}
			cur.TargetFile = target
			continue
		}
		if strings.HasPrefix(line, "@@ ") {
			if cur == nil {
				if defaultPath == "" {
					return nil, errors.New("hunk found without preceding +++ file header or path argument")
				}
				cur = &filePatch{TargetFile: defaultPath}
			}
			if curHunk != nil {
				cur.Hunks = append(cur.Hunks, *curHunk)
			}
			hunk, err := parseHunkHeader(line)
			if err != nil {
				return nil, err
			}
			curHunk = hunk
			continue
		}
		if curHunk != nil {
			if len(line) > 0 && (line[0] == ' ' || line[0] == '+' || line[0] == '-') {
				curHunk.Lines = append(curHunk.Lines, line)
			} else if line == "" {
				curHunk.Lines = append(curHunk.Lines, " ")
			}
		}
	}

	if curHunk != nil && cur != nil {
		cur.Hunks = append(cur.Hunks, *curHunk)
	}
	if cur != nil && len(cur.Hunks) > 0 {
		if cur.TargetFile == "" && defaultPath != "" {
			cur.TargetFile = defaultPath
		}
		patches = append(patches, *cur)
	}

	if len(patches) == 0 && defaultPath != "" && curHunk != nil {
		patches = append(patches, filePatch{
			TargetFile: defaultPath,
			Hunks:      []diffHunk{*curHunk},
		})
	}

	return patches, nil
}

func parseHunkHeader(header string) (*diffHunk, error) {
	// Format: @@ -oldStart,oldCount +newStart,newCount @@
	parts := strings.Split(header, "@@")
	if len(parts) < 3 {
		return nil, fmt.Errorf("malformed hunk header %q", header)
	}
	coords := strings.Fields(strings.TrimSpace(parts[1]))
	if len(coords) < 2 {
		return nil, fmt.Errorf("malformed hunk coordinates %q", header)
	}

	oldPart := strings.TrimPrefix(coords[0], "-")
	newPart := strings.TrimPrefix(coords[1], "+")

	oldStart, oldCount := parseCoordPair(oldPart)
	newStart, newCount := parseCoordPair(newPart)

	return &diffHunk{
		OldStart: oldStart,
		OldCount: oldCount,
		NewStart: newStart,
		NewCount: newCount,
		Lines:    []string{},
	}, nil
}

func parseCoordPair(s string) (int, int) {
	parts := strings.Split(s, ",")
	start, _ := strconv.Atoi(parts[0])
	count := 1
	if len(parts) > 1 {
		count, _ = strconv.Atoi(parts[1])
	}
	return start, count
}

func applyFilePatch(content string, fp filePatch) (string, int, error) {
	fileLines := strings.Split(content, "\n")
	// Normalize CRLF to LF
	for i := range fileLines {
		fileLines[i] = strings.TrimRight(fileLines[i], "\r")
	}

	appliedCount := 0
	lineOffset := 0

	for hunkIdx, hunk := range fp.Hunks {
		var oldHunkLines []string
		var newHunkLines []string

		for _, hl := range hunk.Lines {
			if len(hl) == 0 {
				continue
			}
			tag := hl[0]
			text := hl[1:]
			if tag == ' ' {
				oldHunkLines = append(oldHunkLines, text)
				newHunkLines = append(newHunkLines, text)
			} else if tag == '-' {
				oldHunkLines = append(oldHunkLines, text)
			} else if tag == '+' {
				newHunkLines = append(newHunkLines, text)
			}
		}

		// Locate the hunk in fileLines. First try the expected line number.
		targetIdx := -1
		expectedIdx := hunk.OldStart - 1 + lineOffset
		if expectedIdx >= 0 && expectedIdx < len(fileLines) && matchHunkAt(fileLines, expectedIdx, oldHunkLines) {
			targetIdx = expectedIdx
		} else {
			// Scan whole file for unique context match
			for i := 0; i <= len(fileLines)-len(oldHunkLines); i++ {
				if matchHunkAt(fileLines, i, oldHunkLines) {
					if targetIdx != -1 {
						return "", 0, fmt.Errorf("hunk %d has ambiguous context match in file", hunkIdx+1)
					}
					targetIdx = i
				}
			}
		}

		if targetIdx == -1 {
			return "", 0, fmt.Errorf("hunk %d context does not match file contents", hunkIdx+1)
		}

		// Replace lines in fileLines
		fileLines = append(fileLines[:targetIdx], append(newHunkLines, fileLines[targetIdx+len(oldHunkLines):]...)...)
		lineOffset += len(newHunkLines) - len(oldHunkLines)
		appliedCount++
	}

	return strings.Join(fileLines, "\n"), appliedCount, nil
}

func matchHunkAt(fileLines []string, start int, expected []string) bool {
	if start+len(expected) > len(fileLines) {
		return false
	}
	for i := 0; i < len(expected); i++ {
		if strings.TrimSpace(fileLines[start+i]) != strings.TrimSpace(expected[i]) {
			return false
		}
	}
	return true
}
