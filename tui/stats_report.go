package tui

import (
	"fmt"
	"strings"

	"github.com/kawaiipantsu/boop/internal/stats"
)

// trackerText renders the shared statistics tracker (§28).
//
// The tracker is the process-wide record every frontend shares, as opposed to
// Model.stats, which counts only what this terminal session has seen. When
// nothing has been recorded into it, it says so rather than printing a column
// of zeros that reads like a bug in the counters.
func trackerText(snap stats.Snapshot, sessionID string) string {
	var b strings.Builder
	b.WriteString("runtime statistics (shared tracker)\n")
	fmt.Fprintf(&b, "  uptime           %s\n", formatDuration(snap.Uptime.Duration().Truncate(1e9)))

	agg := snap.Totals
	if scoped, ok := snap.Sessions[sessionID]; ok {
		agg = scoped
		b.WriteString("  scope            this session\n")
	}

	c := agg.Counters
	if c == (stats.Counters{}) && agg.Tokens.Approximate().IsZero() {
		b.WriteString("  nothing has been recorded into the shared tracker yet")
		return b.String()
	}

	fmt.Fprintf(&b, "  model calls      %d (%d failed)\n", c.ModelCalls, c.ModelCallFailures)
	fmt.Fprintf(&b, "  tool calls       %d (%d failed)\n", c.ToolCalls, c.ToolFailures)
	fmt.Fprintf(&b, "  commands         %d (%d failed, %d timed out)\n", c.Commands, c.CommandFailures, c.CommandTimeouts)
	if c.TestRuns > 0 {
		fmt.Fprintf(&b, "  test runs        %d (%d failed) — %d passed, %d failed, %d skipped\n",
			c.TestRuns, c.TestRunsFailed, c.TestsPassed, c.TestsFailed, c.TestsSkipped)
	}
	if c.RepairIterations > 0 {
		fmt.Fprintf(&b, "  repair loops     %d (%d recovered)\n", c.RepairIterations, c.RepairSuccesses)
	}
	if c.AgentsSpawned > 0 {
		fmt.Fprintf(&b, "  agents           %d spawned, %d complete, %d failed\n",
			c.AgentsSpawned, c.AgentsCompleted, c.AgentsFailed)
	}
	b.WriteString("  " + tokenLine(agg.Tokens) + "\n")
	fmt.Fprintf(&b, "  time in model    %s · tools %s · commands %s\n",
		agg.Durations.Model, agg.Durations.Tool, agg.Durations.Command)

	cost := agg.Cost
	switch {
	case cost.Total() == 0 && cost.UnpricedCalls > 0:
		fmt.Fprintf(&b, "  cost             unknown — %d call(s) have no pricing metadata\n", cost.UnpricedCalls)
	case cost.Total() == 0:
		b.WriteString("  cost             0.00\n")
	default:
		fmt.Fprintf(&b, "  cost             %.4f %s%s\n", cost.Total(), orDefault(cost.Currency, "USD"),
			costQualifier(cost))
	}
	return strings.TrimRight(b.String(), "\n")
}

// tokenLine reports measured and estimated tokens separately, because a
// provider-reported count and Boop's own guess are not the same fact (§28).
func tokenLine(t stats.TokenCounts) string {
	measured := t.Measured
	if t.Exact() {
		return fmt.Sprintf("tokens           %d prompt · %d output · %d total",
			measured.Prompt, measured.Completion, measured.Total)
	}
	est := t.Estimated
	return fmt.Sprintf("tokens           %d prompt · %d output · %d total (plus ~%d estimated)",
		measured.Prompt, measured.Completion, measured.Total, est.Total)
}

// costQualifier flags a total that is partly guessed or partly unknown.
func costQualifier(c stats.CostTotals) string {
	switch {
	case c.UnpricedCalls > 0:
		return fmt.Sprintf(" (incomplete: %d unpriced call(s))", c.UnpricedCalls)
	case c.Estimated > 0:
		return " (partly estimated)"
	default:
		return ""
	}
}
