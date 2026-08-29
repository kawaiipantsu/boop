package session

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/boop-dev/boop/internal/provider"
)

// ErrBudgetTooSmall reports that the mandatory part of the context — the
// system prompt and the newest turn — does not fit in the configured budget.
//
// It is an error rather than a silent truncation: quietly dropping the system
// prompt would change how the model behaves without anyone being told.
var ErrBudgetTooSmall = errors.New("session: context budget too small")

// Defaults for ContextManager. They are conservative on purpose: a local model
// with a small window is the primary target (§2.1), not a 200k cloud context.
const (
	// DefaultBudget is the total prompt token budget when none is configured.
	DefaultBudget = 8192
	// DefaultMinRecentMessages is the number of newest turns that are never
	// evicted, so the model always sees what was just said.
	DefaultMinRecentMessages = 2
	// DefaultSummaryBudget caps the summary that replaces evicted turns.
	DefaultSummaryBudget = 256
	// DefaultCharsPerToken is the heuristic estimator's ratio.
	DefaultCharsPerToken = 4.0
)

const (
	// messageOverheadTokens approximates the per-message framing every chat
	// API adds (role markers, delimiters).
	messageOverheadTokens = 4
	// sectionOverheadTokens approximates the heading and fences this package
	// writes around each context section.
	sectionOverheadTokens = 8
	// binaryPartTokens is the flat charge for a non-text content part. Real
	// image cost is provider- and resolution-specific; this is a placeholder
	// large enough that images are not treated as free.
	binaryPartTokens = 1024
)

// TokenCounter estimates how many tokens a piece of text will occupy.
//
// It is an interface so a real tokenizer can be substituted per provider
// without touching the context manager.
type TokenCounter interface {
	// CountText returns the estimated token count of s. Implementations must
	// be monotonic — a longer prefix never counts fewer tokens — because
	// truncation binary-searches against this.
	CountText(s string) int
}

// HeuristicCounter estimates tokens as characters divided by CharsPerToken.
//
// This is an ESTIMATE, not a tokenizer. Real tokenization is model-specific and
// would mean shipping vocabulary tables for every backend Boop talks to, which
// contradicts the dependency budget (§40). The estimate is used only to decide
// what to include before a request; providers report true usage afterwards in
// provider.Usage, and that is what gets persisted and displayed (§28). Expect
// it to be within roughly ±20% for English prose and to be least accurate for
// dense code, minified data and non-Latin scripts.
type HeuristicCounter struct {
	// CharsPerToken defaults to DefaultCharsPerToken when zero or negative.
	CharsPerToken float64
}

// CountText implements TokenCounter.
func (c HeuristicCounter) CountText(s string) int {
	if s == "" {
		return 0
	}
	ratio := c.CharsPerToken
	if ratio <= 0 {
		ratio = DefaultCharsPerToken
	}
	n := int(math.Ceil(float64(utf8.RuneCountInString(s)) / ratio))
	if n < 1 {
		n = 1
	}
	return n
}

// DefaultTokenCounter returns the heuristic estimator used when none is set.
func DefaultTokenCounter() TokenCounter { return HeuristicCounter{CharsPerToken: DefaultCharsPerToken} }

// CountMessage estimates the token cost of one message, including its framing,
// tool calls and non-text parts.
func CountMessage(c TokenCounter, msg provider.Message) int {
	if c == nil {
		c = DefaultTokenCounter()
	}
	n := messageOverheadTokens
	n += c.CountText(string(msg.Role))
	n += c.CountText(msg.Content)
	n += c.CountText(msg.Name)
	n += c.CountText(msg.ToolCallID)
	for _, part := range msg.Parts {
		n += c.CountText(part.Text)
		if len(part.Data) > 0 {
			n += binaryPartTokens
		}
	}
	for _, call := range msg.ToolCalls {
		n += c.CountText(call.Name) + c.CountText(call.Arguments)
	}
	return n
}

// CountMessages estimates the token cost of a slice of messages.
func CountMessages(c TokenCounter, msgs []provider.Message) int {
	total := 0
	for _, msg := range msgs {
		total += CountMessage(c, msg)
	}
	return total
}

// ContextFile is a file the user or the runtime explicitly put into context.
type ContextFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// ToolOutput is a tool or command result explicitly kept in context, such as
// the output of the last failing test run.
type ToolOutput struct {
	Tool    string    `json:"tool"`
	Content string    `json:"content"`
	At      time.Time `json:"at"`
}

