package session_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/boop-dev/boop/internal/provider"
	"github.com/boop-dev/boop/internal/session"
)

// wordCounter counts whitespace-separated words. It exists to prove the
// TokenCounter is genuinely pluggable rather than a hard-coded heuristic.
type wordCounter struct{ calls int }

func (c *wordCounter) CountText(s string) int {
	c.calls++
	return len(strings.Fields(s))
}

// failingSummarizer always errors, exercising the fallback path.
type failingSummarizer struct{}

func (failingSummarizer) Summarize(context.Context, []provider.Message) (string, error) {
	return "", errors.New("summarizer unavailable")
}

// fixedSummarizer returns a canned summary.
type fixedSummarizer struct {
	text string
	seen int
}

func (f *fixedSummarizer) Summarize(_ context.Context, evicted []provider.Message) (string, error) {
	f.seen = len(evicted)
	return f.text, nil
}

// userTurn is a convenience for building history.
func userTurn(content string) provider.Message {
	return provider.Message{Role: provider.RoleUser, Content: content}
}

func assistantTurn(content string) provider.Message {
	return provider.Message{Role: provider.RoleAssistant, Content: content}
}

func TestHeuristicCounter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		counter session.HeuristicCounter
		text    string
		want    int
	}{
		{"empty", session.HeuristicCounter{}, "", 0},
		{"default ratio rounds up", session.HeuristicCounter{}, "abcde", 2},
		{"exact multiple", session.HeuristicCounter{}, "abcdefgh", 2},
		{"single character costs one", session.HeuristicCounter{}, "a", 1},
		{"custom ratio", session.HeuristicCounter{CharsPerToken: 2}, "abcd", 2},
		{"invalid ratio falls back", session.HeuristicCounter{CharsPerToken: -3}, "abcd", 1},
		{"counts runes not bytes", session.HeuristicCounter{CharsPerToken: 1}, "æøå", 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.counter.CountText(tc.text); got != tc.want {
				t.Errorf("CountText(%q) = %d, want %d", tc.text, got, tc.want)
			}
		})
	}
}

func TestCountMessageIncludesEverything(t *testing.T) {
	t.Parallel()
	counter := session.HeuristicCounter{CharsPerToken: 1}

	tests := []struct {
		name string
		msg  provider.Message
		// want is expressed relative to a plain message so the per-message
		// framing constant does not have to be duplicated here.
		moreThan provider.Message
	}{
		{"tool calls cost tokens",
			provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{Name: "run", Arguments: `{"a":1}`}}},
			provider.Message{Role: provider.RoleAssistant}},
		{"text parts cost tokens",
			provider.Message{Role: provider.RoleUser, Parts: []provider.ContentPart{{Kind: provider.PartText, Text: "hello"}}},
			provider.Message{Role: provider.RoleUser}},
		{"binary parts are not free",
			provider.Message{Role: provider.RoleUser, Parts: []provider.ContentPart{{Kind: provider.PartImage, Data: []byte{1, 2, 3}}}},
			provider.Message{Role: provider.RoleUser}},
		{"name and tool call id cost tokens",
			provider.Message{Role: provider.RoleTool, Name: "run", ToolCallID: "call-1"},
			provider.Message{Role: provider.RoleTool}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := session.CountMessage(counter, tc.msg)
			base := session.CountMessage(counter, tc.moreThan)
			if got <= base {
				t.Errorf("CountMessage = %d, want more than the %d of a bare message", got, base)
			}
		})
	}

	if session.CountMessage(nil, userTurn("hi")) == 0 {
		t.Error("CountMessage with a nil counter should fall back to the default estimator")
	}
	if got := session.CountMessages(counter, []provider.Message{userTurn("a"), userTurn("b")}); got == 0 {
		t.Error("CountMessages = 0")
	}
}

