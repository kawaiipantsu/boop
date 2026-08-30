package tools

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kawaiipantsu/boop/internal/permissions"
)

// Search limits.
const (
	fsDefaultSearchLimit = 100
	fsMaxSearchLimit     = 1000
	fsMaxContextLines    = 5
	fsMaxSearchLineChars = 500
	// fsMaxSearchFileBytes skips files too large to be worth grepping for a
	// model-facing answer.
	fsMaxSearchFileBytes = 8 << 20
)

// SearchTool searches file contents with a regular expression.
type SearchTool struct{ ws *Workspace }

// NewSearchTool returns a search tool confined to ws.
func NewSearchTool(ws *Workspace) *SearchTool { return &SearchTool{ws: ws} }

type searchArgs struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path"`
	Glob            string `json:"glob"`
	CaseInsensitive bool   `json:"case_insensitive"`
	Context         int    `json:"context"`
	Limit           int    `json:"limit"`
	IncludeIgnored  bool   `json:"include_ignored"`
}

// SearchMatch is one matching line.
type SearchMatch struct {
	// Path is relative to the project root.
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// SearchData is the structured payload of a search result.
type SearchData struct {
	Pattern      string        `json:"pattern"`
	Root         string        `json:"root"`
	Matches      []SearchMatch `json:"matches"`
	FilesMatched int           `json:"files_matched"`
	FilesScanned int           `json:"files_scanned"`
	FilesSkipped int           `json:"files_skipped"`
	Truncated    bool          `json:"truncated"`
}

// Name implements Tool.
func (t *SearchTool) Name() string { return "search" }

// Description implements Tool.
func (t *SearchTool) Description() string {
	return "Search file contents with a Go (RE2) regular expression and return matching lines " +
		"with their file and line number. Binary files and generated directories are skipped. " +
		"Use glob to restrict which files are searched and context to include surrounding lines."
}

// Schema implements Tool.
func (t *SearchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{
				"type":        "string",
				"description": "RE2 regular expression, e.g. \"func New[A-Z]\". Backreferences and lookaround are not supported.",
			},
			"path": map[string]any{
				"type":        "string",
				"default":     ".",
				"description": "File or directory to search, relative to the project root.",
			},
			"glob": map[string]any{
				"type":        "string",
				"description": "Optional filename glob restricting which files are searched, e.g. \"*.go\".",
			},
			"case_insensitive": map[string]any{
				"type":        "boolean",
				"default":     false,
				"description": "Match without regard to letter case.",
			},
			"context": map[string]any{
				"type":        "integer",
				"minimum":     0,
				"maximum":     fsMaxContextLines,
				"default":     0,
				"description": "Number of lines of context to show before and after each match.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"maximum":     fsMaxSearchLimit,
				"default":     fsDefaultSearchLimit,
				"description": "Maximum number of matching lines to return.",
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
func (t *SearchTool) Permission(call Call) (permissions.Action, error) {
	var a searchArgs
	if err := call.Bind(&a); err != nil {
		return permissions.Action{}, err
	}
	abs, display, contained := fsTarget(t.ws, fsOrDot(a.Path))
	summary := fmt.Sprintf("Search %s for %q", display, a.Pattern)
	if a.Glob != "" {
		summary = fmt.Sprintf("Search %s files in %s for %q", a.Glob, display, a.Pattern)
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
func (t *SearchTool) Execute(ctx context.Context, call Call) (Result, error) {
	var a searchArgs
	if err := call.Bind(&a); err != nil {
		return Errorf(call, "invalid arguments: %v", err), nil
	}
	if strings.TrimSpace(a.Pattern) == "" {
		return Errorf(call, "the %q argument is required", "pattern"), nil
	}
	expr := a.Pattern
	if a.CaseInsensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		// Recoverable: the model can rewrite the expression.
		return Errorf(call, "invalid regular expression %q: %v. Boop uses Go RE2 syntax, "+
			"which has no backreferences or lookaround.", a.Pattern, err), nil
	}
	if a.Glob != "" {
		if err := fsValidateGlob(a.Glob); err != nil {
			return Errorf(call, "invalid glob %q: %v", a.Glob, err), nil
		}
	}

	abs, err := t.ws.Resolve(fsOrDot(a.Path))
	if err != nil {
		return Errorf(call, "cannot search %q: %v", a.Path, err), nil
	}
	rel := fsOrDot(t.ws.Rel(abs))
	info, err := os.Stat(abs)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Errorf(call, "path not found: %s", rel), nil
	case err != nil:
		return Errorf(call, "cannot search %s: %v", rel, err), nil
	}

	limit := fsClamp(a.Limit, fsDefaultSearchLimit, fsMaxSearchLimit)
	contextLines := min(max(a.Context, 0), fsMaxContextLines)

	s := &fsSearcher{
		ws:      t.ws,
		re:      re,
		glob:    a.Glob,
		context: contextLines,
		limit:   limit,
	}

	if info.IsDir() {
		err = fsWalk(ctx, abs, a.IncludeIgnored, func(full string, entry os.DirEntry) error {
			if entry.IsDir() || !entry.Type().IsRegular() {
				return nil
			}
			return s.scanFile(ctx, full)
		})
	} else {
		err = s.scanFile(ctx, abs)
	}
	if err != nil && !errors.Is(err, fsErrStopWalk) {
		if ctx.Err() != nil {
			return Result{}, err
		}
		return Errorf(call, "cannot search %s: %v", rel, err), nil
	}

	data := SearchData{
		Pattern:      a.Pattern,
		Root:         rel,
		Matches:      s.matches,
		FilesMatched: s.filesMatched,
		FilesScanned: s.filesScanned,
		FilesSkipped: s.filesSkipped,
		Truncated:    s.truncated,
	}
	if len(s.matches) == 0 {
		return fsResult(call, fmt.Sprintf("No matches for %q in %s (%d file%s searched, %d skipped as binary or oversized).",
			a.Pattern, rel, s.filesScanned, fsPlural(s.filesScanned), s.filesSkipped), data), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d match%s in %d file%s for %q\n\n",
		len(s.matches), map[bool]string{true: "", false: "es"}[len(s.matches) == 1],
		s.filesMatched, fsPlural(s.filesMatched), a.Pattern)
	b.WriteString(s.rendered.String())
	if s.truncated {
		fmt.Fprintf(&b, "\n[truncated at %d matches; refine the pattern or raise limit]", limit)
	}
	return fsResult(call, strings.TrimRight(b.String(), "\n"), data), nil
}

// fsSearcher accumulates bounded regex matches across files.
type fsSearcher struct {
	ws      *Workspace
	re      *regexp.Regexp
	glob    string
	context int
	limit   int

	matches      []SearchMatch
	rendered     strings.Builder
	filesScanned int
	filesMatched int
	filesSkipped int
	truncated    bool
}

// scanFile searches one file, streaming so a large file never has to be held
// in memory in full.
func (s *fsSearcher) scanFile(ctx context.Context, full string) error {
	if s.truncated {
		return fsErrStopWalk
	}
	rel := s.ws.Rel(full)
	if s.glob != "" && !fsGlobMatch(s.glob, filepath.ToSlash(rel)) {
		return nil
	}
	info, err := os.Stat(full)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	if info.Size() > fsMaxSearchFileBytes {
		s.filesSkipped++
		return nil
	}
	f, err := os.Open(full)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			s.filesSkipped++
			return nil
		}
		return err
	}
	defer func() { _ = f.Close() }()

	sniff := make([]byte, fsSniffBytes)
	n, err := io.ReadFull(f, sniff)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		s.filesSkipped++
		return nil
	}
	if fsIsBinary(sniff[:n]) {
		s.filesSkipped++
		return nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	s.filesScanned++

	var (
		br      = bufio.NewReaderSize(f, 64<<10)
		lineNo  int
		before  = make([]string, 0, s.context)
		pending int // remaining trailing context lines to emit
		matched bool
		lastOut int
	)
	for {
		line, readErr := br.ReadString('\n')
		if len(line) > 0 {
			lineNo++
			if lineNo%fsCancelInterval == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			text := fsTrimLine(line)
			switch {
			case s.re.MatchString(text):
				if len(s.matches) >= s.limit {
					s.truncated = true
					return fsErrStopWalk
				}
				if !matched {
					matched = true
					s.filesMatched++
				}
				for i, ctxLine := range before {
					n := lineNo - len(before) + i
					if n > lastOut {
						fmt.Fprintf(&s.rendered, "%s:%d-%s\n", rel, n, ctxLine)
						lastOut = n
					}
				}
				before = before[:0]
				fmt.Fprintf(&s.rendered, "%s:%d:%s\n", rel, lineNo, text)
				lastOut = lineNo
				s.matches = append(s.matches, SearchMatch{Path: rel, Line: lineNo, Text: text})
				pending = s.context
			case pending > 0:
				fmt.Fprintf(&s.rendered, "%s:%d-%s\n", rel, lineNo, text)
				lastOut = lineNo
				pending--
			case s.context > 0:
				if len(before) == s.context {
					before = before[1:]
				}
				before = append(before, text)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return readErr
		}
		if s.rendered.Len() > fsMaxOutputBytes {
			s.truncated = true
			return fsErrStopWalk
		}
	}
	if matched && s.context > 0 {
		s.rendered.WriteString("\n")
	}
	return nil
}

// fsTrimLine strips the line terminator and caps very long lines, which are
// usually minified assets rather than something a model wants in full.
func fsTrimLine(line string) string {
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if len(line) > fsMaxSearchLineChars {
		return line[:fsMaxSearchLineChars] + "… (line truncated)"
	}
	return line
}
