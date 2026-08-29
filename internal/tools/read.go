package tools

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/kawaiipantsu/boop/internal/permissions"
)

// Byte budgets shared by the filesystem tools.
//
// The output cap exists because tool results are fed back into a model context
// window: an unbounded file would evict the conversation that asked for it.
// The scan cap bounds work done on disk for pathological inputs.
const (
	fsMaxOutputBytes = 256 << 10 // returned to the model
	fsMaxScanBytes   = 64 << 20  // read from disk before giving up
	fsSniffBytes     = 8 << 10   // inspected for binary detection
	fsCancelInterval = 512       // lines/entries between ctx checks
)

// ReadTool returns the contents of a text file inside the workspace.
type ReadTool struct{ ws *Workspace }

// NewReadTool returns a read tool confined to ws.
func NewReadTool(ws *Workspace) *ReadTool { return &ReadTool{ws: ws} }

type readArgs struct {
	Path        string `json:"path"`
	Offset      int    `json:"offset"`
	Limit       int    `json:"limit"`
	LineNumbers bool   `json:"line_numbers"`
}

// ReadData is the structured payload of a read result, for UIs and persistence.
type ReadData struct {
	Path       string `json:"path"`
	Bytes      int64  `json:"bytes"`
	TotalLines int    `json:"total_lines"`
	FirstLine  int    `json:"first_line"`
	LastLine   int    `json:"last_line"`
	Truncated  bool   `json:"truncated"`
	Binary     bool   `json:"binary"`
	MediaType  string `json:"media_type,omitempty"`
}

// Name implements Tool.
func (t *ReadTool) Name() string { return "read" }

// Description implements Tool.
func (t *ReadTool) Description() string {
	return "Read a UTF-8 text file from the project. Supports reading a line range of a large " +
		"file via offset and limit. Binary files are reported rather than dumped, and output is " +
		"truncated once it exceeds the size cap."
}

// Schema implements Tool.
func (t *ReadTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "File path relative to the project root.",
			},
			"offset": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"description": "1-based line number to start reading from. Defaults to the first line.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"description": "Maximum number of lines to return. Defaults to the whole file, subject to the output cap.",
			},
			"line_numbers": map[string]any{
				"type":        "boolean",
				"default":     false,
				"description": "Prefix each returned line with its line number.",
			},
		},
		"required":             []string{"path"},
		"additionalProperties": false,
	}
}

// Permission implements Tool. Reading is low risk, raised for paths that
// commonly hold credentials so the user is asked before secrets are pulled
// into a model context (PROJECT.md §48).
func (t *ReadTool) Permission(call Call) (permissions.Action, error) {
	var a readArgs
	if err := call.Bind(&a); err != nil {
		return permissions.Action{}, err
	}
	abs, display, contained := fsTarget(t.ws, a.Path)
	what := fmt.Sprintf("Read %s", display)
	if a.Offset > 0 || a.Limit > 0 {
		what = fmt.Sprintf("Read %s from %s", fsLineRange(a.Offset, a.Limit), display)
	}
	return permissions.Action{
		Category: permissions.CatFilesystemRead,
		Risk:     fsReadRisk(display, contained),
		Tool:     t.Name(),
		Summary:  what,
		Detail:   fsTargetDetail(abs, contained),
		Paths:    []string{abs},
	}, nil
}

