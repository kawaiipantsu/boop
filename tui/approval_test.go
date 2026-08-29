package tui

import (
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/permissions"
)

func TestApprovalChoicesRespectSessionGrantRules(t *testing.T) {
	tests := []struct {
		name    string
		action  permissions.Action
		want    []string
		session bool
	}{
		{
			name:    "ordinary shell command",
			action:  permissions.Action{Tool: "run", Category: permissions.CatShellExecute, Risk: permissions.RiskLow},
			want:    []string{"Approve once", "Always for session", "Reject"},
			session: true,
		},
		{
			name:    "medium risk write",
			action:  permissions.Action{Tool: "write", Category: permissions.CatFilesystemWrite, Risk: permissions.RiskMedium},
			want:    []string{"Approve once", "Always for session", "Reject"},
			session: true,
		},
		{
			name:   "critical risk cannot be remembered",
			action: permissions.Action{Tool: "run", Category: permissions.CatShellExecute, Risk: permissions.RiskCritical},
			want:   []string{"Approve once", "Reject"},
		},
		{
			name: "production cannot be remembered",
			action: permissions.Action{Tool: "run", Category: permissions.CatProductionChange,
				Risk: permissions.RiskHigh, Production: true},
			want: []string{"Approve once", "Reject"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			choices := approvalChoices(tc.action)
			got := make([]string, len(choices))
			for i, c := range choices {
				got[i] = c.Label
			}
			if !equalStrings(got, tc.want) {
				t.Fatalf("choices = %q, want %q", got, tc.want)
			}
			// The UI must never offer a scope the engine would refuse.
			for _, c := range choices {
				if c.Scope != permissions.ScopeOnce && !permissions.CanGrantSession(tc.action) {
					t.Fatalf("offered scope %q for an action that cannot be remembered", c.Scope)
				}
			}
			if tc.session != permissions.CanGrantSession(tc.action) {
				t.Fatalf("test expectation disagrees with permissions.CanGrantSession")
			}
		})
	}
}

func TestApprovalPromptShowsEverythingNeededToJudge(t *testing.T) {
	prompt := newApprovalPrompt(permissions.PendingApproval{
		ID: "p1",
		Action: permissions.Action{
			Tool: "run", Category: permissions.CatShellExecute, Risk: permissions.RiskHigh,
			Summary: "run a shell command", Detail: "rm -rf build", Paths: []string{"/tmp/x"},
		},
		Decision: permissions.Decision{Outcome: permissions.OutcomeConfirm, Reason: "shell execution needs confirmation"},
	})
	text := promptText(prompt, 70)
	for _, want := range []string{
		"APPROVAL REQUIRED", "run a shell command", "rm -rf build",
		"/tmp/x", "risk: HIGH", "shell.execute", "shell execution needs confirmation",
		"Approve once", "Always for session", "Reject",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("prompt is missing %q:\n%s", want, text)
		}
	}
}

func TestApprovalPromptMarksProduction(t *testing.T) {
	prompt := newApprovalPrompt(permissions.PendingApproval{
		ID: "p2",
		Action: permissions.Action{
			Tool: "run", Category: permissions.CatProductionChange, Risk: permissions.RiskCritical,
			Summary: "deploy to production", Detail: "terraform apply", Production: true,
		},
	})
	lines := prompt.Lines(70)
	if lines[0].Style != promptProduction {
		t.Fatalf("production prompt did not use the production style: %+v", lines[0])
	}
	text := promptText(prompt, 70)
	if !strings.Contains(text, "PRODUCTION CHANGE") {
		t.Fatalf("production is not called out:\n%s", text)
	}
	if strings.Contains(text, "Always for session") {
		t.Fatalf("production must not offer a session grant:\n%s", text)
	}
}

func TestApprovalPromptWrapsToWidth(t *testing.T) {
	prompt := newApprovalPrompt(permissions.PendingApproval{
		Action: permissions.Action{
			Tool: "run", Category: permissions.CatShellExecute, Risk: permissions.RiskLow,
			Summary: "run a very long command line that will certainly need to be wrapped somewhere",
			Detail:  strings.Repeat("abcdefghij ", 12),
		},
	})
	for _, width := range []int{20, 40, 80} {
		for _, l := range prompt.Lines(width) {
			if displayWidth(l.Text) > width {
				t.Fatalf("width %d produced %q", width, l.Text)
			}
		}
	}
}

