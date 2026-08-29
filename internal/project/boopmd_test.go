package project

import (
	"strings"
	"testing"
)

// handEdited is a Boop.md a user has reorganised: sections Boop does not know
// about, prose under headings, a fenced code block containing a line that
// looks like a heading, and a missing canonical section.
const handEdited = `# Boop Project Memory

Some notes I wrote above the first section.

## Project

Name: widget
Root: /srv/widget

## Team Conventions

We squash-merge. Ask Sam before touching billing.

## Goals

- Ship v2
- Keep the parser boring

## Architecture

The parser is intentionally dumb. Example of what NOT to write:

` + "```markdown" + `
## Not A Real Section
This lives inside a fence.
` + "```" + `

## Session Summaries

### 2026-01-02 09:00 — Bootstrapped

- did things
`

func TestParseRenderRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{"empty", ""},
		{"only prose", "just some text\nwith no headings at all\n"},
		{"hand edited", handEdited},
		{"no trailing newline", "## Goals\n\n- one thing"},
		{"crlf line endings", "# Boop Project Memory\r\n\r\n## Project\r\n\r\nName: x\r\n"},
		{"duplicate sections", "## Goals\n\na\n\n## Goals\n\nb\n"},
		{"heading without space", "##Goals\n\nnot a section heading\n"},
		{"indented code block", "## Notes\n\n    ## indented code\n\ntext\n"},
		{"tilde fence", "## Notes\n\n~~~\n## fenced\n~~~\n\ntail\n"},
		{"trailing whitespace preserved", "## Goals   \n\n- item with trailing space   \n\n\n"},
		{"generated template", NewDocument().String()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := string(Parse([]byte(tc.src)).Render())
			if got != tc.src {
				t.Errorf("round trip changed the document.\n--- want ---\n%q\n--- got ---\n%q", tc.src, got)
			}
		})
	}
}

func TestParseSections(t *testing.T) {
	d := Parse([]byte(handEdited))

	wantTitles := []string{"Project", "Team Conventions", "Goals", "Architecture", "Session Summaries"}
	got := d.Titles()
	if len(got) != len(wantTitles) {
		t.Fatalf("titles = %v, want %v", got, wantTitles)
	}
	for i, want := range wantTitles {
		if got[i] != want {
			t.Errorf("title[%d] = %q, want %q", i, got[i], want)
		}
	}
	if !strings.Contains(d.Preamble(), "Some notes I wrote") {
		t.Errorf("preamble lost user prose: %q", d.Preamble())
	}
	if d.Text(SectionGoals) != "- Ship v2\n- Keep the parser boring" {
		t.Errorf("Goals text = %q", d.Text(SectionGoals))
	}
	if !strings.Contains(d.Text(SectionArchitecture), "## Not A Real Section") {
		t.Error("fenced pseudo-heading should stay inside the Architecture body")
	}
	if d.Has("Decisions") {
		t.Error("Decisions should be absent from the hand-edited document")
	}
	if d.Section("gOaLs") == nil {
		t.Error("section lookup should be case-insensitive")
	}
	if d.Text("Nope") != "" {
		t.Error("missing section should read as empty")
	}
}

func TestEnsureCanonicalPreservesUserContent(t *testing.T) {
	d := Parse([]byte(handEdited))
	d.EnsureCanonical()

	for _, title := range CanonicalSections() {
		if !d.Has(title) {
			t.Errorf("EnsureCanonical did not add %q", title)
		}
	}
	if !d.Has("Team Conventions") {
		t.Fatal("unknown section was dropped")
	}
	out := d.String()
	for _, fragment := range []string{
		"Some notes I wrote above the first section.",
		"We squash-merge. Ask Sam before touching billing.",
		"- Keep the parser boring",
		"## Not A Real Section",
		"### 2026-01-02 09:00 — Bootstrapped",
	} {
		containsSubstring(t, out, fragment)
	}

	// Canonical sections keep their relative order even around unknown ones.
	var ranks []int
	for _, title := range d.Titles() {
		if r := canonicalRank(title); r >= 0 {
			ranks = append(ranks, r)
		}
	}
	for i := 1; i < len(ranks); i++ {
		if ranks[i] < ranks[i-1] {
			t.Fatalf("canonical sections out of order: %v (%v)", ranks, d.Titles())
		}
	}

	// A second pass must be a no-op.
	before := d.String()
	d.EnsureCanonical()
	if after := d.String(); after != before {
		t.Errorf("EnsureCanonical is not idempotent:\n%q\n%q", before, after)
	}
}

func TestSetTextAndAppendText(t *testing.T) {
	d := NewDocument()
	d.SetText(SectionGoals, "\n\n- one\n- two\n\n")
	if got := d.Text(SectionGoals); got != "- one\n- two" {
		t.Errorf("Text = %q", got)
	}
	d.AppendText(SectionGoals, "- three")
	if got := d.Text(SectionGoals); got != "- one\n- two\n\n- three" {
		t.Errorf("after append Text = %q", got)
	}
	d.AppendText(SectionGoals, "   \n  ")
	if got := d.Text(SectionGoals); got != "- one\n- two\n\n- three" {
		t.Errorf("appending blank text should be a no-op, got %q", got)
	}
	d.SetText(SectionGoals, "")
	if got := d.Text(SectionGoals); got != "" {
		t.Errorf("cleared Text = %q", got)
	}
	// Rendering stays well-formed after mutation.
	if strings.Contains(d.String(), "\n\n\n") {
		t.Errorf("document has collapsed blank-line handling:\n%s", d.String())
	}
}

