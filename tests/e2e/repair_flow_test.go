//go:build e2e

package e2e

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/boop-dev/boop/internal/provider"
	"github.com/boop-dev/boop/tests/fixtures"
)

// shellStub is a deterministic stand-in for the run tool: commands listed in
// failures exit non-zero with the given stderr, everything else succeeds.
func shellStub(t *testing.T, failures map[string]string) toolFunc {
	t.Helper()
	return func(call provider.ToolCall) toolResult {
		command, err := commandOf(call)
		if err != nil {
			// A model that emits invalid JSON gets a structured failure back,
			// which is what lets it repair itself rather than derailing.
			return toolResult{ExitCode: 2, Stderr: err.Error()}
		}
		if stderr, bad := failures[command]; bad {
			return toolResult{Command: command, ExitCode: 1, Stderr: stderr, Duration: time.Millisecond}
		}
		return toolResult{Command: command, ExitCode: 0, Stdout: "ok\n", Duration: time.Millisecond}
	}
}

// TestErrorRepairLoop drives the exact flow §41 names as representative:
// prompt → model response → run tool → failure → repair → test → success.
func TestErrorRepairLoop(t *testing.T) {
	p := fixtures.NewFakeProvider("fake",
		func() fixtures.Turn {
			turn := fixtures.ToolTurn(provider.ToolCall{
				ID: "call_1", Name: "run", Arguments: `{"command":"go tset ./..."}`,
			})
			turn.Text = "Running the test suite."
			return turn
		}(),
		func() fixtures.Turn {
			turn := fixtures.ToolTurn(provider.ToolCall{
				ID: "call_2", Name: "run", Arguments: `{"command":"go test ./..."}`,
			})
			turn.Text = "That was a typo: `tset` should be `test`. Retrying."
			return turn
		}(),
		fixtures.TextTurn("The test suite passes."),
	)

	exec := shellStub(t, map[string]string{
		"go tset ./...": `go: unknown command "tset"`,
	})

	out, err := runLoop(context.Background(), p, "boop-test-model",
		"run the tests", []provider.ToolDefinition{runToolDefinition()}, exec,
		loopConfig{MaxToolIterations: 10, RequireToolCapability: true})
	if err != nil {
		t.Fatalf("loop: %v", err)
	}

	if out.Turns != 3 {
		t.Errorf("model turns = %d, want 3", out.Turns)
	}
	if len(out.Executions) != 2 {
		t.Fatalf("tool executions = %+v", out.Executions)
	}
	if !out.Executions[0].failed() || out.Executions[0].ExitCode != 1 {
		t.Errorf("first execution should have failed: %+v", out.Executions[0])
	}
	if out.Executions[1].failed() {
		t.Errorf("repaired execution should have succeeded: %+v", out.Executions[1])
	}
	if out.FinalText != "The test suite passes." {
		t.Errorf("final text = %q", out.FinalText)
	}
	if p.TurnsRemaining() != 0 {
		t.Errorf("script not fully consumed: %d turns left", p.TurnsRemaining())
	}

	// The model must actually have been shown the structured failure: that is
	// the whole mechanism behind the repair loop.
	requests := p.Requests()
	if len(requests) != 3 {
		t.Fatalf("provider calls = %d, want 3", len(requests))
	}
	second := requests[1].Messages
	last := second[len(second)-1]
	if last.Role != provider.RoleTool || last.ToolCallID != "call_1" {
		t.Fatalf("second request did not end with the tool result: %+v", last)
	}
	if !strings.Contains(last.Content, `"exit_code":1`) {
		t.Errorf("tool result did not carry the exit code: %s", last.Content)
	}
	if !strings.Contains(last.Content, "unknown command") {
		t.Errorf("tool result did not carry stderr: %s", last.Content)
	}

	// The transcript must interleave assistant tool calls with tool results so
	// session persistence (§46) has a faithful record.
	var roles []string
	for _, m := range out.Transcript {
		roles = append(roles, string(m.Role))
	}
	want := "system,user,assistant,tool,assistant,tool,assistant"
	if got := strings.Join(roles, ","); got != want {
		t.Errorf("transcript roles = %s, want %s", got, want)
	}
}

