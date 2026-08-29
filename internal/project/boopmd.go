package project

import (
	"strings"
)

// MemoryFileName is the canonical name of the project memory file (§16).
const MemoryFileName = "Boop.md"

// DocumentTitle is the H1 Boop writes above the sections of a new memory file.
const DocumentTitle = "# Boop Project Memory"

// Canonical section titles of Boop.md, in the order §16 mandates.
const (
	SectionProject          = "Project"
	SectionGoals            = "Goals"
	SectionArchitecture     = "Architecture"
	SectionImportantFiles   = "Important Files"
	SectionDecisions        = "Decisions"
	SectionCurrentWork      = "Current Work"
	SectionKnownProblems    = "Known Problems"
	SectionTests            = "Tests"
	SectionUsefulCommands   = "Useful Commands"
	SectionAgentNotes       = "Agent Notes"
	SectionSessionSummaries = "Session Summaries"
)

// canonicalOrder is the authoritative section order. Position matters: a
// missing section is inserted relative to the canonical sections that already
// exist, so a hand-edited file keeps a predictable shape.
var canonicalOrder = []string{
	SectionProject,
	SectionGoals,
	SectionArchitecture,
	SectionImportantFiles,
	SectionDecisions,
	SectionCurrentWork,
	SectionKnownProblems,
	SectionTests,
	SectionUsefulCommands,
	SectionAgentNotes,
	SectionSessionSummaries,
}

// CanonicalSections returns the §16 section titles in order.
func CanonicalSections() []string {
	out := make([]string, len(canonicalOrder))
	copy(out, canonicalOrder)
	return out
}

// Markers delimiting the region of a section that Boop owns. Everything
// outside the markers belongs to the user and is never rewritten, which is how
// generated facts and hand-written prose coexist in one section.
const (
	managedStart = "<!-- boop:generated -->"
	managedEnd   = "<!-- /boop:generated -->"
)

// Section is one "## Title" block of a Boop.md document.
//
// The raw heading and body text are kept verbatim so that parsing and
// re-rendering an untouched document is byte-for-byte lossless; only explicit
// mutations normalise formatting.
type Section struct {
	// Title is the heading text with surrounding whitespace removed.
	Title string

	heading string // raw heading line, including its line terminator
	body    string // raw text between this heading and the next
}

// Body returns the section's raw text, exactly as it appears in the file.
func (s *Section) Body() string { return s.body }

// Text returns the section body with leading and trailing blank lines removed.
func (s *Section) Text() string { return trimBlankLines(s.body) }

// SetText replaces the section body with text, normalising the surrounding
// blank lines to the canonical single-blank-line layout.
func (s *Section) SetText(text string) {
	text = trimBlankLines(text)
	if text == "" {
		s.body = "\n"
		return
	}
	s.body = "\n" + text + "\n\n"
}

// Document is a parsed Boop.md.
//
// A Document is an ordered list of level-2 sections plus the preamble that
// precedes the first one. Unknown sections, unknown ordering and arbitrary
// prose are all preserved: users edit this file by hand and must never lose
// work to a round trip through Boop.
type Document struct {
	preamble string
	sections []*Section
}

// NewDocument returns an empty memory document containing every canonical
// section in §16 order.
func NewDocument() *Document {
	d := &Document{preamble: DocumentTitle + "\n\n"}
	for _, title := range canonicalOrder {
		d.sections = append(d.sections, &Section{
			Title:   title,
			heading: "## " + title + "\n",
			body:    "\n",
		})
	}
	return d
}