func TestSetTextInsertsInCanonicalPosition(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		insert  string
		wantIdx int
	}{
		{"before later canonical", "## Project\n\na\n\n## Tests\n\nb\n", SectionGoals, 1},
		{"after earlier canonical", "## Project\n\na\n", SectionTests, 1},
		{"first when nothing earlier", "## Tests\n\nb\n", SectionProject, 0},
		{"after unknown trailing section", "## Project\n\na\n\n## Scratch\n\nx\n", SectionGoals, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := Parse([]byte(tc.src))
			d.SetText(tc.insert, "inserted")
			titles := d.Titles()
			if titles[tc.wantIdx] != tc.insert {
				t.Errorf("titles = %v, want %q at index %d", titles, tc.insert, tc.wantIdx)
			}
			if !strings.Contains(d.String(), "## "+tc.insert+"\n\ninserted\n") {
				t.Errorf("unexpected rendering:\n%s", d.String())
			}
			// Nothing from the original may be lost.
			for _, line := range strings.Split(tc.src, "\n") {
				if line = strings.TrimSpace(line); line != "" {
					containsSubstring(t, d.String(), line)
				}
			}
		})
	}
}

func TestSetTextOnUnterminatedLastSection(t *testing.T) {
	d := Parse([]byte("## Project\n\nName: x"))
	d.SetText(SectionGoals, "- ship")
	out := d.String()
	containsSubstring(t, out, "Name: x\n## Goals")
	if strings.Contains(out, "Name: x## Goals") {
		t.Errorf("heading glued onto user's last line:\n%q", out)
	}
}

func TestManagedBlock(t *testing.T) {
	d := Parse([]byte("## Project\n\nHand-written note that must survive.\n"))

	d.SetManagedText(SectionProject, "Name: widget\nRoot: /srv/widget")
	out := d.String()
	containsSubstring(t, out, managedStart)
	containsSubstring(t, out, managedEnd)
	containsSubstring(t, out, "Name: widget")
	containsSubstring(t, out, "Hand-written note that must survive.")

	if got := d.ManagedText(SectionProject); got != "Name: widget\nRoot: /srv/widget" {
		t.Errorf("ManagedText = %q", got)
	}

	// Regenerating replaces only the managed region.
	d.SetManagedText(SectionProject, "Name: widget2")
	out = d.String()
	containsSubstring(t, out, "Name: widget2")
	containsSubstring(t, out, "Hand-written note that must survive.")
	if strings.Contains(out, "Root: /srv/widget") {
		t.Error("stale generated content survived regeneration")
	}
	if n := strings.Count(out, managedStart); n != 1 {
		t.Errorf("managed block duplicated %d times:\n%s", n, out)
	}

	// A user may add prose after the block; it must survive too.
	sec := d.Section(SectionProject)
	sec.SetText(sec.Text() + "\n\nMore prose added later.")
	d.SetManagedText(SectionProject, "Name: widget3")
	out = d.String()
	containsSubstring(t, out, "More prose added later.")
	containsSubstring(t, out, "Name: widget3")

	// Managed text of a section without a block, or a missing section, is empty.
	if got := d.ManagedText(SectionGoals); got != "" {
		t.Errorf("ManagedText(Goals) = %q", got)
	}
	if got := d.ManagedText("Nope"); got != "" {
		t.Errorf("ManagedText(missing) = %q", got)
	}
}

func TestManagedBlockRoundTripsAfterRegeneration(t *testing.T) {
	d := Parse([]byte(handEdited))
	d.EnsureCanonical()
	d.SetManagedText(SectionProject, "Name: widget")
	first := d.Render()

	// Parsing what we wrote and regenerating identical content must be stable,
	// otherwise repeated /prep runs would churn the file.
	d2 := Parse(first)
	d2.EnsureCanonical()
	d2.SetManagedText(SectionProject, "Name: widget")
	if second := string(d2.Render()); second != string(first) {
		t.Errorf("regeneration is not stable:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

func TestSetPreamble(t *testing.T) {
	d := Parse([]byte("## Goals\n\n- x\n"))
	d.SetPreamble("# Boop Project Memory")
	containsSubstring(t, d.String(), "# Boop Project Memory\n\n## Goals")
	d.SetPreamble("  \n ")
	if d.Preamble() != "" {
		t.Errorf("Preamble = %q, want empty", d.Preamble())
	}
}

func TestNewDocumentHasCanonicalSections(t *testing.T) {
	d := NewDocument()
	got := d.Titles()
	want := CanonicalSections()
	if len(got) != len(want) {
		t.Fatalf("titles = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("titles = %v, want %v", got, want)
		}
	}
	if !strings.HasPrefix(d.String(), DocumentTitle+"\n\n## Project\n") {
		t.Errorf("unexpected template:\n%s", d.String())
	}
}