// TestRepairScriptHelper proves the shipped helper drives the same loop, so
// other suites can express a repair exchange in one line.
func TestRepairScriptHelper(t *testing.T) {
	p := fixtures.NewFakeProvider("fake",
		fixtures.RepairScript("run", `{"command":"go buld"}`, `{"command":"go build"}`, "Fixed and built.")...)
	exec := shellStub(t, map[string]string{"go buld": `unknown command "buld"`})

	out, err := runLoop(context.Background(), p, "boop-test-model", "build it",
		[]provider.ToolDefinition{runToolDefinition()}, exec, loopConfig{})
	if err != nil {
		t.Fatalf("loop: %v", err)
	}
	if len(out.Executions) != 2 || !out.Executions[0].failed() || out.Executions[1].failed() {
		t.Fatalf("executions = %+v", out.Executions)
	}
	if out.FinalText != "Fixed and built." {
		t.Errorf("final text = %q", out.FinalText)
	}
}

// TestMalformedToolArgumentsAreRepairable shows that invalid JSON from the
// model is data, not a crash.
func TestMalformedToolArgumentsAreRepairable(t *testing.T) {
	p := fixtures.NewFakeProvider("fake",
		fixtures.ToolTurn(provider.ToolCall{ID: "call_1", Name: "run", Arguments: `{"command": go test`}),
		fixtures.ToolTurn(provider.ToolCall{ID: "call_2", Name: "run", Arguments: `{"command":"go test ./..."}`}),
		fixtures.TextTurn("Recovered."),
	)

	out, err := runLoop(context.Background(), p, "boop-test-model", "test it",
		[]provider.ToolDefinition{runToolDefinition()}, shellStub(t, nil), loopConfig{})
	if err != nil {
		t.Fatalf("loop: %v", err)
	}
	if len(out.Executions) != 2 || out.Executions[0].ExitCode != 2 {
		t.Fatalf("executions = %+v", out.Executions)
	}
	if !strings.Contains(out.Executions[0].Stderr, "invalid tool arguments") {
		t.Errorf("stderr = %q", out.Executions[0].Stderr)
	}
	if out.FinalText != "Recovered." {
		t.Errorf("final text = %q", out.FinalText)
	}
}

// TestIterationLimitBoundsTheLoop guards the configured ceiling of §13.
func TestIterationLimitBoundsTheLoop(t *testing.T) {
	p := fixtures.NewFakeProvider("fake",
		fixtures.ToolTurn(provider.ToolCall{ID: "call_x", Name: "run", Arguments: `{"command":"loop"}`}))
	p.RepeatLastTurn(true) // a model stuck calling the same tool forever

	out, err := runLoop(context.Background(), p, "boop-test-model", "go",
		[]provider.ToolDefinition{runToolDefinition()},
		shellStub(t, map[string]string{"loop": "still broken"}), loopConfig{MaxToolIterations: 4})
	if err == nil {
		t.Fatal("the loop must stop at the configured iteration limit")
	}
	if !errors.Is(err, errIterationLimit) {
		t.Fatalf("err = %v", err)
	}
	if out.Turns != 4 {
		t.Errorf("turns = %d, want 4", out.Turns)
	}
	if len(out.Executions) != 3 {
		t.Errorf("executions = %d, want 3 (the fourth turn is not executed)", len(out.Executions))
	}
}

// TestProviderErrorAbortsLoop checks that a mid-stream provider failure ends
// the run with a normalized error rather than a half-finished answer.
func TestProviderErrorAbortsLoop(t *testing.T) {
	boom := provider.NewError(provider.ErrServer, "fake", "the model server crashed", nil)
	p := fixtures.NewFakeProvider("fake", fixtures.ErrorTurn(boom))

	_, err := runLoop(context.Background(), p, "boop-test-model", "hi", nil, shellStub(t, nil), loopConfig{})
	if err == nil {
		t.Fatal("expected the provider error to surface")
	}
	cat, ok := provider.CategoryOf(err)
	if !ok || cat != provider.ErrServer {
		t.Fatalf("category = %q ok=%v", cat, ok)
	}
	if !provider.IsRetryable(err) {
		t.Error("a server error should be retryable")
	}
}

