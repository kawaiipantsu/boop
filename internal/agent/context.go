package agent

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kawaiipantsu/boop/internal/project"
	"github.com/kawaiipantsu/boop/internal/provider"
	"github.com/kawaiipantsu/boop/internal/tools"
)

// This file implements the single most important rule of the agent runtime
// (§10): a worker does not receive a copy of the main conversation. It receives
// its task, the requirements that bear on it, the relevant files, an explicit
// list of allowed tools, and the relevant project memory — nothing else.
//
// The tool restriction is enforced structurally rather than by instruction:
// RestrictTools builds a registry containing only the granted tools, so a
// worker asking for anything else is told the tool does not exist.

// FileRef points a worker at a file it needs, optionally with an excerpt.
type FileRef struct {
	// Path is workspace-relative.
	Path string `json:"path"`
	// Note explains why the file matters.
	Note string `json:"note,omitempty"`
	// Content is an optional inline excerpt. Leaving it empty is normal: the
	// worker has the read tool and reads what it needs, which keeps the
	// starting context small.
	Content string `json:"content,omitempty"`
}

// Brief is the complete, isolated context handed to one worker.
type Brief struct {
	// Objective is the wider goal, in one line, so the worker understands
	// where its task sits without seeing the conversation that produced it.
	Objective string `json:"objective,omitempty"`
	// Task is what this worker must do.
	Task string `json:"task"`
	// Requirements are the user requirements that bear on this task.
	Requirements []string `json:"requirements,omitempty"`
	// Files are the relevant files.
	Files []FileRef `json:"files,omitempty"`
	// Reads and Writes state the paths the task may touch, so the worker knows
	// where its boundary is and that other agents own the rest.
	Reads  []string `json:"reads,omitempty"`
	Writes []string `json:"writes,omitempty"`
	// AllowedTools is the explicit grant. It is also enforced in code.
	AllowedTools []string `json:"allowed_tools"`
	// Memory is the relevant slice of project memory, never the whole file.
	Memory string `json:"memory,omitempty"`
	// Validation states how completion is checked.
	Validation string `json:"validation,omitempty"`
	// Environment is the runtime detail (platform, workspace root, model).
	Environment string `json:"environment,omitempty"`
}

// Messages renders the brief as the worker's entire starting history.
//
// Two messages: the worker system prompt plus environment, and the brief. There
// is no third message carrying "what the user said earlier", by design.
func (b Brief) Messages(systemPrompt string) []provider.Message {
	system := strings.TrimSpace(systemPrompt)
	if system == "" {
		system = WorkerPrompt()
	}
	var sb strings.Builder
	sb.WriteString(system)
	if env := strings.TrimSpace(b.Environment); env != "" {
		sb.WriteString("\n\n## Environment\n\n")
		sb.WriteString(env)
	}
	if len(b.AllowedTools) > 0 {
		sb.WriteString("\n\n## Allowed tools\n\n")
		sb.WriteString(strings.Join(b.AllowedTools, ", "))
		sb.WriteString("\n\nNo other tool is available to you. Asking for one will fail.")
	} else {
		sb.WriteString("\n\n## Allowed tools\n\nNone. Answer from the context you were given.")
	}

	return []provider.Message{
		{Role: provider.RoleSystem, Content: sb.String()},
		{Role: provider.RoleUser, Content: b.Text()},
	}
}