// Selection is the explicitly chosen context (§47).
//
// Explicit selection is the whole point: Boop never scans a repository into the
// prompt. Something is in context because a user command, or a deliberate
// runtime decision, put it here. Selection backs `/context add` and
// `/context clear`. It is safe for concurrent use.
type Selection struct {
	mu      sync.RWMutex
	files   []ContextFile
	results []ToolOutput
}

// NewSelection returns an empty selection.
func NewSelection() *Selection { return &Selection{} }

// AddFile adds or replaces a file in the selection, preserving insertion order
// for a replacement so the user's ordering is stable.
func (s *Selection) AddFile(path, content string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.files {
		if s.files[i].Path == path {
			s.files[i].Content = content
			return
		}
	}
	s.files = append(s.files, ContextFile{Path: path, Content: content})
}

// RemoveFile drops a file from the selection, reporting whether it was present.
func (s *Selection) RemoveFile(path string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.files {
		if s.files[i].Path == path {
			s.files = append(s.files[:i], s.files[i+1:]...)
			return true
		}
	}
	return false
}

// Files returns the selected files in selection order.
func (s *Selection) Files() []ContextFile {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ContextFile, len(s.files))
	copy(out, s.files)
	return out
}

// AddToolResult appends a tool result, oldest first. A zero At is stamped now.
func (s *Selection) AddToolResult(tool, content string, at time.Time) {
	if s == nil {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results = append(s.results, ToolOutput{Tool: tool, Content: content, At: at})
}

// ToolResults returns the selected tool results, oldest first.
func (s *Selection) ToolResults() []ToolOutput {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ToolOutput, len(s.results))
	copy(out, s.results)
	return out
}

// TrimToolResults keeps only the n newest tool results, discarding the rest.
func (s *Selection) TrimToolResults(n int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if n <= 0 {
		s.results = nil
		return
	}
	if len(s.results) > n {
		s.results = append([]ToolOutput(nil), s.results[len(s.results)-n:]...)
	}
}

// Clear empties the selection, backing `/context clear`.
func (s *Selection) Clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files, s.results = nil, nil
}

// IsEmpty reports whether nothing is selected.
func (s *Selection) IsEmpty() bool {
	if s == nil {
		return true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.files) == 0 && len(s.results) == 0
}

// Summarizer condenses evicted turns so the model still knows they happened.
//
// It is an interface so a model-backed summarizer can replace the default
// offline one; Build passes its context through so a network call can be
// cancelled.
type Summarizer interface {
	Summarize(ctx context.Context, evicted []provider.Message) (string, error)
}

// OutlineSummarizer is the default summarizer: deterministic, offline and free.
//
// It does not call a model. A model-backed summary is better prose but costs a
// request on every overflow, so the honest default is a compact outline that
// tells the model what was dropped and lets it ask if it needs more.
type OutlineSummarizer struct {
	// MaxRunesPerMessage caps each outline line; zero applies 120.
	MaxRunesPerMessage int
	// MaxMessages caps how many turns are listed; zero applies 12, keeping the
	// newest of the evicted set.
	MaxMessages int
}

// Summarize implements Summarizer.
func (s OutlineSummarizer) Summarize(_ context.Context, evicted []provider.Message) (string, error) {
	if len(evicted) == 0 {
		return "", nil
	}
	perMessage := s.MaxRunesPerMessage
	if perMessage <= 0 {
		perMessage = 120
	}
	maxMessages := s.MaxMessages
	if maxMessages <= 0 {
		maxMessages = 12
	}
	listed := evicted
	if len(listed) > maxMessages {
		listed = listed[len(listed)-maxMessages:]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d earlier turns were omitted to stay within the context budget.", len(evicted))
	if len(listed) < len(evicted) {
		fmt.Fprintf(&b, " The most recent %d of them:", len(listed))
	} else {
		b.WriteString(" They were:")
	}
	for _, msg := range listed {
		b.WriteString("\n- ")
		b.WriteString(string(msg.Role))
		b.WriteString(": ")
		b.WriteString(outlineText(msg, perMessage))
	}
	return b.String(), nil
}

// outlineText renders a one-line gist of a message.
func outlineText(msg provider.Message, maxRunes int) string {
	text := strings.TrimSpace(msg.Content)
	if text == "" {
		for _, part := range msg.Parts {
			if strings.TrimSpace(part.Text) != "" {
				text = strings.TrimSpace(part.Text)
				break
			}
		}
	}
	if text == "" && len(msg.ToolCalls) > 0 {
		names := make([]string, len(msg.ToolCalls))
		for i, call := range msg.ToolCalls {
			names[i] = call.Name
		}
		text = "called " + strings.Join(names, ", ")
	}
	if text == "" {
		text = "(no text)"
	}
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "…"
	}
	return text
}