func TestApprovalPromptQueueDepth(t *testing.T) {
	prompt := newApprovalPrompt(permissions.PendingApproval{Action: permissions.Action{Tool: "run"}})
	prompt.queued = 3
	if !strings.Contains(prompt.Lines(80)[0].Text, "3 more waiting") {
		t.Fatalf("queue depth not shown: %q", prompt.Lines(80)[0].Text)
	}
}

func TestApprovalCursorMovementWraps(t *testing.T) {
	prompt := newApprovalPrompt(permissions.PendingApproval{
		Action: permissions.Action{Tool: "run", Risk: permissions.RiskLow},
	})
	n := len(prompt.choices)
	prompt.move(-1)
	if prompt.cursor != n-1 {
		t.Fatalf("cursor = %d, want %d", prompt.cursor, n-1)
	}
	prompt.move(1)
	if prompt.cursor != 0 {
		t.Fatalf("cursor = %d, want 0", prompt.cursor)
	}
}

func TestApprovalChoiceForKey(t *testing.T) {
	prompt := newApprovalPrompt(permissions.PendingApproval{
		Action: permissions.Action{Tool: "run", Risk: permissions.RiskLow},
	})
	for _, tc := range []struct {
		key      rune
		found    bool
		approved bool
	}{
		{'a', true, true},
		{'s', true, true},
		{'r', true, false},
		{'z', false, false},
	} {
		choice, ok := prompt.choiceFor(tc.key)
		if ok != tc.found {
			t.Fatalf("choiceFor(%q) found = %v, want %v", tc.key, ok, tc.found)
		}
		if ok && choice.Approved != tc.approved {
			t.Fatalf("choiceFor(%q).Approved = %v", tc.key, choice.Approved)
		}
	}
}

func TestButtonRowHitTesting(t *testing.T) {
	choices := approvalChoices(permissions.Action{Tool: "run", Risk: permissions.RiskLow})
	row, spans := buttonRow(choices, 0)
	if len(spans) != len(choices) {
		t.Fatalf("spans = %d, want %d", len(spans), len(choices))
	}
	if displayWidth(row) < spans[len(spans)-1].End {
		t.Fatalf("last span %v runs past the row width %d", spans[len(spans)-1], displayWidth(row))
	}
	for i, span := range spans {
		if got := hitButton(spans, span.Start); got != i {
			t.Errorf("click at the start of button %d resolved to %d", i, got)
		}
		if got := hitButton(spans, span.End-1); got != i {
			t.Errorf("click at the end of button %d resolved to %d", i, got)
		}
	}
	if got := hitButton(spans, 0); got != -1 {
		t.Errorf("click in the left gutter resolved to %d, want -1", got)
	}
	if got := hitButton(spans, spans[len(spans)-1].End+5); got != -1 {
		t.Errorf("click past the last button resolved to %d, want -1", got)
	}
}

func TestButtonRowMarksTheCursor(t *testing.T) {
	choices := approvalChoices(permissions.Action{Tool: "run", Risk: permissions.RiskLow})
	row, _ := buttonRow(choices, 1)
	if !strings.Contains(row, ">Always for session") {
		t.Fatalf("cursor marker missing: %q", row)
	}
}

func TestResolutionEntry(t *testing.T) {
	tests := []struct {
		name  string
		event permissions.ApprovalEvent
		want  string
		state ToolState
	}{
		{
			name: "approved once",
			event: permissions.ApprovalEvent{
				Approved: true, Scope: permissions.ScopeOnce,
				Approval: permissions.PendingApproval{Action: permissions.Action{Tool: "run", Detail: "ls"}},
			},
			want: "approved: ls", state: ToolOK,
		},
		{
			name: "approved for the session",
			event: permissions.ApprovalEvent{
				Approved: true, Scope: permissions.ScopeSessionCommand,
				Approval: permissions.PendingApproval{Action: permissions.Action{Tool: "run", Detail: "ls"}},
			},
			want: "remembered for this session", state: ToolOK,
		},
		{
			name: "denied",
			event: permissions.ApprovalEvent{
				Approval: permissions.PendingApproval{Action: permissions.Action{Tool: "run", Detail: "rm -rf /"}},
			},
			want: "denied: rm -rf /", state: ToolDenied,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entry := resolutionEntry(tc.event)
			if !strings.Contains(entry.Text, tc.want) {
				t.Errorf("entry text = %q, want it to contain %q", entry.Text, tc.want)
			}
			if entry.State != tc.state {
				t.Errorf("state = %q, want %q", entry.State, tc.state)
			}
		})
	}
}

func promptText(p *approvalPrompt, width int) string {
	var b strings.Builder
	for _, l := range p.Lines(width) {
		b.WriteString(l.Text)
		b.WriteString("\n")
	}
	return b.String()
}