// Text renders the brief as the worker's task message.
func (b Brief) Text() string {
	var sb strings.Builder
	if o := strings.TrimSpace(b.Objective); o != "" {
		fmt.Fprintf(&sb, "## Wider objective\n\n%s\n\n", o)
	}
	fmt.Fprintf(&sb, "## Your task\n\n%s\n", strings.TrimSpace(b.Task))

	writeList(&sb, "Requirements", b.Requirements)

	if len(b.Files) > 0 {
		sb.WriteString("\n## Relevant files\n\n")
		for _, f := range b.Files {
			if f.Note != "" {
				fmt.Fprintf(&sb, "- %s — %s\n", f.Path, f.Note)
			} else {
				fmt.Fprintf(&sb, "- %s\n", f.Path)
			}
		}
		for _, f := range b.Files {
			if strings.TrimSpace(f.Content) == "" {
				continue
			}
			fmt.Fprintf(&sb, "\n### %s\n\n```\n%s\n```\n", f.Path, strings.TrimRight(f.Content, "\n"))
		}
	}

	writeList(&sb, "You may read", b.Reads)

	if len(b.Writes) > 0 {
		writeList(&sb, "You may write", b.Writes)
		sb.WriteString("\nOther agents are working on other paths at the same time. Do not write anywhere else.\n")
	} else {
		sb.WriteString("\n## You may write\n\nNothing. This task is read-only; report what you found.\n")
	}

	if m := strings.TrimSpace(b.Memory); m != "" {
		fmt.Fprintf(&sb, "\n## Project memory\n\n%s\n", m)
	}
	if v := strings.TrimSpace(b.Validation); v != "" {
		fmt.Fprintf(&sb, "\n## How this task is validated\n\n%s\n", v)
	}
	return sb.String()
}

func writeList(sb *strings.Builder, heading string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(sb, "\n## %s\n\n", heading)
	for _, item := range items {
		if s := strings.TrimSpace(item); s != "" {
			fmt.Fprintf(sb, "- %s\n", s)
		}
	}
}

// MemorySource supplies the slice of project memory a task actually needs.
//
// It is an interface because §10 asks for *relevant* memory: the whole Boop.md
// is exactly the sort of blind bulk context this package exists to avoid.
type MemorySource interface {
	Relevant(task Task) string
}

// MemorySourceFunc adapts a function to MemorySource.
type MemorySourceFunc func(task Task) string

// Relevant implements MemorySource.
func (f MemorySourceFunc) Relevant(task Task) string { return f(task) }

// memorySections are the Boop.md sections a worker can act on. Session
// summaries and goals are conversation-shaped and deliberately excluded.
var memorySections = []string{
	project.SectionArchitecture,
	project.SectionImportantFiles,
	project.SectionDecisions,
	project.SectionKnownProblems,
	project.SectionUsefulCommands,
	project.SectionAgentNotes,
}

// ProjectMemory adapts a Boop.md document to MemorySource.
//
// It returns only the durable, actionable sections, and only those with
// content. Nil memory yields an empty string rather than a panic, because a
// project without a Boop.md is normal.
func ProjectMemory(m *project.Memory) MemorySource {
	return MemorySourceFunc(func(Task) string {
		if m == nil || m.Document() == nil {
			return ""
		}
		doc := m.Document()
		var sb strings.Builder
		for _, title := range memorySections {
			body := strings.TrimSpace(doc.Text(title))
			if body == "" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteString("\n\n")
			}
			fmt.Fprintf(&sb, "### %s\n\n%s", title, body)
		}
		return sb.String()
	})
}

// Read-only, mutating and command tool names, used to derive a default grant.
var (
	readOnlyTools = []string{"read", "list", "find", "search"}
	writeTools    = []string{"write", "edit"}
)

// DefaultAllowedTools derives a conservative tool grant from the task shape.
//
// Every worker can look around. Only a task that declared what it writes may
// write, and nothing gets to run commands unless the plan says so explicitly —
// an unattended agent that can run arbitrary commands is the one mistake this
// runtime cannot take back.
func DefaultAllowedTools(task Task) []string {
	allowed := append([]string(nil), readOnlyTools...)
	if len(task.writePaths()) > 0 {
		allowed = append(allowed, writeTools...)
	}
	sort.Strings(allowed)
	return allowed
}