// Options configures a ContextManager.
type Options struct {
	// Budget is the token budget for the prompt. Zero applies DefaultBudget.
	//
	// Either set Budget to the model's context window and Reserve to the
	// answer allowance, or set Budget to the window minus the answer and
	// leave Reserve at zero. Both are valid; the pair must not double-count.
	Budget int
	// Reserve is held back from Budget for the model's answer. Zero, the
	// default, holds nothing back — see Budget.
	Reserve int
	// MinRecentMessages is the number of newest turns that are never evicted.
	// Zero applies DefaultMinRecentMessages.
	MinRecentMessages int
	// SummaryBudget caps the summary of evicted turns. Zero applies
	// DefaultSummaryBudget; a negative value disables summarization.
	SummaryBudget int
	// Counter estimates token cost. Nil applies DefaultTokenCounter.
	Counter TokenCounter
	// Summarizer condenses evicted turns. Nil applies OutlineSummarizer.
	Summarizer Summarizer
}

// ContextManager assembles the prompt that is actually sent to a model (§47).
//
// It never blind-sends a repository or a whole session. Everything it includes
// is either mandatory (the system prompt, the newest turns) or explicitly
// selected, and anything that does not fit is evicted in a documented order
// with the eviction reported back to the caller so a UI can say so.
type ContextManager struct {
	opts Options
}

// NewContextManager returns a manager with defaults applied to opts.
func NewContextManager(opts Options) *ContextManager {
	if opts.Budget <= 0 {
		opts.Budget = DefaultBudget
	}
	if opts.Reserve < 0 {
		opts.Reserve = 0
	}
	if opts.MinRecentMessages == 0 {
		opts.MinRecentMessages = DefaultMinRecentMessages
	}
	if opts.MinRecentMessages < 0 {
		opts.MinRecentMessages = 0
	}
	if opts.SummaryBudget == 0 {
		opts.SummaryBudget = DefaultSummaryBudget
	}
	if opts.SummaryBudget < 0 {
		opts.SummaryBudget = 0
	}
	if opts.Counter == nil {
		opts.Counter = DefaultTokenCounter()
	}
	if opts.Summarizer == nil {
		opts.Summarizer = OutlineSummarizer{}
	}
	return &ContextManager{opts: opts}
}

// Options returns the effective configuration, defaults applied.
func (cm *ContextManager) Options() Options { return cm.opts }

// Input is everything the context manager may draw on.
type Input struct {
	// SystemPrompt is mandatory context and is never evicted or truncated.
	SystemPrompt string
	// ProjectMemory is the Boop.md content: compressed, durable project
	// knowledge (§16). It is a summary already, so it is included whole or not
	// at all.
	ProjectMemory string
	// Selection is the explicitly chosen files and tool output. May be nil.
	Selection *Selection
	// History is the conversation so far, oldest first.
	History []provider.Message
}

// Assembly is the result of building a context.
type Assembly struct {
	// Messages is the prompt, ready to place in a provider.ChatRequest. When
	// HasSystem is true, Messages[0] is the assembled system message.
	Messages  []provider.Message `json:"messages"`
	HasSystem bool               `json:"has_system"`
	// Tokens is the estimated cost of Messages; Budget is what was available
	// after the response reserve.
	Tokens int `json:"tokens"`
	Budget int `json:"budget"`
	// EvictedMessages counts history turns left out of the prompt.
	EvictedMessages int `json:"evicted_messages"`
	// Summary is the note that replaced the evicted turns, empty when nothing
	// was evicted or summarization is disabled.
	Summary string `json:"summary,omitempty"`
	// ProjectMemoryIncluded reports whether Boop.md fitted.
	ProjectMemoryIncluded bool `json:"project_memory_included"`
	// DroppedFiles names explicitly selected files that did not fit, so the UI
	// can tell the user their selection was not honoured in full.
	DroppedFiles []string `json:"dropped_files,omitempty"`
	// DroppedToolResults counts selected tool results that did not fit.
	DroppedToolResults int `json:"dropped_tool_results,omitempty"`
	// OrphanedToolMessages counts tool-result turns dropped because the
	// assistant turn that requested them was evicted.
	OrphanedToolMessages int `json:"orphaned_tool_messages,omitempty"`
}

// WithinBudget reports whether the assembly fits its budget.
func (a Assembly) WithinBudget() bool { return a.Tokens <= a.Budget }