// Parse reads a Boop.md document. Parsing never fails: any input is a valid
// document, because refusing to load a hand-edited file would be worse than
// treating unrecognised text as prose.
//
// Only level-2 ATX headings ("## Title") outside fenced code blocks start a
// section, so H1 titles stay in the preamble and H3 sub-headings stay inside
// their section's body.
func Parse(data []byte) *Document {
	d := &Document{}
	var (
		cur   *Section
		pre   strings.Builder
		bodyB strings.Builder
		fence string
	)
	flush := func() {
		if cur != nil {
			cur.body = bodyB.String()
			bodyB.Reset()
		}
	}
	for _, line := range splitLines(string(data)) {
		text := strings.TrimRight(line, "\r\n")
		if marker := fenceMarker(text); marker != "" {
			switch {
			case fence == "":
				fence = marker
			case strings.HasPrefix(marker, fence[:1]) && len(marker) >= len(fence):
				fence = ""
			}
		}
		if fence == "" && isSectionHeading(text) {
			flush()
			cur = &Section{
				Title:   strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "##")),
				heading: line,
			}
			d.sections = append(d.sections, cur)
			continue
		}
		if cur == nil {
			pre.WriteString(line)
			continue
		}
		bodyB.WriteString(line)
	}
	flush()
	d.preamble = pre.String()
	return d
}

// Render serialises the document. For a document that has not been mutated,
// Render(Parse(b)) == b.
func (d *Document) Render() []byte {
	var b strings.Builder
	b.WriteString(d.preamble)
	for _, s := range d.sections {
		b.WriteString(s.heading)
		b.WriteString(s.body)
	}
	return []byte(b.String())
}

// String returns the rendered document.
func (d *Document) String() string { return string(d.Render()) }

// Preamble returns the text before the first section, typically the H1 title.
func (d *Document) Preamble() string { return d.preamble }

// SetPreamble replaces the text before the first section.
func (d *Document) SetPreamble(text string) {
	text = trimBlankLines(text)
	if text == "" {
		d.preamble = ""
		return
	}
	d.preamble = text + "\n\n"
}

// Titles returns the section titles in document order.
func (d *Document) Titles() []string {
	out := make([]string, 0, len(d.sections))
	for _, s := range d.sections {
		out = append(out, s.Title)
	}
	return out
}

// Sections returns the document's sections in order.
func (d *Document) Sections() []*Section { return d.sections }

// Section returns the first section with the given title, matched
// case-insensitively, or nil.
func (d *Document) Section(title string) *Section {
	key := sectionKey(title)
	for _, s := range d.sections {
		if sectionKey(s.Title) == key {
			return s
		}
	}
	return nil
}

// Has reports whether the document contains the named section.
func (d *Document) Has(title string) bool { return d.Section(title) != nil }

// Text returns the trimmed body of the named section, or "" if absent.
func (d *Document) Text(title string) string {
	if s := d.Section(title); s != nil {
		return s.Text()
	}
	return ""
}

// SetText replaces the body of the named section, creating it in canonical
// position if it does not exist.
func (d *Document) SetText(title, text string) {
	d.ensure(title).SetText(text)
}

// AppendText appends a block of text to the named section, separated from any
// existing content by a blank line. The section is created if missing.
func (d *Document) AppendText(title, text string) {
	text = trimBlankLines(text)
	if text == "" {
		return
	}
	s := d.ensure(title)
	if existing := s.Text(); existing != "" {
		text = existing + "\n\n" + text
	}
	s.SetText(text)
}

// ManagedText returns the content of the section's Boop-generated block, or ""
// if the section has none.
func (d *Document) ManagedText(title string) string {
	s := d.Section(title)
	if s == nil {
		return ""
	}
	lines := splitLines(s.body)
	start, end, ok := managedRange(lines)
	if !ok {
		return ""
	}
	return trimBlankLines(strings.Join(lines[start+1:end], ""))
}

// SetManagedText replaces the Boop-generated block of the named section,
// leaving any user-written text in that section untouched. If no block exists
// yet one is inserted at the top of the section, above the user's prose.
func (d *Document) SetManagedText(title, content string) {
	s := d.ensure(title)
	block := managedStart + "\n\n" + trimBlankLines(content) + "\n\n" + managedEnd + "\n"
	lines := splitLines(s.body)
	if start, end, ok := managedRange(lines); ok {
		rest := strings.Join(lines[end+1:], "")
		s.body = strings.Join(lines[:start], "") + block + rest
		return
	}
	rest := trimBlankLines(s.body)
	if rest == "" {
		s.body = "\n" + block + "\n"
		return
	}
	s.body = "\n" + block + "\n" + rest + "\n\n"
}