func TestNewContextManagerAppliesDefaults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   session.Options
		want session.Options
	}{
		{"zero value", session.Options{}, session.Options{
			Budget: session.DefaultBudget, Reserve: 0,
			MinRecentMessages: session.DefaultMinRecentMessages, SummaryBudget: session.DefaultSummaryBudget,
		}},
		{"negatives are clamped", session.Options{Budget: 100, Reserve: -1, MinRecentMessages: -1, SummaryBudget: -1},
			session.Options{Budget: 100, Reserve: 0, MinRecentMessages: 0, SummaryBudget: 0}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := session.NewContextManager(tc.in).Options()
			if got.Budget != tc.want.Budget || got.Reserve != tc.want.Reserve ||
				got.MinRecentMessages != tc.want.MinRecentMessages || got.SummaryBudget != tc.want.SummaryBudget {
				t.Errorf("options = %+v, want %+v", got, tc.want)
			}
			if got.Counter == nil {
				t.Error("Counter was not defaulted")
			}
			if got.Summarizer == nil {
				t.Error("Summarizer was not defaulted")
			}
		})
	}
}

func TestBuildIncludesEverythingWhenItFits(t *testing.T) {
	t.Parallel()
	cm := session.NewContextManager(session.Options{Budget: 100000, Reserve: 0})
	sel := session.NewSelection()
	sel.AddFile("main.go", "package main")
	sel.AddToolResult("run", "all tests passed", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	asm, err := cm.Build(context.Background(), session.Input{
		SystemPrompt:  "You are Boop.",
		ProjectMemory: "# Boop Project Memory\n\n## Goals\nShip v0.1.0.",
		Selection:     sel,
		History:       []provider.Message{userTurn("hello"), assistantTurn("hi")},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !asm.HasSystem || len(asm.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (system + 2 turns)", len(asm.Messages))
	}
	system := asm.Messages[0].Content
	for _, want := range []string{"You are Boop.", "Boop Project Memory", "main.go", "package main", "all tests passed"} {
		if !strings.Contains(system, want) {
			t.Errorf("system message is missing %q:\n%s", want, system)
		}
	}
	if asm.EvictedMessages != 0 || len(asm.DroppedFiles) != 0 || asm.DroppedToolResults != 0 {
		t.Errorf("nothing should have been dropped: %+v", asm)
	}
	if !asm.ProjectMemoryIncluded {
		t.Error("ProjectMemoryIncluded = false")
	}
	if !asm.WithinBudget() {
		t.Errorf("tokens %d exceed budget %d", asm.Tokens, asm.Budget)
	}
}

func TestBuildNeverSendsTheWholeSession(t *testing.T) {
	t.Parallel()
	// A long history against a small budget: this is the §47 invariant.
	var history []provider.Message
	for i := range 200 {
		history = append(history, userTurn(fmt.Sprintf("turn %03d %s", i, strings.Repeat("filler ", 20))))
	}
	cm := session.NewContextManager(session.Options{Budget: 1000, Reserve: 100, MinRecentMessages: 2})

	asm, err := cm.Build(context.Background(), session.Input{SystemPrompt: "sys", History: history})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if asm.EvictedMessages == 0 {
		t.Fatal("nothing was evicted; the whole session was sent")
	}
	if len(asm.Messages) >= len(history) {
		t.Fatalf("assembled %d messages from a %d-turn history", len(asm.Messages), len(history))
	}
	if !asm.WithinBudget() {
		t.Errorf("tokens %d exceed budget %d", asm.Tokens, asm.Budget)
	}
	// The system prompt survives, and so do the newest turns.
	if !asm.HasSystem || !strings.Contains(asm.Messages[0].Content, "sys") {
		t.Error("the system prompt was not preserved")
	}
	last := asm.Messages[len(asm.Messages)-1]
	if !strings.Contains(last.Content, "turn 199") {
		t.Errorf("newest turn missing; last message is %q", truncate(last.Content))
	}
	secondLast := asm.Messages[len(asm.Messages)-2]
	if !strings.Contains(secondLast.Content, "turn 198") {
		t.Errorf("MinRecentMessages not honoured; second-to-last is %q", truncate(secondLast.Content))
	}
}

func TestBuildSummarizesEvictedTurns(t *testing.T) {
	t.Parallel()
	var history []provider.Message
	for i := range 60 {
		history = append(history, userTurn(fmt.Sprintf("turn %02d %s", i, strings.Repeat("x", 200))))
	}
	summarizer := &fixedSummarizer{text: "The user asked about migrations repeatedly."}
	cm := session.NewContextManager(session.Options{
		Budget: 800, Reserve: 0, MinRecentMessages: 1, SummaryBudget: 60, Summarizer: summarizer,
	})

	asm, err := cm.Build(context.Background(), session.Input{SystemPrompt: "sys", History: history})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if asm.EvictedMessages == 0 {
		t.Fatal("expected evictions")
	}
	if asm.Summary != summarizer.text {
		t.Errorf("Summary = %q, want %q", asm.Summary, summarizer.text)
	}
	if summarizer.seen != asm.EvictedMessages {
		t.Errorf("summarizer saw %d evicted turns, assembly reports %d", summarizer.seen, asm.EvictedMessages)
	}
	if !strings.Contains(asm.Messages[0].Content, summarizer.text) {
		t.Error("the summary is not in the system message")
	}
	if !asm.WithinBudget() {
		t.Errorf("tokens %d exceed budget %d", asm.Tokens, asm.Budget)
	}
}

func TestBuildFallsBackWhenSummarizerFails(t *testing.T) {
	t.Parallel()
	var history []provider.Message
	for i := range 60 {
		history = append(history, userTurn(fmt.Sprintf("turn %02d %s", i, strings.Repeat("x", 200))))
	}
	cm := session.NewContextManager(session.Options{
		Budget: 800, Reserve: 0, MinRecentMessages: 1, Summarizer: failingSummarizer{},
	})
	asm, err := cm.Build(context.Background(), session.Input{SystemPrompt: "sys", History: history})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if asm.Summary == "" {
		t.Fatal("a failed summarizer must still leave a note that context was elided")
	}
	if !strings.Contains(asm.Summary, "omitted") {
		t.Errorf("Summary = %q", asm.Summary)
	}
}

func TestOutlineSummarizerIsTheDefault(t *testing.T) {
	t.Parallel()
	evicted := []provider.Message{
		userTurn("please fix the parser"),
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{Name: "run"}}},
		{Role: provider.RoleUser, Parts: []provider.ContentPart{{Kind: provider.PartText, Text: "from a part"}}},
		{Role: provider.RoleTool},
	}
	got, err := session.OutlineSummarizer{}.Summarize(context.Background(), evicted)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	for _, want := range []string{"4 earlier turns", "please fix the parser", "called run", "from a part", "(no text)"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary is missing %q:\n%s", want, got)
		}
	}

	if empty, err := (session.OutlineSummarizer{}).Summarize(context.Background(), nil); err != nil || empty != "" {
		t.Errorf("Summarize(nil) = %q, %v; want empty", empty, err)
	}

	// Long turns are clipped and the listing itself is capped.
	many := make([]provider.Message, 30)
	for i := range many {
		many[i] = userTurn(strings.Repeat("y", 500))
	}
	got, err = session.OutlineSummarizer{MaxRunesPerMessage: 10, MaxMessages: 3}.Summarize(context.Background(), many)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if n := strings.Count(got, "\n- "); n != 3 {
		t.Errorf("listed %d turns, want 3", n)
	}
	if strings.Contains(got, strings.Repeat("y", 11)) {
		t.Error("per-message clipping was not applied")
	}
}

