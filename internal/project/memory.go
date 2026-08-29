package project

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Limits on the Session Summaries section. Boop.md is compressed durable
// knowledge, not a transcript (§16): raw session data belongs in SQLite, so the
// newest entries are kept in full and older ones collapse to one line each.
const (
	// DefaultMaxSessionSummaries is how many summaries are kept verbatim.
	DefaultMaxSessionSummaries = 20
	// DefaultMaxArchivedSummaries is how many compressed one-liners are kept.
	DefaultMaxArchivedSummaries = 100
)

// archiveHeading titles the compressed block of older session summaries.
const archiveHeading = "Earlier Sessions (compressed)"

// summaryTimeLayout is the timestamp format used inside Boop.md. It is short,
// sortable and human-readable; precision beyond the minute belongs in the
// session database.
const summaryTimeLayout = "2006-01-02 15:04"

// memoryNames are the file names accepted as project memory, in preference
// order. Boop writes MemoryFileName but honours an existing casing variant
// rather than creating a second file next to it.
var memoryNames = []string{MemoryFileName, "boop.md", "BOOP.md"}

// SessionSummary is one compressed session entry.
type SessionSummary struct {
	// Time is when the session ended. Zero means "now" at append time.
	Time time.Time
	// Title is a one-line description of what the session achieved.
	Title string
	// Bullets are the durable details worth keeping.
	Bullets []string
}

// Memory is the read/modify/write interface to a project's Boop.md.
//
// Mutations happen in memory; Save writes the file atomically. Every string
// that reaches the document passes through Redact first: Boop.md is committed
// to repositories and must never carry credentials (§45).
type Memory struct {
	// MaxSessionSummaries caps verbatim summaries; 0 uses the default.
	MaxSessionSummaries int
	// MaxArchivedSummaries caps compressed summaries; 0 uses the default.
	MaxArchivedSummaries int
	// Now supplies timestamps; nil uses time.Now.
	Now func() time.Time

	path    string
	doc     *Document
	created bool
	mode    fs.FileMode
}

// LoadOrCreate opens the Boop.md in dir, or prepares a new one in memory when
// the file does not exist. Nothing is written until Save is called.
func LoadOrCreate(dir string) (*Memory, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", dir, err)
	}
	for _, name := range memoryNames {
		p := filepath.Join(abs, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return OpenMemory(p)
		}
	}
	return &Memory{
		path:    filepath.Join(abs, MemoryFileName),
		doc:     NewDocument(),
		created: true,
		mode:    0o644,
	}, nil
}

// OpenMemory loads a specific memory file. A missing file is not an error: the
// returned Memory holds a fresh document that Save will create.
func OpenMemory(path string) (*Memory, error) {
	m := &Memory{path: path, mode: 0o644}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		m.doc = Parse(data)
		if st, statErr := os.Stat(path); statErr == nil {
			m.mode = st.Mode().Perm()
		}
	case os.IsNotExist(err):
		m.doc = NewDocument()
		m.created = true
	default:
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	return m, nil
}

// Path returns the file the memory is stored in.
func (m *Memory) Path() string { return m.path }

// Created reports that the file did not exist when the memory was loaded.
func (m *Memory) Created() bool { return m.created }

// Document exposes the parsed document for structured access.
func (m *Memory) Document() *Document { return m.doc }

// Render returns the document bytes that Save would write.
func (m *Memory) Render() []byte { return m.doc.Render() }

func (m *Memory) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Memory) maxSummaries() int {
	if m.MaxSessionSummaries > 0 {
		return m.MaxSessionSummaries
	}
	return DefaultMaxSessionSummaries
}

func (m *Memory) maxArchived() int {
	if m.MaxArchivedSummaries > 0 {
		return m.MaxArchivedSummaries
	}
	return DefaultMaxArchivedSummaries
}