// TestCancellationStopsLoop covers §51: an interrupt ends the exchange
// promptly with a cancelled error.
func TestCancellationStopsLoop(t *testing.T) {
	turn := fixtures.TextTurn("")
	turn.TextChunks = []string{"one ", "two ", "three ", "four"}
	turn.EventDelay = 30 * time.Millisecond
	p := fixtures.NewFakeProvider("fake", turn)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(45 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := runLoop(ctx, p, "boop-test-model", "talk", nil, shellStub(t, nil), loopConfig{})
	if err == nil {
		t.Fatal("cancellation must surface as an error")
	}
	if cat, _ := provider.CategoryOf(err); cat != provider.ErrCancelled {
		t.Fatalf("category = %q, want cancelled (err: %v)", cat, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("cancellation took %v, expected it to be prompt", elapsed)
	}
}

// TestStreamedDeltasArriveInOrder is the UI-facing guarantee: whatever a
// frontend renders is the model's text in order (§25).
func TestStreamedDeltasArriveInOrder(t *testing.T) {
	turn := fixtures.TextTurn("")
	turn.TextChunks = []string{"Hel", "lo, ", "Boop"}
	p := fixtures.NewFakeProvider("fake", turn)

	out, err := runLoop(context.Background(), p, "boop-test-model", "hi", nil, shellStub(t, nil), loopConfig{})
	if err != nil {
		t.Fatalf("loop: %v", err)
	}
	if strings.Join(out.Deltas, "|") != "Hel|lo, |Boop" {
		t.Errorf("deltas = %v", out.Deltas)
	}
	if out.FinalText != "Hello, Boop" {
		t.Errorf("final text = %q", out.FinalText)
	}
}

// TestToolCapabilityIsCheckedNotAssumed covers §8: a model without tool
// support is reported, not attempted.
func TestToolCapabilityIsCheckedNotAssumed(t *testing.T) {
	p := fixtures.NewFakeProvider("fake", fixtures.TextTurn("never reached"))
	p.SetCapabilities("text-only", provider.Capabilities{provider.CapabilityStreaming})

	_, err := runLoop(context.Background(), p, "text-only", "run the tests",
		[]provider.ToolDefinition{runToolDefinition()}, shellStub(t, nil),
		loopConfig{RequireToolCapability: true})
	var unsupported *provider.UnsupportedCapabilityError
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want an UnsupportedCapabilityError", err)
	}
	if len(unsupported.Missing) != 1 || unsupported.Missing[0] != provider.CapabilityTools {
		t.Errorf("missing = %v", unsupported.Missing)
	}
	if len(p.Requests()) != 0 {
		t.Error("the model must not be called when it cannot do the job")
	}
}

// TestExchangeIsDeterministic re-runs the same script and demands identical
// results: an e2e suite that is not reproducible is not worth running.
func TestExchangeIsDeterministic(t *testing.T) {
	script := func() *fixtures.FakeProvider {
		return fixtures.NewFakeProvider("fake",
			fixtures.RepairScript("run", `{"command":"bad"}`, `{"command":"good"}`, "done")...)
	}
	exec := shellStub(t, map[string]string{"bad": "nope"})

	var previous loopOutcome
	for i := 0; i < 3; i++ {
		out, err := runLoop(context.Background(), script(), "boop-test-model", "go",
			[]provider.ToolDefinition{runToolDefinition()}, exec, loopConfig{})
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if i > 0 {
			if out.FinalText != previous.FinalText || out.Turns != previous.Turns ||
				len(out.Executions) != len(previous.Executions) {
				t.Fatalf("run %d differed: %+v vs %+v", i, out, previous)
			}
		}
		previous = out
	}
}