// EnsureCanonical adds any missing §16 section, each in canonical position.
// Existing sections, including ones Boop does not know about, are left alone.
func (d *Document) EnsureCanonical() {
	if trimBlankLines(d.preamble) == "" {
		d.preamble = DocumentTitle + "\n\n"
	}
	for _, title := range canonicalOrder {
		d.ensure(title)
	}
}

// ensure returns the named section, inserting an empty one if needed.
func (d *Document) ensure(title string) *Section {
	if s := d.Section(title); s != nil {
		return s
	}
	s := &Section{Title: title, heading: "## " + title + "\n", body: "\n"}
	idx := d.insertIndex(title)
	// Guarantee the preceding block ends with a newline, so inserting a
	// heading can never glue itself onto the user's last line.
	if idx == 0 {
		if d.preamble != "" && !strings.HasSuffix(d.preamble, "\n") {
			d.preamble += "\n"
		}
	} else if prev := d.sections[idx-1]; prev.body == "" {
		prev.body = "\n"
	} else if !strings.HasSuffix(prev.body, "\n") {
		prev.body += "\n"
	}
	d.sections = append(d.sections, nil)
	copy(d.sections[idx+1:], d.sections[idx:])
	d.sections[idx] = s
	return s
}

// insertIndex reports where a canonical section belongs: before the first
// canonical section that should follow it, else immediately after the last
// canonical section that should precede it, else at the end. Keeping canonical
// sections grouped means an unknown trailing section the user appended stays
// at the bottom where they put it.
func (d *Document) insertIndex(title string) int {
	rank := canonicalRank(title)
	if rank < 0 {
		return len(d.sections)
	}
	after := -1
	for i, s := range d.sections {
		r := canonicalRank(s.Title)
		if r < 0 {
			continue
		}
		if r > rank {
			return i
		}
		after = i
	}
	if after >= 0 {
		return after + 1
	}
	return len(d.sections)
}

func canonicalRank(title string) int {
	key := sectionKey(title)
	for i, t := range canonicalOrder {
		if sectionKey(t) == key {
			return i
		}
	}
	return -1
}

func sectionKey(title string) string { return strings.ToLower(strings.TrimSpace(title)) }

// managedRange locates the generated block within raw body lines.
func managedRange(lines []string) (start, end int, ok bool) {
	start, end = -1, -1
	for i, l := range lines {
		switch strings.TrimSpace(l) {
		case managedStart:
			if start < 0 {
				start = i
			}
		case managedEnd:
			if start >= 0 && end < 0 {
				end = i
			}
		}
	}
	if start < 0 || end < 0 || end < start {
		return 0, 0, false
	}
	return start, end, true
}

// isSectionHeading reports whether text is a level-2 ATX heading.
func isSectionHeading(text string) bool {
	t := strings.TrimLeft(text, " ")
	if len(text)-len(t) > 3 {
		return false // 4+ spaces is an indented code block
	}
	return strings.HasPrefix(t, "## ") && !strings.HasPrefix(t, "###")
}

// fenceMarker returns the fence run that opens or closes a code block, or "".
func fenceMarker(text string) string {
	t := strings.TrimLeft(text, " ")
	if len(text)-len(t) > 3 {
		return ""
	}
	for _, c := range []byte{'`', '~'} {
		n := 0
		for n < len(t) && t[n] == c {
			n++
		}
		if n >= 3 {
			return t[:n]
		}
	}
	return ""
}

// splitLines splits s into lines, keeping each line's terminator so that
// joining the result reproduces s exactly.
func splitLines(s string) []string {
	var out []string
	for len(s) > 0 {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			out = append(out, s)
			break
		}
		out = append(out, s[:i+1])
		s = s[i+1:]
	}
	return out
}

// trimBlankLines removes leading and trailing whitespace-only lines and any
// trailing line terminator, leaving interior blank lines intact.
func trimBlankLines(s string) string {
	lines := splitLines(s)
	start, end := 0, len(lines)
	for start < end && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return strings.TrimRight(strings.Join(lines[start:end], ""), "\r\n")
}