// Execute implements Tool.
func (t *ReadTool) Execute(ctx context.Context, call Call) (Result, error) {
	var a readArgs
	if err := call.Bind(&a); err != nil {
		return Errorf(call, "invalid arguments: %v", err), nil
	}
	if strings.TrimSpace(a.Path) == "" {
		return Errorf(call, "the %q argument is required", "path"), nil
	}
	abs, err := t.ws.Resolve(a.Path)
	if err != nil {
		return Errorf(call, "cannot read %q: %v", a.Path, err), nil
	}
	rel := t.ws.Rel(abs)

	info, err := os.Stat(abs)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Errorf(call, "file not found: %s", rel), nil
	case err != nil:
		return Errorf(call, "cannot read %s: %v", rel, err), nil
	case info.IsDir():
		return Errorf(call, "%s is a directory; use the list tool to see its contents", rel), nil
	}

	f, err := os.Open(abs)
	if err != nil {
		return Errorf(call, "cannot open %s: %v", rel, err), nil
	}
	defer f.Close()

	sniff := make([]byte, fsSniffBytes)
	n, err := io.ReadFull(f, sniff)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return Errorf(call, "cannot read %s: %v", rel, err), nil
	}
	sniff = sniff[:n]

	if info.Size() == 0 {
		return fsResult(call, fmt.Sprintf("%s is empty (0 bytes).", rel),
			ReadData{Path: rel, Bytes: 0}), nil
	}
	if fsIsBinary(sniff) {
		media := http.DetectContentType(sniff)
		// Not an error: the tool answered the question, and there is nothing
		// here for the model to repair by retrying.
		content := fmt.Sprintf(
			"%s is a binary file (%s, detected as %s) and cannot be shown as text.",
			rel, fsHumanBytes(info.Size()), media)
		return fsResult(call, content, ReadData{
			Path: rel, Bytes: info.Size(), Binary: true, MediaType: media,
		}), nil
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Errorf(call, "cannot read %s: %v", rel, err), nil
	}

	start := a.Offset
	if start < 1 {
		start = 1
	}
	body, stats, err := fsReadLines(ctx, f, start, a.Limit, a.LineNumbers)
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, err
		}
		return Errorf(call, "cannot read %s: %v", rel, err), nil
	}
	if stats.collected == 0 && start > 1 {
		return Errorf(call, "offset %d is past the end of %s, which has %d line(s)",
			start, rel, stats.total), nil
	}

	data := ReadData{
		Path:       rel,
		Bytes:      info.Size(),
		TotalLines: stats.total,
		FirstLine:  stats.first,
		LastLine:   stats.last,
		Truncated:  stats.truncated,
	}
	var note string
	switch {
	case stats.truncated:
		note = fmt.Sprintf("\n[truncated: showed lines %d-%d of %d; output capped at %s. "+
			"Read on with offset=%d]", stats.first, stats.last, stats.total,
			fsHumanBytes(fsMaxOutputBytes), stats.last+1)
	case stats.last > 0 && stats.last < stats.total:
		note = fmt.Sprintf("\n[showed lines %d-%d of %d; read on with offset=%d]",
			stats.first, stats.last, stats.total, stats.last+1)
	}
	return fsResult(call, body+note, data), nil
}

// fsLineStats summarises what fsReadLines managed to collect.
type fsLineStats struct {
	total     int
	first     int
	last      int
	collected int
	truncated bool
}

// fsReadLines renders the [start, start+limit) line window of r, stopping once
// the output cap is reached. It keeps counting lines afterwards so the caller
// can tell the model how much of the file it has not seen.
func fsReadLines(ctx context.Context, r io.Reader, start, limit int, numbers bool) (string, fsLineStats, error) {
	var (
		out     strings.Builder
		stats   fsLineStats
		br      = bufio.NewReaderSize(r, 64<<10)
		scanned int64
		done    bool
	)
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			stats.total++
			scanned += int64(len(line))
			if stats.total%fsCancelInterval == 0 {
				if cerr := ctx.Err(); cerr != nil {
					return "", stats, cerr
				}
			}
			if !done && stats.total >= start && (limit <= 0 || stats.collected < limit) {
				text := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
				var rendered string
				if numbers {
					rendered = fmt.Sprintf("%6d\t%s\n", stats.total, text)
				} else {
					rendered = text + "\n"
				}
				if out.Len()+len(rendered) > fsMaxOutputBytes {
					stats.truncated = true
					done = true
				} else {
					out.WriteString(rendered)
					stats.collected++
					if stats.first == 0 {
						stats.first = stats.total
					}
					stats.last = stats.total
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", stats, err
		}
		if scanned > fsMaxScanBytes {
			stats.truncated = true
			break
		}
	}
	return out.String(), stats, nil
}