// Save writes the document atomically: a temporary file in the same directory
// is written, synced and renamed over the target, so an interrupted write can
// never truncate the user's project memory.
func (m *Memory) Save() error {
	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %q: %w", dir, err)
	}
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Errorf("temp name: %w", err)
	}
	tmp := filepath.Join(dir, "."+filepath.Base(m.path)+".tmp-"+hex.EncodeToString(suffix[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, m.mode)
	if err != nil {
		return fmt.Errorf("create %q: %w", tmp, err)
	}
	if _, err := f.Write(m.doc.Render()); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write %q: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("sync %q: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, m.path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace %q: %w", m.path, err)
	}
	m.created = false
	return nil
}

// AppendDecision records a durable decision as a dated bullet under Decisions.
func (m *Memory) AppendDecision(text string) {
	text = collapse(Redact(text))
	if text == "" {
		return
	}
	m.doc.AppendText(SectionDecisions, fmt.Sprintf("- %s — %s", m.now().Format("2006-01-02"), text))
}

// RecordKnownProblem adds a problem under Known Problems, ignoring duplicates
// so a repeated failure does not grow the file without adding information.
func (m *Memory) RecordKnownProblem(text string) {
	text = collapse(Redact(text))
	if text == "" {
		return
	}
	bullet := "- " + text
	for _, line := range strings.Split(m.doc.Text(SectionKnownProblems), "\n") {
		if strings.TrimSpace(line) == bullet {
			return
		}
	}
	m.doc.AppendText(SectionKnownProblems, bullet)
}

// SetCurrentWork replaces the Current Work section. It is state, not history:
// what was previously in progress belongs in a session summary.
func (m *Memory) SetCurrentWork(text string) {
	m.doc.SetText(SectionCurrentWork, Redact(text))
}

// AppendSessionSummary appends a summary and then trims the section back
// within its bounds.
func (m *Memory) AppendSessionSummary(s SessionSummary) {
	if s.Time.IsZero() {
		s.Time = m.now()
	}
	s.Title = collapse(Redact(s.Title))
	if s.Title == "" {
		s.Title = "Session"
	}
	clean := make([]string, 0, len(s.Bullets))
	for _, b := range s.Bullets {
		if b = collapse(Redact(b)); b != "" {
			clean = append(clean, b)
		}
	}
	s.Bullets = clean

	entries, archived := m.parseSummaries()
	entries = append(entries, s)
	m.writeSummaries(entries, archived)
}

// Summaries returns the verbatim session summaries, oldest first.
func (m *Memory) Summaries() []SessionSummary {
	entries, _ := m.parseSummaries()
	return entries
}

// ArchivedSummaries returns the compressed one-line summaries, oldest first.
func (m *Memory) ArchivedSummaries() []string {
	_, archived := m.parseSummaries()
	return archived
}

// parseSummaries splits the Session Summaries section into full entries and
// the compressed archive. Text that does not look like an entry is discarded
// only from this section's generated shape; Boop owns its layout, while the
// rest of the document is never reshaped.
func (m *Memory) parseSummaries() (entries []SessionSummary, archived []string) {
	body := m.doc.Text(SectionSessionSummaries)
	if body == "" {
		return nil, nil
	}
	var cur *SessionSummary
	inArchive := false
	flush := func() {
		if cur != nil {
			entries = append(entries, *cur)
			cur = nil
		}
	}
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if heading, ok := strings.CutPrefix(trimmed, "### "); ok {
			flush()
			heading = strings.TrimSpace(heading)
			if strings.EqualFold(heading, archiveHeading) {
				inArchive = true
				continue
			}
			inArchive = false
			ts, title := splitSummaryHeading(heading)
			cur = &SessionSummary{Time: ts, Title: title}
			continue
		}
		if trimmed == "" {
			continue
		}
		switch {
		case inArchive:
			archived = append(archived, strings.TrimPrefix(trimmed, "- "))
		case cur != nil:
			cur.Bullets = append(cur.Bullets, strings.TrimPrefix(trimmed, "- "))
		}
	}
	flush()
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Time.Before(entries[j].Time) })
	return entries, archived
}

// splitSummaryHeading parses "2026-08-29 14:04 — Title".
func splitSummaryHeading(h string) (time.Time, string) {
	for _, sep := range []string{" — ", " - ", " – "} {
		if stamp, title, ok := strings.Cut(h, sep); ok {
			if ts, err := time.Parse(summaryTimeLayout, strings.TrimSpace(stamp)); err == nil {
				return ts, strings.TrimSpace(title)
			}
		}
	}
	return time.Time{}, h
}

