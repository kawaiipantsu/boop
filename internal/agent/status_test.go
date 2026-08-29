package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kawaiipantsu/boop/internal/provider"
)

func TestRunReportTally(t *testing.T) {
	tests := []struct {
		name                          string
		tasks                         []TaskResult
		ok                            int
		failed, blocked, cancelled    int
		wantTokens, wantCalls, wantOK bool
	}{
		{
			name: "all complete",
			tasks: []TaskResult{
				{TaskID: "a", Status: TaskComplete, Usage: provider.Usage{TotalTokens: 5}, ToolCalls: 1},
				{TaskID: "b", Status: TaskComplete, Usage: provider.Usage{TotalTokens: 7}, ToolCalls: 2},
			},
			ok: 2, wantOK: true,
		},
		{
			name: "mixed",
			tasks: []TaskResult{
				{TaskID: "a", Status: TaskComplete},
				{TaskID: "b", Status: TaskFailed},
				{TaskID: "c", Status: TaskBlocked},
				{TaskID: "d", Status: TaskCancelled},
			},
			ok: 1, failed: 1, blocked: 1, cancelled: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &RunReport{Tasks: tc.tasks}
			r.tally()
			if r.Succeeded != tc.ok || r.Failed != tc.failed || r.Blocked != tc.blocked || r.Cancelled != tc.cancelled {
				t.Errorf("counts = %d/%d/%d/%d, want %d/%d/%d/%d",
					r.Succeeded, r.Failed, r.Blocked, r.Cancelled, tc.ok, tc.failed, tc.blocked, tc.cancelled)
			}
			if r.OK() != tc.wantOK {
				t.Errorf("OK() = %v, want %v", r.OK(), tc.wantOK)
			}
		})
	}

	r := &RunReport{Tasks: []TaskResult{
		{Status: TaskComplete, Usage: provider.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3, CachedTokens: 1}, ToolCalls: 2},
		{Status: TaskComplete, Usage: provider.Usage{PromptTokens: 4, CompletionTokens: 5, TotalTokens: 9}, ToolCalls: 3},
	}}
	r.tally()
	if r.Usage.TotalTokens != 12 || r.Usage.PromptTokens != 5 || r.Usage.CachedTokens != 1 || r.ToolCalls != 5 {
		t.Errorf("usage = %+v, tool calls = %d", r.Usage, r.ToolCalls)
	}

	if (&RunReport{Error: "boom"}).OK() {
		t.Error("OK() = true with a run-level error")
	}
	var nilReport *RunReport
	if nilReport.OK() {
		t.Error("OK() on nil = true")
	}
	if !strings.Contains(nilReport.Summary(), "no agent run") {
		t.Error("Summary() on nil must not panic and should say nothing ran")
	}
}

func TestRunReportSummary(t *testing.T) {
	r := &RunReport{
		Objective: "add retry",
		Degraded:  true,
		Reason:    "planning failed",
		Tasks: []TaskResult{
			{TaskID: "a", Status: TaskComplete, Output: "wrote the code"},
			{TaskID: "b", Status: TaskFailed, Error: "tests did not pass"},
			{TaskID: "c", Status: TaskComplete, Output: "reviewed", Truncated: true},
		},
	}
	r.tally()
	got := r.Summary()
	for _, want := range []string{
		"add retry", "degraded", "planning failed",
		"a — complete", "wrote the code",
		"b — failed", "tests did not pass",
		"iteration limit",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary is missing %q:\n%s", want, got)
		}
	}
}

