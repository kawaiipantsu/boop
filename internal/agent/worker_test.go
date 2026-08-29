package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/app"
	"github.com/kawaiipantsu/boop/internal/provider"
)

func TestLoopRunnerRunsATaskAndReportsUsage(t *testing.T) {
	p := &scriptedProvider{turns: [][]provider.ChatEvent{
		toolTurn("c1", "read", `{"path":"x.go"}`),
		textTurn("I read the file."),
	}}
	reg, made := fullRegistry("read", "write", "run")
	bus, _ := newBusWithCollector()
	runner := &LoopRunner{Loops: newLoopFactory(t, p, reg, bus), Tools: reg}

	a := NewAgent(AgentSpec{Name: "w1", Task: "read x.go", Bus: bus})
	brief := (&Scope{}).Brief(BriefRequest{Task: Task{ID: "t1", Description: "read x.go", Reads: []string{"x.go"}}})

	outcome, err := runner.RunTask(context.Background(), a, brief)
	if err != nil {
		t.Fatalf("RunTask() = %v", err)
	}
	if outcome.Output != "I read the file." {
		t.Errorf("Output = %q", outcome.Output)
	}
	if outcome.AgentID != a.ID {
		t.Errorf("AgentID = %q, want %q", outcome.AgentID, a.ID)
	}
	if outcome.ToolCalls != 1 {
		t.Errorf("ToolCalls = %d, want 1", outcome.ToolCalls)
	}
	if outcome.Usage.TotalTokens == 0 {
		t.Error("Usage was not reported")
	}
	if made["read"].executions() != 1 {
		t.Errorf("read ran %d times, want 1", made["read"].executions())
	}
	if a.State() != StatusThinking {
		t.Errorf("State() = %q, want the runner to have moved the agent past idle", a.State())
	}
}

// TestLoopRunnerEnforcesTheAllowlist is the §10 guarantee that matters most:
// the restriction is structural, not a line in a prompt.
func TestLoopRunnerEnforcesTheAllowlist(t *testing.T) {
	p := &scriptedProvider{turns: [][]provider.ChatEvent{
		toolTurn("c1", "run", `{"command":"rm -rf /"}`),
		textTurn("I could not run that."),
	}}
	reg, made := fullRegistry("read", "write", "run")
	bus, _ := newBusWithCollector()
	runner := &LoopRunner{Loops: newLoopFactory(t, p, reg, bus), Tools: reg}

	a := NewAgent(AgentSpec{Name: "w1", Task: "look around", Bus: bus})
	brief := Brief{Task: "look around", AllowedTools: []string{"read"}}

	outcome, err := runner.RunTask(context.Background(), a, brief)
	if err != nil {
		t.Fatalf("RunTask() = %v", err)
	}
	if made["run"].executions() != 0 {
		t.Fatalf("the run tool executed %d times despite not being granted", made["run"].executions())
	}
	if outcome.Output != "I could not run that." {
		t.Errorf("Output = %q", outcome.Output)
	}

	reqs := p.requests()
	if len(reqs) != 2 {
		t.Fatalf("got %d model requests, want 2", len(reqs))
	}
	// The model is only ever offered the granted tool.
	if len(reqs[0].Tools) != 1 || reqs[0].Tools[0].Name != "read" {
		t.Errorf("tools offered = %+v, want only read", reqs[0].Tools)
	}
	// And when it asks for another anyway, it is told the tool does not exist.
	var toolReply string
	for _, m := range reqs[1].Messages {
		if m.Role == provider.RoleTool {
			toolReply = m.Content
		}
	}
	if !strings.Contains(toolReply, "no tool named") {
		t.Errorf("tool reply = %q, want a refusal naming the missing tool", toolReply)
	}
}

// TestLoopRunnerSendsNoConversationHistory pins the isolation rule: the worker
// starts from its brief, not from a copy of the main conversation.
func TestLoopRunnerSendsNoConversationHistory(t *testing.T) {
	p := &scriptedProvider{turns: [][]provider.ChatEvent{textTurn("done")}}
	reg, _ := fullRegistry("read")
	runner := &LoopRunner{Loops: newLoopFactory(t, p, reg, nil), Tools: reg}

	a := NewAgent(AgentSpec{Name: "w1", Task: "summarise"})
	brief := Brief{Task: "summarise the router", AllowedTools: []string{"read"}}
	if _, err := runner.RunTask(context.Background(), a, brief); err != nil {
		t.Fatalf("RunTask() = %v", err)
	}

	reqs := p.requests()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests, want 1", len(reqs))
	}
	if len(reqs[0].Messages) != 2 {
		t.Fatalf("the worker started with %d messages, want exactly the system prompt and its brief", len(reqs[0].Messages))
	}
}

func TestLoopRunnerOverridesSelectionFromTheAgent(t *testing.T) {
	p := &scriptedProvider{turns: [][]provider.ChatEvent{textTurn("ok")}}
	reg, _ := fullRegistry("read")
	base := newLoopFactory(t, p, reg, nil)

	var captured *app.Loop
	runner := &LoopRunner{
		Loops: func(id string) *app.Loop {
			captured = base(id)
			return captured
		},
		Tools:         reg,
		MaxIterations: 2,
	}
	a := NewAgent(AgentSpec{Name: "w", Task: "t", Provider: "scripted", Model: "test"})
	if _, err := runner.RunTask(context.Background(), a, Brief{Task: "t"}); err != nil {
		t.Fatalf("RunTask() = %v", err)
	}
	// The factory's loop must not be mutated: the runner works on a copy.
	if captured.MaxIterations != 5 {
		t.Errorf("the shared loop's MaxIterations changed to %d", captured.MaxIterations)
	}
	if len(captured.Tools.Names()) != 1 {
		t.Errorf("the shared registry was replaced: %v", captured.Tools.Names())
	}
}

func TestLoopRunnerWithoutAFactory(t *testing.T) {
	tests := []struct {
		name   string
		runner *LoopRunner
	}{
		{"nil runner", nil},
		{"no factory", &LoopRunner{}},
		{"factory returns nil", &LoopRunner{Loops: func(string) *app.Loop { return nil }}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAgent(AgentSpec{Task: "t"})
			_, err := tc.runner.RunTask(context.Background(), a, Brief{Task: "t"})
			if !errors.Is(err, ErrNoLoop) {
				t.Errorf("RunTask() = %v, want ErrNoLoop", err)
			}
		})
	}
}

func TestLoopRunnerFallsBackToTheFactoryRegistry(t *testing.T) {
	p := &scriptedProvider{turns: [][]provider.ChatEvent{
		toolTurn("c1", "read", `{}`),
		textTurn("ok"),
	}}
	reg, made := fullRegistry("read", "write")
	// No Tools field: the runner must restrict whatever the loop already has.
	runner := &LoopRunner{Loops: newLoopFactory(t, p, reg, nil)}

	a := NewAgent(AgentSpec{Task: "t"})
	if _, err := runner.RunTask(context.Background(), a, Brief{Task: "t", AllowedTools: []string{"read"}}); err != nil {
		t.Fatalf("RunTask() = %v", err)
	}
	if made["read"].executions() != 1 {
		t.Errorf("read ran %d times, want 1", made["read"].executions())
	}
	if len(p.requests()[0].Tools) != 1 {
		t.Errorf("tools offered = %+v, want only read", p.requests()[0].Tools)
	}
}