// Build assembles the prompt for one request.
//
// Inclusion order, highest priority first:
//
//  1. the system prompt — mandatory, never evicted;
//  2. the newest MinRecentMessages turns — mandatory;
//  3. a summary of anything evicted, within SummaryBudget;
//  4. Boop.md project memory;
//  5. explicitly selected files, in selection order;
//  6. explicitly selected tool output, newest first;
//  7. older history, newest first, until the budget runs out.
//
// Explicit selection outranks older history on purpose: the user chose those
// files for this request, whereas older turns are already represented by the
// summary. Build returns ErrBudgetTooSmall when 1 and 2 alone do not fit.
func (cm *ContextManager) Build(ctx context.Context, in Input) (Assembly, error) {
	effective := cm.opts.Budget - cm.opts.Reserve
	if effective <= 0 {
		return Assembly{}, fmt.Errorf("budget %d reserves %d for the response, leaving nothing: %w",
			cm.opts.Budget, cm.opts.Reserve, ErrBudgetTooSmall)
	}
	// First pass assumes nothing is evicted, which is the common case and
	// costs no summary allowance.
	asm, err := cm.assemble(ctx, in, effective, 0)
	if err != nil {
		return Assembly{}, err
	}
	if asm.EvictedMessages == 0 || cm.opts.SummaryBudget == 0 {
		return asm, nil
	}
	// Second pass makes room for a summary of what the first pass evicted.
	allowance := min(cm.opts.SummaryBudget, effective/4)
	if allowance <= 0 {
		return asm, nil
	}
	withSummary, err := cm.assemble(ctx, in, effective, allowance)
	if err != nil {
		// Reserving for a summary made the mandatory part not fit; the
		// summary is a nicety, the first pass is still correct.
		return asm, nil
	}
	return withSummary, nil
}

// assemble builds one candidate context. summaryAllowance is the token budget
// set aside for a summary of evicted turns; zero disables it.
func (cm *ContextManager) assemble(ctx context.Context, in Input, effective, summaryAllowance int) (Assembly, error) {
	counter := cm.opts.Counter
	asm := Assembly{Budget: effective}

	// 1. The system prompt is mandatory.
	systemCost := messageOverheadTokens + counter.CountText(in.SystemPrompt)
	if systemCost > effective {
		return Assembly{}, fmt.Errorf("system prompt needs about %d tokens of the %d available: %w",
			systemCost, effective, ErrBudgetTooSmall)
	}
	remaining := effective - systemCost

	// 2. The newest turns are mandatory. Shrink the guaranteed tail only if it
	// genuinely cannot fit, and fail rather than send a prompt without the
	// most recent turn.
	history := in.History
	cut := max(len(history)-cm.opts.MinRecentMessages, 0)
	for cut < len(history) && CountMessages(counter, history[cut:]) > remaining {
		cut++
	}
	if len(history) > 0 && cut >= len(history) && cm.opts.MinRecentMessages > 0 {
		return Assembly{}, fmt.Errorf("the newest turn needs about %d tokens of the %d available: %w",
			CountMessage(counter, history[len(history)-1]), remaining, ErrBudgetTooSmall)
	}
	remaining -= CountMessages(counter, history[cut:])

	// 3. Hold back room for the summary before optional context competes for it.
	if summaryAllowance > remaining {
		summaryAllowance = remaining
	}
	remaining -= summaryAllowance

	// 4. Project memory: included whole or not at all, because Boop.md is
	// already a compressed summary and half of one is misleading.
	memorySection := section{}
	if body := strings.TrimSpace(in.ProjectMemory); body != "" {
		cost := sectionOverheadTokens + counter.CountText(body)
		if cost <= remaining {
			remaining -= cost
			memorySection = section{heading: "Project memory (Boop.md)", body: body}
			asm.ProjectMemoryIncluded = true
		}
	}

	// 5. Explicitly selected files, in the order the user selected them.
	var fileSections []section
	for _, file := range in.Selection.Files() {
		body := renderFile(file)
		cost := sectionOverheadTokens + counter.CountText(body)
		if cost > remaining {
			asm.DroppedFiles = append(asm.DroppedFiles, file.Path)
			continue
		}
		remaining -= cost
		fileSections = append(fileSections, section{heading: file.Path, body: body})
	}

	// 6. Selected tool output, newest first: a stale command result is the
	// first thing worth losing.
	results := in.Selection.ToolResults()
	var resultSections []section
	for i := len(results) - 1; i >= 0; i-- {
		body := renderToolOutput(results[i])
		cost := sectionOverheadTokens + counter.CountText(body)
		if cost > remaining {
			asm.DroppedToolResults++
			continue
		}
		remaining -= cost
		resultSections = append(resultSections, section{heading: results[i].Tool, body: body})
	}
	reverseSlice(resultSections)

	// 7. Older history, newest first, with whatever is left.
	for cut > 0 {
		cost := CountMessage(counter, history[cut-1])
		if cost > remaining {
			break
		}
		remaining -= cost
		cut--
	}

	// A retained window must not begin with a tool result whose requesting
	// assistant turn was evicted: providers reject the orphan, so drop it.
	for cut < len(history) && history[cut].Role == provider.RoleTool {
		cut++
		asm.OrphanedToolMessages++
	}

	asm.EvictedMessages = cut

	// 8. Summarize what was evicted, trimmed to the allowance held back above.
	var summarySection section
	if cut > 0 && summaryAllowance > 0 {
		summary, err := cm.opts.Summarizer.Summarize(ctx, history[:cut])
		if err != nil {
			// A failed summary must not fail the request; fall back to a note
			// that at least tells the model context is missing.
			summary = fmt.Sprintf("%d earlier turns were omitted to stay within the context budget.", cut)
		}
		summary = truncateToTokens(counter, strings.TrimSpace(summary), summaryAllowance-sectionOverheadTokens)
		if summary != "" {
			asm.Summary = summary
			summarySection = section{heading: "Earlier conversation (summarized)", body: summary}
		}
	}

	// 9. Emit a single system message. One message rather than several keeps
	// the prompt valid across providers that accept only one system turn.
	sections := make([]section, 0, len(fileSections)+len(resultSections)+2)
	if memorySection.heading != "" {
		sections = append(sections, memorySection)
	}
	if len(fileSections) > 0 {
		sections = append(sections, section{heading: "Selected files", children: fileSections})
	}
	if len(resultSections) > 0 {
		sections = append(sections, section{heading: "Recent tool output", children: resultSections})
	}
	if summarySection.heading != "" {
		sections = append(sections, summarySection)
	}

	messages := make([]provider.Message, 0, len(history)-cut+1)
	if systemText := renderSystem(in.SystemPrompt, sections); systemText != "" {
		messages = append(messages, provider.Message{Role: provider.RoleSystem, Content: systemText})
		asm.HasSystem = true
	}
	messages = append(messages, history[cut:]...)

	asm.Messages = messages
	asm.Tokens = CountMessages(counter, messages)
	return asm, nil
}