// RestrictTools returns a registry holding only the named tools.
//
// This is what makes the allowlist real. The worker's loop is given the
// restricted registry, so the filtered set is both what the model is offered
// (Definitions) and what can possibly be dispatched (Get) — a worker cannot
// call a tool it was not granted even if it invents the name.
//
// A nil allowed slice grants nothing, which is the safe reading of "no tools
// were specified" for a worker. Names that are not registered are ignored.
func RestrictTools(src *tools.Registry, allowed []string) *tools.Registry {
	out := tools.NewRegistry()
	if src == nil {
		return out
	}
	for _, name := range allowed {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if t, ok := src.Get(name); ok {
			out.Register(t)
		}
	}
	return out
}

// Scope decides what each worker is allowed to see and use.
type Scope struct {
	// DefaultTools is the grant for tasks that do not name their own. Empty
	// falls back to DefaultAllowedTools.
	DefaultTools []string
	// Memory supplies relevant project memory. Nil supplies none.
	Memory MemorySource
	// Environment is the runtime detail every worker is told.
	Environment string
	// Files resolves a path to an inline excerpt. Nil sends paths only, which
	// is the cheap default: workers have the read tool.
	Files func(path string) (FileRef, bool)
	// MaxMemoryBytes truncates project memory. Zero uses
	// DefaultMaxMemoryBytes.
	MaxMemoryBytes int
}

// DefaultMaxMemoryBytes bounds the project memory pasted into a brief, so a
// large Boop.md cannot quietly become the bulk context §10 forbids.
const DefaultMaxMemoryBytes = 8000

// BriefRequest is what the coordinator knows when it prepares a worker.
type BriefRequest struct {
	Task Task
	// Objective is the wider goal the task belongs to.
	Objective string
	// Requirements apply to the whole run; task requirements are added to them.
	Requirements []string
}

// Brief assembles the isolated context for one task.
func (s *Scope) Brief(req BriefRequest) Brief {
	task := req.Task
	b := Brief{
		Objective:    strings.TrimSpace(req.Objective),
		Task:         task.Description,
		Requirements: mergeStrings(req.Requirements, task.Requirements),
		Reads:        normalizeAll(task.Reads),
		Writes:       normalizeAll(task.Writes),
		Validation:   task.Validation,
	}

	switch {
	case len(task.AllowedTools) > 0:
		b.AllowedTools = dedupe(task.AllowedTools)
	case s != nil && len(s.DefaultTools) > 0:
		b.AllowedTools = dedupe(s.DefaultTools)
	default:
		b.AllowedTools = DefaultAllowedTools(task)
	}

	if s == nil {
		return b
	}
	b.Environment = s.Environment

	// Files worth naming are the ones the task reads or writes.
	for _, p := range append(append([]string(nil), b.Reads...), b.Writes...) {
		ref := FileRef{Path: p}
		if s.Files != nil {
			if resolved, ok := s.Files(p); ok {
				ref = resolved
			}
		}
		b.Files = appendFile(b.Files, ref)
	}

	if s.Memory != nil {
		limit := s.MaxMemoryBytes
		if limit <= 0 {
			limit = DefaultMaxMemoryBytes
		}
		b.Memory = truncate(s.Memory.Relevant(task), limit)
	}
	return b
}

func appendFile(files []FileRef, ref FileRef) []FileRef {
	if ref.Path == "" {
		return files
	}
	for i, existing := range files {
		if existing.Path != ref.Path {
			continue
		}
		if existing.Content == "" && ref.Content != "" {
			files[i] = ref
		}
		return files
	}
	return append(files, ref)
}

func mergeStrings(lists ...[]string) []string {
	var all []string
	for _, list := range lists {
		all = append(all, list...)
	}
	return dedupe(all)
}

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeAll(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if n := NormalizePath(p); n != "" {
			out = append(out, n)
		}
	}
	return dedupe(out)
}

// truncate cuts text to at most limit bytes on a line boundary and says so.
func truncate(text string, limit int) string {
	text = strings.TrimSpace(text)
	if limit <= 0 || len(text) <= limit {
		return text
	}
	cut := text[:limit]
	if i := strings.LastIndex(cut, "\n"); i > limit/2 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut) + "\n\n[project memory truncated]"
}