func TestBuildDropsExplicitSelectionThatDoesNotFit(t *testing.T) {
	t.Parallel()
	sel := session.NewSelection()
	sel.AddFile("small.go", "package main")
	sel.AddFile("huge.go", strings.Repeat("x", 20000))
	sel.AddToolResult("run", strings.Repeat("y", 20000), time.Now())

	cm := session.NewContextManager(session.Options{Budget: 600, Reserve: 0, MinRecentMessages: 1})
	asm, err := cm.Build(context.Background(), session.Input{
		SystemPrompt: "sys", Selection: sel, History: []provider.Message{userTurn("hi")},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(asm.DroppedFiles) != 1 || asm.DroppedFiles[0] != "huge.go" {
		t.Errorf("DroppedFiles = %v, want [huge.go]", asm.DroppedFiles)
	}
	if asm.DroppedToolResults != 1 {
		t.Errorf("DroppedToolResults = %d, want 1", asm.DroppedToolResults)
	}
	if !strings.Contains(asm.Messages[0].Content, "small.go") {
		t.Error("the file that did fit was not included")
	}
	if !asm.WithinBudget() {
		t.Errorf("tokens %d exceed budget %d", asm.Tokens, asm.Budget)
	}
}

func TestBuildDropsProjectMemoryThatDoesNotFit(t *testing.T) {
	t.Parallel()
	cm := session.NewContextManager(session.Options{Budget: 200, Reserve: 0, MinRecentMessages: 1})
	asm, err := cm.Build(context.Background(), session.Input{
		SystemPrompt:  "sys",
		ProjectMemory: strings.Repeat("memory ", 5000),
		History:       []provider.Message{userTurn("hi")},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if asm.ProjectMemoryIncluded {
		t.Error("ProjectMemoryIncluded = true, want false for memory that cannot fit")
	}
	if !asm.WithinBudget() {
		t.Errorf("tokens %d exceed budget %d", asm.Tokens, asm.Budget)
	}
}

func TestBuildPrefersExplicitSelectionOverOlderHistory(t *testing.T) {
	t.Parallel()
	sel := session.NewSelection()
	sel.AddFile("chosen.go", strings.Repeat("z", 400))

	var history []provider.Message
	for i := range 40 {
		history = append(history, userTurn(fmt.Sprintf("turn %02d %s", i, strings.Repeat("h", 100))))
	}
	cm := session.NewContextManager(session.Options{Budget: 700, Reserve: 0, MinRecentMessages: 1, SummaryBudget: -1})

	asm, err := cm.Build(context.Background(), session.Input{
		SystemPrompt: "sys", Selection: sel, History: history,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(asm.DroppedFiles) != 0 {
		t.Fatalf("the explicitly selected file was dropped: %v", asm.DroppedFiles)
	}
	if !strings.Contains(asm.Messages[0].Content, "chosen.go") {
		t.Error("chosen.go is not in the system message")
	}
	if asm.EvictedMessages == 0 {
		t.Error("older history should have been evicted to make room")
	}
}

func TestBuildFailsWhenMandatoryContextCannotFit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		opts  session.Options
		input session.Input
	}{
		{
			"system prompt alone is too big",
			session.Options{Budget: 20, Reserve: 0},
			session.Input{SystemPrompt: strings.Repeat("s", 4000)},
		},
		{
			"newest turn is too big",
			session.Options{Budget: 40, Reserve: 0, MinRecentMessages: 1},
			session.Input{SystemPrompt: "sys", History: []provider.Message{userTurn(strings.Repeat("u", 4000))}},
		},
		{
			"reserve consumes the whole budget",
			session.Options{Budget: 100, Reserve: 100},
			session.Input{SystemPrompt: "sys"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cm := session.NewContextManager(tc.opts)
			if _, err := cm.Build(context.Background(), tc.input); !errors.Is(err, session.ErrBudgetTooSmall) {
				t.Errorf("err = %v, want ErrBudgetTooSmall", err)
			}
		})
	}
}

func TestBuildShrinksTheGuaranteedTailBeforeFailing(t *testing.T) {
	t.Parallel()
	// The newest turn fits, but the two newest do not: the tail shrinks rather
	// than the build failing.
	cm := session.NewContextManager(session.Options{Budget: 200, Reserve: 0, MinRecentMessages: 2, SummaryBudget: -1})
	history := []provider.Message{
		userTurn(strings.Repeat("a", 2000)),
		userTurn("the newest turn"),
	}
	asm, err := cm.Build(context.Background(), session.Input{SystemPrompt: "sys", History: history})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if asm.EvictedMessages != 1 {
		t.Errorf("EvictedMessages = %d, want 1", asm.EvictedMessages)
	}
	last := asm.Messages[len(asm.Messages)-1]
	if last.Content != "the newest turn" {
		t.Errorf("last message = %q", truncate(last.Content))
	}
}

func TestBuildDropsOrphanedToolResults(t *testing.T) {
	t.Parallel()
	// The retained window would start on a tool result whose requesting
	// assistant turn was evicted; providers reject that, so it is dropped.
	history := []provider.Message{
		userTurn(strings.Repeat("q", 4000)),
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "c1", Name: "run", Arguments: strings.Repeat("{", 2000)}}},
		{Role: provider.RoleTool, ToolCallID: "c1", Content: "output"},
		userTurn("what happened?"),
	}
	cm := session.NewContextManager(session.Options{Budget: 300, Reserve: 0, MinRecentMessages: 2, SummaryBudget: -1})
	asm, err := cm.Build(context.Background(), session.Input{SystemPrompt: "sys", History: history})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if asm.OrphanedToolMessages != 1 {
		t.Errorf("OrphanedToolMessages = %d, want 1", asm.OrphanedToolMessages)
	}
	for _, msg := range asm.Messages {
		if msg.Role == provider.RoleTool {
			t.Errorf("an orphaned tool result was sent: %+v", msg)
		}
	}
	if asm.EvictedMessages != 3 {
		t.Errorf("EvictedMessages = %d, want 3", asm.EvictedMessages)
	}
}

func TestBuildWithoutHistoryOrSystemPrompt(t *testing.T) {
	t.Parallel()
	cm := session.NewContextManager(session.Options{Budget: 1000, Reserve: 0})
	asm, err := cm.Build(context.Background(), session.Input{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(asm.Messages) != 0 || asm.HasSystem {
		t.Errorf("empty input produced %+v", asm)
	}
	if asm.Tokens != 0 {
		t.Errorf("Tokens = %d, want 0", asm.Tokens)
	}
}

func TestBuildUsesTheSuppliedTokenCounter(t *testing.T) {
	t.Parallel()
	counter := &wordCounter{}
	cm := session.NewContextManager(session.Options{Budget: 1000, Reserve: 0, Counter: counter})
	if _, err := cm.Build(context.Background(), session.Input{
		SystemPrompt: "one two three", History: []provider.Message{userTurn("four five")},
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if counter.calls == 0 {
		t.Fatal("the substituted TokenCounter was never called")
	}

	// The same input assembled under two counters must give different budgets
	// worth of history, proving the substitution actually drives decisions.
	var history []provider.Message
	for i := range 40 {
		history = append(history, userTurn(fmt.Sprintf("turn %02d has exactly some words here", i)))
	}
	input := session.Input{SystemPrompt: "sys", History: history}

	heuristic, err := session.NewContextManager(session.Options{Budget: 300, Reserve: 0, SummaryBudget: -1}).
		Build(context.Background(), input)
	if err != nil {
		t.Fatalf("Build with the heuristic counter: %v", err)
	}
	words, err := session.NewContextManager(session.Options{Budget: 300, Reserve: 0, SummaryBudget: -1, Counter: &wordCounter{}}).
		Build(context.Background(), input)
	if err != nil {
		t.Fatalf("Build with the word counter: %v", err)
	}
	if len(words.Messages) <= len(heuristic.Messages) {
		t.Errorf("word counter kept %d messages, heuristic kept %d; a cheaper counter should keep more",
			len(words.Messages), len(heuristic.Messages))
	}
}

func TestSelection(t *testing.T) {
	t.Parallel()
	sel := session.NewSelection()
	if !sel.IsEmpty() {
		t.Error("a new selection should be empty")
	}

	sel.AddFile("a.go", "first")
	sel.AddFile("b.go", "second")
	sel.AddFile("a.go", "updated")
	files := sel.Files()
	if len(files) != 2 {
		t.Fatalf("len = %d, want 2 (re-adding a path must replace, not duplicate)", len(files))
	}
	if files[0].Path != "a.go" || files[0].Content != "updated" {
		t.Errorf("files[0] = %+v, want a.go/updated in its original position", files[0])
	}

	// Files() must hand back a copy.
	files[0].Content = "mutated"
	if sel.Files()[0].Content != "updated" {
		t.Error("Files() exposed internal state")
	}

	if !sel.RemoveFile("a.go") {
		t.Error("RemoveFile(a.go) = false, want true")
	}
	if sel.RemoveFile("a.go") {
		t.Error("RemoveFile of an absent path = true, want false")
	}

	base := time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		sel.AddToolResult("run", fmt.Sprint(i), base.Add(time.Duration(i)*time.Minute))
	}
	sel.AddToolResult("read", "stamped", time.Time{})
	results := sel.ToolResults()
	if len(results) != 6 {
		t.Fatalf("len = %d, want 6", len(results))
	}
	if results[5].At.IsZero() {
		t.Error("a zero timestamp should be stamped with now")
	}

	sel.TrimToolResults(2)
	results = sel.ToolResults()
	if len(results) != 2 || results[0].Content != "4" {
		t.Errorf("after TrimToolResults(2): %+v", results)
	}
	sel.TrimToolResults(0)
	if len(sel.ToolResults()) != 0 {
		t.Error("TrimToolResults(0) should discard everything")
	}

	sel.AddFile("c.go", "x")
	sel.Clear()
	if !sel.IsEmpty() {
		t.Error("Clear did not empty the selection")
	}
}

func TestNilSelectionIsUsable(t *testing.T) {
	t.Parallel()
	var sel *session.Selection
	sel.AddFile("a.go", "x")
	sel.AddToolResult("run", "x", time.Now())
	sel.TrimToolResults(1)
	sel.Clear()
	if !sel.IsEmpty() || sel.Files() != nil || sel.ToolResults() != nil || sel.RemoveFile("a.go") {
		t.Error("a nil Selection should behave as empty")
	}

	cm := session.NewContextManager(session.Options{Budget: 1000, Reserve: 0})
	if _, err := cm.Build(context.Background(), session.Input{SystemPrompt: "sys", Selection: nil}); err != nil {
		t.Fatalf("Build with a nil selection: %v", err)
	}
}

func TestBuildIntegratesWithStoredHistory(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	ctx := context.Background()
	id := seedTranscript(t, m,
		userTurn("first question"),
		assistantTurn("first answer"),
		userTurn("second question"),
	)
	history, err := m.History().Messages(ctx, id, session.TranscriptOptions{})
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}

	cm := session.NewContextManager(session.Options{Budget: 4000, Reserve: 0})
	asm, err := cm.Build(ctx, session.Input{SystemPrompt: "You are Boop.", History: history})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(asm.Messages) != 4 {
		t.Fatalf("messages = %d, want 4", len(asm.Messages))
	}
	if asm.Messages[len(asm.Messages)-1].Content != "second question" {
		t.Errorf("last message = %q", asm.Messages[len(asm.Messages)-1].Content)
	}
}

// truncate shortens a string for readable failure output.
func truncate(s string) string {
	if len(s) <= 60 {
		return s
	}
	return s[:60] + "…"
}
