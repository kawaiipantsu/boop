package agent

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kawaiipantsu/boop/internal/provider"
)

// This file is the reporting surface: everything here is JSON-serialisable and
// UI-independent, because `/agents`, the TUI header and the WebUI all render
// the same snapshot (§2.3).

// AgentInfo is a point-in-time copy of an agent.
type AgentInfo struct {
	ID         string        `json:"id"`
	Name       string        `json:"name,omitempty"`
	Task       string        `json:"task,omitempty"`
	Provider   string        `json:"provider,omitempty"`
	Model      string        `json:"model,omitempty"`
	Status     AgentStatus   `json:"status"`
	ParentID   string        `json:"parent_id,omitempty"`
	RootID     string        `json:"root_id,omitempty"`
	Depth      int           `json:"depth,omitempty"`
	StartedAt  time.Time     `json:"started_at"`
	FinishedAt time.Time     `json:"finished_at,omitempty"`
	Duration   time.Duration `json:"duration,omitempty"`
	Output     string        `json:"output,omitempty"`
	Error      string        `json:"error,omitempty"`
}

// ShortID is the abbreviated identifier shown in listings and accepted by
// `/agents stop <id>`.
func (a AgentInfo) ShortID() string { return shortID(a.ID) }

// Line renders one agent as a single row for `/agents list` and the TUI.
func (a AgentInfo) Line() string {
	name := a.Name
	if name == "" {
		name = a.ShortID()
	}
	line := fmt.Sprintf("%-8s  %-9s  %s", a.ShortID(), a.Status, name)
	if a.Task != "" && a.Task != name {
		line += ": " + firstLine(a.Task)
	}
	if a.Error != "" {
		line += "  — " + firstLine(a.Error)
	}
	return line
}

// Snapshot is the fleet state for `/agents`, the TUI header and the WebUI.
type Snapshot struct {
	// Enabled mirrors `/agents on|off`.
	Enabled bool `json:"enabled"`
	// Max is the concurrency limit set by `/agents max <int>`.
	Max int `json:"max"`
	// MaxDepth and MaxAgents are the recursion bounds (§11).
	MaxDepth  int `json:"max_depth"`
	MaxAgents int `json:"max_agents"`
	// Counts by lifecycle position.
	Total     int `json:"total"`
	Active    int `json:"active"`
	Idle      int `json:"idle"`
	Complete  int `json:"complete"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`
	// Agents lists the fleet, newest last.
	Agents []AgentInfo `json:"agents"`
	At     time.Time   `json:"at"`
}

// Summary renders the snapshot as the one line a TUI header can afford.
func (s Snapshot) Summary() string {
	state := "on"
	if !s.Enabled {
		state = "off"
	}
	return fmt.Sprintf("agents %s · %d/%d active · %d complete · %d failed",
		state, s.Active, s.Max, s.Complete, s.Failed)
}

// String renders the snapshot for `/agents list`.
func (s Snapshot) String() string {
	var sb strings.Builder
	sb.WriteString(s.Summary())
	if len(s.Agents) == 0 {
		sb.WriteString("\nno agents")
		return sb.String()
	}
	for _, a := range s.Agents {
		sb.WriteString("\n")
		sb.WriteString(a.Line())
	}
	return sb.String()
}

// RunReport aggregates one objective's execution.
//
// It is what the coordinator hands back to the main conversation: the caller
// asked one question and gets one answer, not a transcript of every worker.
type RunReport struct {
	Objective string `json:"objective,omitempty"`
	// Degraded and Reason report a planner that fell back to a single task.
	Degraded bool   `json:"degraded,omitempty"`
	Reason   string `json:"reason,omitempty"`

	Tasks  []TaskResult `json:"tasks"`
	Agents []AgentInfo  `json:"agents,omitempty"`

	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Blocked   int `json:"blocked"`
	Cancelled int `json:"cancelled"`

	Usage     provider.Usage `json:"usage"`
	ToolCalls int            `json:"tool_calls"`

	StartedAt  time.Time     `json:"started_at"`
	FinishedAt time.Time     `json:"finished_at"`
	Duration   time.Duration `json:"duration"`

	// Error is the run-level failure, such as a rejected graph or a
	// cancellation. Individual task failures are in Tasks.
	Error string `json:"error,omitempty"`
}