// writeSummaries renders entries back into the section, compressing everything
// beyond the verbatim cap into the archive and dropping the oldest archive
// lines once that too is full.
func (m *Memory) writeSummaries(entries []SessionSummary, archived []string) {
	if over := len(entries) - m.maxSummaries(); over > 0 {
		for _, e := range entries[:over] {
			archived = append(archived, e.oneLine())
		}
		entries = entries[over:]
	}
	if over := len(archived) - m.maxArchived(); over > 0 {
		archived = archived[over:]
	}

	var b strings.Builder
	if len(archived) > 0 {
		b.WriteString("### " + archiveHeading + "\n\n")
		for _, a := range archived {
			b.WriteString("- " + a + "\n")
		}
	}
	for _, e := range entries {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("### " + e.headingText() + "\n\n")
		for _, bullet := range e.Bullets {
			b.WriteString("- " + bullet + "\n")
		}
	}
	m.doc.SetText(SectionSessionSummaries, b.String())
}

func (s SessionSummary) headingText() string {
	if s.Time.IsZero() {
		return s.Title
	}
	return s.Time.Format(summaryTimeLayout) + " — " + s.Title
}

// oneLine compresses a summary to a single archive bullet: the headline
// survives, the detail does not.
func (s SessionSummary) oneLine() string {
	if s.Time.IsZero() {
		return s.Title
	}
	return s.Time.Format("2006-01-02") + " — " + s.Title
}

// ---------------------------------------------------------------------------
// redaction

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),                    // OpenAI / Anthropic style
	regexp.MustCompile(`(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}`), // GitHub tokens
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),             // GitHub fine-grained PAT
	regexp.MustCompile(`xox[abposr]-[A-Za-z0-9-]{10,}`),            // Slack
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),                         // AWS access key id
	regexp.MustCompile(`AIza[0-9A-Za-z_-]{20,}`),                   // Google API key
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/-]{16,}=*`),    // Authorization headers
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
}

// assignedSecretRe matches "key = value" shapes whose key names a credential.
var assignedSecretRe = regexp.MustCompile(`(?i)\b([a-z0-9_.-]*(?:api[_-]?key|secret|token|password|passwd|access[_-]?key|private[_-]?key))\b(\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;]+)`)

// envReferenceRe recognises the forms §45 explicitly endorses: a reference to
// an environment variable is not a secret and must survive redaction, or Boop
// would mangle its own documented configuration examples.
var envReferenceRe = regexp.MustCompile(`^(?:"|')?(?:\$\{?[A-Za-z_][A-Za-z0-9_]*\}?|[A-Z][A-Z0-9_]{2,}|<[^>]*>|\.\.\.|)(?:"|')?$`)

// RedactedPlaceholder replaces any value that looks like a credential.
const RedactedPlaceholder = "[REDACTED]"

// Redact removes anything that looks like a credential from text.
//
// It is deliberately eager: a false positive costs a mangled note, a false
// negative commits an API key to a file that lives in the repository (§45).
func Redact(text string) string {
	for _, re := range secretPatterns {
		text = re.ReplaceAllString(text, RedactedPlaceholder)
	}
	text = assignedSecretRe.ReplaceAllStringFunc(text, func(match string) string {
		mt := assignedSecretRe.FindStringSubmatch(match)
		if len(mt) != 4 {
			return match
		}
		key, sep, value := mt[1], mt[2], mt[3]
		if strings.HasSuffix(strings.ToLower(key), "_env") || envReferenceRe.MatchString(value) {
			return match
		}
		if v := strings.Trim(value, `"'`); v == "" || v == RedactedPlaceholder {
			return match
		}
		return key + sep + RedactedPlaceholder
	})
	return text
}

// collapse folds a value onto a single line so it cannot break list structure.
func collapse(s string) string {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' })
	for i := range fields {
		fields[i] = strings.TrimSpace(fields[i])
	}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return strings.Join(out, " ")
}
