package project

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixedClock(base time.Time) func() time.Time {
	n := 0
	return func() time.Time {
		n++
		return base.Add(time.Duration(n) * time.Minute)
	}
}

func TestLoadOrCreateMissingFile(t *testing.T) {
	dir := t.TempDir()
	m, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if !m.Created() {
		t.Error("Created should be true for a missing Boop.md")
	}
	if want := filepath.Join(dir, MemoryFileName); m.Path() != want {
		t.Errorf("Path = %q, want %q", m.Path(), want)
	}
	if _, err := os.Stat(m.Path()); !os.IsNotExist(err) {
		t.Error("LoadOrCreate must not touch the filesystem")
	}

	if err := m.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(m.Path())
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	for _, title := range CanonicalSections() {
		containsSubstring(t, string(data), "## "+title)
	}
	if m.Created() {
		t.Error("Created should be false after Save")
	}
}

func TestLoadOrCreateHonoursExistingCasing(t *testing.T) {
	dir := t.TempDir()
	lower := filepath.Join(dir, "boop.md")
	if err := os.WriteFile(lower, []byte("## Goals\n\n- x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if m.Path() != lower {
		t.Errorf("Path = %q, want existing %q", m.Path(), lower)
	}
	if m.Created() {
		t.Error("Created should be false for an existing file")
	}
}

func TestSaveIsLosslessForUntouchedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MemoryFileName)
	if err := os.WriteFile(path, []byte(handEdited), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if err := m.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != handEdited {
		t.Errorf("load+save mutated the file:\n--- want ---\n%q\n--- got ---\n%q", handEdited, got)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 600 preserved", perm)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("temporary file left behind: %v", entries)
	}
}

func TestAppendDecision(t *testing.T) {
	m := newTestMemory(t)
	m.AppendDecision("Use modernc SQLite to keep builds CGO-free")
	m.AppendDecision("")
	m.AppendDecision("Line one\nline two")

	text := m.Document().Text(SectionDecisions)
	lines := strings.Split(text, "\n\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 decisions, got %q", text)
	}
	containsSubstring(t, text, "2026-03-01 — Use modernc SQLite to keep builds CGO-free")
	containsSubstring(t, text, "Line one line two")
	if strings.Contains(text, "\n- Line one\nline two") {
		t.Error("multi-line decision should be collapsed to one bullet")
	}
}

func TestRecordKnownProblemDeduplicates(t *testing.T) {
	m := newTestMemory(t)
	m.RecordKnownProblem("flaky pty test on windows")
	m.RecordKnownProblem("flaky pty test on windows")
	m.RecordKnownProblem("lint is slow")
	text := m.Document().Text(SectionKnownProblems)
	if n := strings.Count(text, "flaky pty test on windows"); n != 1 {
		t.Errorf("duplicate problem recorded %d times:\n%s", n, text)
	}
	containsSubstring(t, text, "- lint is slow")
}

func TestSetCurrentWorkReplaces(t *testing.T) {
	m := newTestMemory(t)
	m.SetCurrentWork("Implementing the provider router")
	m.SetCurrentWork("Implementing /prep")
	text := m.Document().Text(SectionCurrentWork)
	if text != "Implementing /prep" {
		t.Errorf("Current Work = %q", text)
	}
}

func TestSessionSummaryTrimming(t *testing.T) {
	tests := []struct {
		name         string
		maxSummaries int
		maxArchived  int
		append       int
		wantFull     int
		wantArchived int
	}{
		{"under cap", 5, 10, 3, 3, 0},
		{"at cap", 5, 10, 5, 5, 0},
		{"over cap compresses oldest", 5, 10, 8, 5, 3},
		{"archive also capped", 3, 4, 20, 3, 4},
		{"default caps", 0, 0, 5, 5, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestMemory(t)
			m.MaxSessionSummaries = tc.maxSummaries
			m.MaxArchivedSummaries = tc.maxArchived
			for i := 0; i < tc.append; i++ {
				m.AppendSessionSummary(SessionSummary{
					Title:   fmt.Sprintf("session %02d", i),
					Bullets: []string{fmt.Sprintf("detail %02d", i)},
				})
			}
			full := m.Summaries()
			if len(full) != tc.wantFull {
				t.Fatalf("verbatim summaries = %d, want %d:\n%s", len(full), tc.wantFull, m.Document().Text(SectionSessionSummaries))
			}
			archived := m.ArchivedSummaries()
			if len(archived) != tc.wantArchived {
				t.Fatalf("archived summaries = %d, want %d:\n%s", len(archived), tc.wantArchived, m.Document().Text(SectionSessionSummaries))
			}
			// The newest entries survive verbatim, the oldest are compressed
			// or dropped: Boop.md must never grow without bound.
			if tc.wantFull > 0 {
				newest := fmt.Sprintf("session %02d", tc.append-1)
				if full[len(full)-1].Title != newest {
					t.Errorf("newest summary = %q, want %q", full[len(full)-1].Title, newest)
				}
				if len(full[len(full)-1].Bullets) != 1 {
					t.Errorf("newest summary lost its detail: %+v", full[len(full)-1])
				}
			}
			for _, a := range archived {
				if strings.Contains(a, "detail ") {
					t.Errorf("archived entry kept full detail: %q", a)
				}
			}
			body := m.Document().Text(SectionSessionSummaries)
			if tc.wantArchived > 0 {
				containsSubstring(t, body, "### "+archiveHeading)
			} else if strings.Contains(body, archiveHeading) {
				t.Errorf("unexpected archive block:\n%s", body)
			}
		})
	}
}