// fsIsBinary reports whether sample looks like binary rather than text.
//
// A NUL byte is decisive; otherwise the heuristic is the density of control
// characters plus UTF-8 validity, which keeps UTF-8 source with unusual
// punctuation on the text side while catching compiled objects and images.
func fsIsBinary(sample []byte) bool {
	if len(sample) == 0 {
		return false
	}
	var control int
	for _, b := range sample {
		if b == 0 {
			return true
		}
		if b < 0x20 && b != '\t' && b != '\n' && b != '\r' && b != '\f' && b != '\v' && b != 0x1b {
			control++
		}
	}
	if control*100 > len(sample)*30 {
		return true
	}
	// Drop a rune that the sample boundary may have cut in half before
	// judging encoding validity.
	trimmed := sample
	for i := 0; i < utf8.UTFMax && len(trimmed) > 0; i++ {
		if utf8.Valid(trimmed) {
			return false
		}
		trimmed = trimmed[:len(trimmed)-1]
	}
	return !utf8.Valid(trimmed)
}

// fsResult builds a successful Result carrying a structured payload.
func fsResult(call Call, content string, data any) Result {
	return Result{CallID: call.ID, Tool: call.Name, Content: content, Data: data}
}

// fsFailure builds a failed Result that still carries a structured payload, so
// a UI can render the attempt that did not work.
func fsFailure(call Call, data any, format string, args ...any) Result {
	r := Errorf(call, format, args...)
	r.Data = data
	return r
}

// fsTarget resolves path for permission classification. It never fails: a path
// that escapes the workspace is reported as such so the approval UI can show
// what was asked for, and Execute rejects it independently.
func fsTarget(ws *Workspace, path string) (abs, display string, contained bool) {
	if strings.TrimSpace(path) == "" {
		return ws.Root(), ws.Rel(ws.Root()), true
	}
	resolved, err := ws.Resolve(path)
	if err != nil {
		return path, path, false
	}
	return resolved, ws.Rel(resolved), true
}

// fsTargetDetail describes the target for an approval dialog.
func fsTargetDetail(abs string, contained bool) string {
	if !contained {
		return "REJECTED: " + abs + " lies outside the project workspace"
	}
	return abs
}

// fsReadRisk grades a read of display.
func fsReadRisk(display string, contained bool) permissions.Risk {
	switch {
	case !contained:
		return permissions.RiskCritical
	case fsSensitive(display):
		return permissions.RiskMedium
	default:
		return permissions.RiskLow
	}
}

// fsWriteRisk grades a mutation of display. Replacing existing content is
// worse than creating something new, because the previous version is gone.
func fsWriteRisk(display string, contained, destructive bool) permissions.Risk {
	switch {
	case !contained:
		return permissions.RiskCritical
	case fsSensitive(display):
		return permissions.RiskHigh
	case destructive:
		return permissions.RiskHigh
	default:
		return permissions.RiskMedium
	}
}

// fsSensitivePatterns are the filename shapes from PROJECT.md §48 that usually
// hold credentials. They raise risk; they never silently block.
var fsSensitivePatterns = []string{
	".env", ".env.*", "*.pem", "*.key", "*.p12", "*.pfx",
	"credentials*", "secrets*", "secret", "id_rsa*", "id_ed25519*", "*.keystore",
}

// fsSensitive reports whether path looks like a secret-bearing file.
func fsSensitive(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	for _, pat := range fsSensitivePatterns {
		if ok, err := filepath.Match(pat, base); err == nil && ok {
			return true
		}
	}
	return false
}

// fsLineRange renders an offset/limit pair for an approval summary.
func fsLineRange(offset, limit int) string {
	if offset < 1 {
		offset = 1
	}
	if limit <= 0 {
		return fmt.Sprintf("lines %d onwards", offset)
	}
	return fmt.Sprintf("lines %d-%d", offset, offset+limit-1)
}

// fsHumanBytes renders a byte count for humans, e.g. "2.1 KB".
func fsHumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 3; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

// fsPlural returns "" for one and "s" otherwise.
func fsPlural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