// section is one labelled block of the assembled system message.
type section struct {
	heading  string
	body     string
	children []section
}

// renderSystem assembles the system prompt and its context sections into one
// message. Headings are markdown so the structure survives in a raw prompt.
func renderSystem(prompt string, sections []section) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(prompt))
	for _, sec := range sections {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("## ")
		b.WriteString(sec.heading)
		if sec.body != "" {
			b.WriteString("\n")
			b.WriteString(sec.body)
		}
		for _, child := range sec.children {
			b.WriteString("\n\n### ")
			b.WriteString(child.heading)
			b.WriteString("\n")
			b.WriteString(child.body)
		}
	}
	return b.String()
}

// renderFile renders a selected file as a fenced block.
func renderFile(file ContextFile) string {
	return "```\n" + strings.TrimRight(file.Content, "\n") + "\n```"
}

// renderToolOutput renders a selected tool result with its timestamp so the
// model can tell fresh output from stale.
func renderToolOutput(out ToolOutput) string {
	var b strings.Builder
	if !out.At.IsZero() {
		b.WriteString(out.At.UTC().Format(time.RFC3339))
		b.WriteString("\n")
	}
	b.WriteString("```\n")
	b.WriteString(strings.TrimRight(out.Content, "\n"))
	b.WriteString("\n```")
	return b.String()
}

// truncateToTokens shortens s until the counter says it fits in maxTokens,
// appending an explicit marker so a truncated block is never mistaken for a
// complete one. It relies on TokenCounter being monotonic.
func truncateToTokens(c TokenCounter, s string, maxTokens int) string {
	if s == "" || maxTokens <= 0 {
		return ""
	}
	if c.CountText(s) <= maxTokens {
		return s
	}
	const marker = "\n… [truncated]"
	budget := maxTokens - c.CountText(marker)
	if budget <= 0 {
		return ""
	}
	runes := []rune(s)
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if c.CountText(string(runes[:mid])) <= budget {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	if lo == 0 {
		return ""
	}
	return strings.TrimRight(string(runes[:lo]), " \n\t") + marker
}

// reverseSlice flips a slice in place.
func reverseSlice[T any](s []T) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