func TestAgentInfoLine(t *testing.T) {
	tests := []struct {
		name string
		info AgentInfo
		want []string
	}{
		{
			name: "named worker",
			info: AgentInfo{ID: "0123456789abcdef", Name: "implement", Status: StatusWorking, Task: "write the adapter"},
			want: []string{"01234567", "working", "implement", "write the adapter"},
		},
		{
			name: "failed worker",
			info: AgentInfo{ID: "abcdef0123", Name: "tests", Status: StatusError, Error: "compile error"},
			want: []string{"abcdef01", "error", "compile error"},
		},
		{
			name: "unnamed",
			info: AgentInfo{ID: "ff", Status: StatusIdle},
			want: []string{"ff", "idle"},
		},
		{
			name: "multiline task collapses",
			info: AgentInfo{ID: "aa", Name: "n", Status: StatusIdle, Task: "line one\nline two"},
			want: []string{"line one"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			line := tc.info.Line()
			if strings.Contains(line, "\n") {
				t.Errorf("Line() contains a newline: %q", line)
			}
			for _, want := range tc.want {
				if !strings.Contains(line, want) {
					t.Errorf("Line() = %q, want it to contain %q", line, want)
				}
			}
		})
	}
}

func TestSnapshotIsJSONSerialisable(t *testing.T) {
	snap := Snapshot{
		Enabled: true, Max: 5, MaxDepth: 3, MaxAgents: 20,
		Total: 1, Active: 1,
		Agents: []AgentInfo{{
			ID: "abc", Name: "w", Status: StatusWorking,
			StartedAt: time.Now(), Duration: time.Second,
		}},
		At: time.Now(),
	}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("Marshal() = %v", err)
	}
	var back Snapshot
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal() = %v", err)
	}
	if back.Max != 5 || len(back.Agents) != 1 || back.Agents[0].Status != StatusWorking {
		t.Errorf("round trip = %+v", back)
	}

	empty := Snapshot{Max: 5}
	if !strings.Contains(empty.String(), "no agents") {
		t.Errorf("String() = %q, want it to say there are none", empty.String())
	}
	if !strings.Contains(empty.Summary(), "agents off") {
		t.Errorf("Summary() = %q, want it to report the disabled state", empty.Summary())
	}
}

func TestRunReportIsJSONSerialisable(t *testing.T) {
	r := &RunReport{
		Objective: "o",
		Tasks:     []TaskResult{{TaskID: "a", Status: TaskComplete, Output: "x"}},
	}
	r.tally()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal() = %v", err)
	}
	if strings.Contains(string(data), `"Err"`) {
		t.Error("the underlying error value must not be serialised")
	}
	var back RunReport
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal() = %v", err)
	}
	if back.Succeeded != 1 {
		t.Errorf("Succeeded = %d, want 1", back.Succeeded)
	}
}

func TestTaskGraph(t *testing.T) {
	tasks := []Task{
		{ID: "inspect", Description: "inspect architecture"},
		{ID: "implement", Description: "implement provider", Dependencies: []string{"inspect"}, Writes: []string{"p.go"}},
		{ID: "tests", Description: "write tests", Dependencies: []string{"inspect"}},
		{ID: "review", Description: "review implementation", Dependencies: []string{"implement", "tests"}},
	}
	got := TaskGraph(tasks)
	for _, want := range []string{"├──", "└──", "inspect architecture", "after implement, tests", "writes p.go"} {
		if !strings.Contains(got, want) {
			t.Errorf("graph is missing %q:\n%s", want, got)
		}
	}
	if lines := strings.Count(got, "\n"); lines != len(tasks)-1 {
		t.Errorf("graph has %d newlines, want %d", lines, len(tasks)-1)
	}
	if TaskGraph(nil) != "no tasks" {
		t.Errorf("TaskGraph(nil) = %q", TaskGraph(nil))
	}
}

func TestFirstLine(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"one line", "one line"},
		{"  padded  ", "padded"},
		{"first\nsecond", "first …"},
		{strings.Repeat("x", 200), strings.Repeat("x", 120) + "…"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := firstLine(tc.in); got != tc.want {
			t.Errorf("firstLine(%.20q) = %.30q, want %.30q", tc.in, got, tc.want)
		}
	}
}