// tally recomputes the counters and totals from the task results.
func (r *RunReport) tally() {
	r.Succeeded, r.Failed, r.Blocked, r.Cancelled = 0, 0, 0, 0
	r.Usage = provider.Usage{}
	r.ToolCalls = 0
	for _, t := range r.Tasks {
		switch t.Status {
		case TaskComplete:
			r.Succeeded++
		case TaskFailed:
			r.Failed++
		case TaskBlocked:
			r.Blocked++
		case TaskCancelled:
			r.Cancelled++
		}
		r.Usage.PromptTokens += t.Usage.PromptTokens
		r.Usage.CompletionTokens += t.Usage.CompletionTokens
		r.Usage.TotalTokens += t.Usage.TotalTokens
		r.Usage.CachedTokens += t.Usage.CachedTokens
		r.ToolCalls += t.ToolCalls
	}
}

// OK reports whether every task completed.
func (r *RunReport) OK() bool {
	return r != nil && r.Error == "" && r.Failed == 0 && r.Blocked == 0 && r.Cancelled == 0
}

// Summary renders the aggregate result as the text fed back into the main
// conversation.
func (r *RunReport) Summary() string {
	if r == nil {
		return "no agent run"
	}
	var sb strings.Builder
	if r.Objective != "" {
		fmt.Fprintf(&sb, "Objective: %s\n", firstLine(r.Objective))
	}
	if r.Degraded && r.Reason != "" {
		fmt.Fprintf(&sb, "Planning degraded to a single task (%s).\n", r.Reason)
	}
	fmt.Fprintf(&sb, "%d task(s): %d complete, %d failed, %d blocked, %d cancelled.\n",
		len(r.Tasks), r.Succeeded, r.Failed, r.Blocked, r.Cancelled)
	if r.Error != "" {
		fmt.Fprintf(&sb, "Run error: %s\n", r.Error)
	}
	for _, t := range r.Tasks {
		fmt.Fprintf(&sb, "\n## %s — %s\n", t.TaskID, t.Status)
		if t.Error != "" {
			fmt.Fprintf(&sb, "%s\n", t.Error)
		}
		if out := strings.TrimSpace(t.Output); out != "" {
			fmt.Fprintf(&sb, "%s\n", out)
		}
		if t.Truncated {
			sb.WriteString("(the worker hit its iteration limit; this result may be incomplete)\n")
		}
	}
	return sb.String()
}

// TaskGraph renders the plan as the indented tree §11 shows, for `/agents` and
// for explaining a plan before it runs.
func TaskGraph(tasks []Task) string {
	if len(tasks) == 0 {
		return "no tasks"
	}
	byID := make(map[string]Task, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}
	var sb strings.Builder
	for i, t := range tasks {
		branch := "├──"
		if i == len(tasks)-1 {
			branch = "└──"
		}
		fmt.Fprintf(&sb, "%s %s: %s", branch, t.ID, firstLine(t.Description))
		if deps := dedupe(t.Dependencies); len(deps) > 0 {
			sorted := append([]string(nil), deps...)
			sort.Strings(sorted)
			fmt.Fprintf(&sb, " (after %s)", strings.Join(sorted, ", "))
		}
		if writes := t.writePaths(); len(writes) > 0 {
			fmt.Fprintf(&sb, " [writes %s]", strings.Join(writes, ", "))
		}
		if i < len(tasks)-1 {
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// firstLine keeps multi-line text out of single-line output.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i]) + " …"
	}
	const limit = 120
	if len(s) > limit {
		s = s[:limit] + "…"
	}
	return s
}