func TestSessionSummaryRoundTripThroughFile(t *testing.T) {
	dir := t.TempDir()
	m, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	m.Now = fixedClock(time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC))
	m.AppendSessionSummary(SessionSummary{Title: "first", Bullets: []string{"a", "b"}})
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}

	reloaded, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.Summaries()
	if len(got) != 1 {
		t.Fatalf("summaries = %+v", got)
	}
	if got[0].Title != "first" {
		t.Errorf("title = %q", got[0].Title)
	}
	if got[0].Time.Format(summaryTimeLayout) != "2026-03-01 09:01" {
		t.Errorf("time = %s", got[0].Time)
	}
	if len(got[0].Bullets) != 2 || got[0].Bullets[0] != "a" {
		t.Errorf("bullets = %v", got[0].Bullets)
	}

	reloaded.AppendSessionSummary(SessionSummary{Title: "second"})
	if titles := summaryTitles(reloaded.Summaries()); strings.Join(titles, ",") != "first,second" {
		t.Errorf("summaries out of order: %v", titles)
	}
}

func TestMemoryMutationsPreserveUnknownSections(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, MemoryFileName), []byte(handEdited), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	m.Now = fixedClock(time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC))
	m.AppendDecision("keep the parser dumb")
	m.SetCurrentWork("wiring /prep")
	m.RecordKnownProblem("no windows CI yet")
	m.AppendSessionSummary(SessionSummary{Title: "worked on memory"})
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, MemoryFileName))
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	for _, fragment := range []string{
		"Some notes I wrote above the first section.",
		"## Team Conventions",
		"We squash-merge. Ask Sam before touching billing.",
		"- Keep the parser boring",
		"## Not A Real Section",
		"### 2026-01-02 09:00 — Bootstrapped",
		"- did things",
		"keep the parser dumb",
		"wiring /prep",
		"no windows CI yet",
		"worked on memory",
	} {
		containsSubstring(t, out, fragment)
	}
}

func TestRedact(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantSub string
	}{
		{name: "openai key", in: "use sk-abcdefghijklmnopqrstuvwx12345 now", want: "use " + RedactedPlaceholder + " now"},
		{name: "github token", in: "token ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456", want: "token " + RedactedPlaceholder},
		{name: "aws key id", in: "AKIAIOSFODNN7EXAMPLE", want: RedactedPlaceholder},
		{name: "slack token", in: "xoxb-1234567890-abcdefghij", want: RedactedPlaceholder},
		{name: "google key", in: "AIzaSyA1234567890abcdefghijklmnopqrst", want: RedactedPlaceholder},
		{name: "bearer header", in: "Authorization: Bearer abcdef1234567890xyz", want: "Authorization: " + RedactedPlaceholder},
		{name: "assigned api key", in: `api_key: "abc123secretvalue"`, want: "api_key: " + RedactedPlaceholder},
		{name: "assigned password", in: "password=hunter2trombone", want: "password=" + RedactedPlaceholder},
		{name: "env var reference kept", in: "api_key_env: OPENAI_API_KEY", want: "api_key_env: OPENAI_API_KEY"},
		{name: "env interpolation kept", in: "token: ${GITHUB_TOKEN}", want: "token: ${GITHUB_TOKEN}"},
		{name: "placeholder kept", in: "password: <your-password>", want: "password: <your-password>"},
		{name: "ordinary prose untouched", in: "The token bucket limiter is fine.", want: "The token bucket limiter is fine."},
		{name: "private key block", in: "-----BEGIN RSA PRIVATE KEY-----\nMIIabc\n-----END RSA PRIVATE KEY-----", want: RedactedPlaceholder},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Redact(tc.in); got != tc.want {
				t.Errorf("Redact(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestMemoryNeverWritesSecrets(t *testing.T) {
	m := newTestMemory(t)
	m.AppendDecision("Rotated the key to sk-abcdefghijklmnopqrstuvwx12345")
	m.SetCurrentWork("debugging auth with password=hunter2trombone")
	m.RecordKnownProblem("CI fails: Bearer abcdef1234567890xyz rejected")
	m.AppendSessionSummary(SessionSummary{
		Title:   "fixed auth using ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456",
		Bullets: []string{"stored AKIAIOSFODNN7EXAMPLE in the vault"},
	})
	out := string(m.Render())
	for _, secret := range []string{
		"sk-abcdefghijklmnopqrstuvwx12345",
		"hunter2trombone",
		"abcdef1234567890xyz",
		"ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456",
		"AKIAIOSFODNN7EXAMPLE",
	} {
		if strings.Contains(out, secret) {
			t.Errorf("secret %q reached Boop.md:\n%s", secret, out)
		}
	}
	containsSubstring(t, out, RedactedPlaceholder)
}

func TestOpenMemoryMissingFile(t *testing.T) {
	m, err := OpenMemory(filepath.Join(t.TempDir(), "nested", MemoryFileName))
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	if !m.Created() {
		t.Error("Created should be true")
	}
	if err := m.Save(); err != nil {
		t.Fatalf("Save should create parent directories: %v", err)
	}
	if _, err := os.Stat(m.Path()); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

func newTestMemory(t *testing.T) *Memory {
	t.Helper()
	m, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	m.Now = fixedClock(time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC))
	return m
}

func summaryTitles(list []SessionSummary) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, s.Title)
	}
	return out
}
