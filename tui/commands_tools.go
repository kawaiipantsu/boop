package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/tools"
)

// treeDefaultDepth is the recursion depth /tree uses when none is given. It
// matches the list tool's own default for a recursive listing.
const treeDefaultDepth = 3

// ---------------------------------------------------------------------------
// The tool-backed slash commands
// ---------------------------------------------------------------------------

// runCmd implements /run <command>.
func (m *Model) runCmd(cmd Command) tea.Cmd {
	line := strings.TrimSpace(cmd.Rest)
	if line == "" {
		return m.say(EntryError, "usage: /run <command>")
	}
	return m.invokeTool("run", map[string]any{"command": line})
}

// execTaskCmd implements /test, /build, /lint and /format, which share the task
// tools' argument shape: an optional explicit command, otherwise detection.
// /format also takes a leading --check (or "check") to run the read-only check.
func (m *Model) execTaskCmd(cmd Command) tea.Cmd {
	args := map[string]any{}
	line := strings.TrimSpace(cmd.Rest)
	if cmd.Name == "format" {
		if rest, ok := strings.CutPrefix(line, "--check"); ok {
			args["check"], line = true, strings.TrimSpace(rest)
		} else if rest, ok := strings.CutPrefix(line, "check"); ok && (rest == "" || rest[0] == ' ') {
			args["check"], line = true, strings.TrimSpace(rest)
		}
	}
	if line != "" {
		args["command"] = line
	}
	return m.invokeTool(cmd.Name, args)
}

// listCmd implements /files (one directory) and /tree (recursive).
//
// Both go through the list tool, which takes the recursion depth, so the two
// commands differ only in the depth they ask for.
func (m *Model) listCmd(cmd Command) tea.Cmd {
	path := cmd.Arg(0)
	if path == "" {
		path = "."
	}
	args := map[string]any{"path": path}
	if cmd.Name == "tree" {
		depth := treeDefaultDepth
		if raw := cmd.Arg(1); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 {
				return m.say(EntryError, "usage: /tree [path] [depth] — depth must be a positive whole number")
			}
			depth = n
		}
		args["recursive"] = true
		args["max_depth"] = depth
	}
	return m.invokeTool("list", args)
}

// searchCmd implements /search <pattern>.
//
// This searches the workspace. Searching the internet is the websearch tool,
// which stays behind the model and the network permission because it discloses
// the query to a third party (§14).
func (m *Model) searchCmd(cmd Command) tea.Cmd {
	pattern := strings.TrimSpace(cmd.Rest)
	if pattern == "" {
		return m.say(EntryError, "usage: /search <pattern> — searches this workspace, not the web")
	}
	return m.invokeTool("search", map[string]any{"pattern": pattern})
}

// ---------------------------------------------------------------------------
// Invoking a tool from the UI
// ---------------------------------------------------------------------------

// invokeTool runs one registered tool exactly as a model-issued call would.
//
// It goes through the registry, the tool's own Permission classification and
// the evaluator, so a slash command cannot reach the filesystem or a shell on
// terms the permission engine never saw (§64.2). Running the command directly
// would be shorter and would quietly skip the confirmation the user expects.
//
// The call itself runs in a tea.Cmd: a build can take minutes, and an approval
// parks the calling goroutine until the update loop answers it, which would
// deadlock if that goroutine were the update loop.
func (m *Model) invokeTool(name string, args map[string]any) tea.Cmd {
	if m.app == nil || m.app.Tools == nil {
		return m.say(EntryError, "no runtime is attached, so there are no tools to run")
	}
	tool, ok := m.app.Tools.Get(name)
	if !ok {
		return m.say(EntryError, fmt.Sprintf("the %s tool is not registered; available: %s",
			name, strings.Join(m.app.Tools.Names(), ", ")))
	}
	if m.turnActive {
		m.notice = "something is already running — Ctrl+C cancels it"
		return nil
	}

	encoded, err := json.Marshal(args)
	if err != nil {
		return m.say(EntryError, "could not encode the tool arguments: "+err.Error())
	}
	call := tools.Call{
		ID:        fmt.Sprintf("tui-%s-%d", name, time.Now().UnixNano()),
		Name:      name,
		Arguments: encoded,
	}

	// Classify before running so the transcript line describes the real
	// action rather than the raw arguments, and so invalid arguments are
	// reported here instead of half-way through execution.
	action, err := tool.Permission(call)
	if err != nil {
		return m.say(EntryError, fmt.Sprintf("invalid arguments for %s: %v", name, err))
	}
	summary := actionHeadline(action)

	m.transcript.StartTool(name, summary)
	m.follow = true

	ctx, cancel := context.WithCancel(m.ctx)
	m.turnCancel = cancel
	m.turnActive = true
	m.status = statusForTool(name)
	m.interruptArmed = false
	if m.approver != nil {
		// Binding the approval to this context is what lets Ctrl+C release a
		// prompt the user has walked away from (§51).
		m.approver.SetTurnContext(ctx)
	}
	m.relayout()

	evaluator := m.app.Evaluator
	approver := m.approver
	wg := m.turns
	wg.Add(1)

	return func() tea.Msg {
		defer wg.Done()
		started := time.Now()

		if evaluator != nil {
			switch decision := evaluator.Evaluate(action); decision.Outcome {
			case permissions.OutcomeDeny:
				return toolDoneMsg{tool: name, summary: "denied", isError: true,
					duration: time.Since(started),
					content:  "denied by policy: " + decision.Reason}
			case permissions.OutcomeConfirm:
				if approver == nil {
					return toolDoneMsg{tool: name, summary: "no approver", isError: true,
						duration: time.Since(started),
						content:  name + " needs approval but no approver is attached"}
				}
				approved, err := approver.Approve(action)
				switch {
				case err != nil:
					return toolDoneMsg{tool: name, summary: "approval failed", isError: true,
						duration: time.Since(started), content: "approval failed: " + err.Error()}
				case !approved:
					return toolDoneMsg{tool: name, summary: "refused", isError: true,
						duration: time.Since(started), content: "you refused this action; nothing ran"}
				}
			}
		}

		result, err := tool.Execute(ctx, call)
		switch {
		case err != nil && (ctx.Err() != nil || isCancellation(err)):
			// The user pressed Ctrl+C. That is not a fault to paint red.
			return toolDoneMsg{tool: name, summary: "interrupted", isError: true,
				duration: time.Since(started), content: "interrupted."}
		case err != nil:
			return toolDoneMsg{tool: name, summary: "failed", isError: true,
				duration: time.Since(started), err: err,
				content: fmt.Sprintf("%s failed: %v", name, err)}
		}
		if result.Duration == 0 {
			result.Duration = time.Since(started)
		}
		return toolDoneMsg{
			tool:     name,
			summary:  result.Display,
			content:  result.Content,
			duration: result.Duration,
			isError:  result.IsError,
		}
	}
}

// finishToolRun folds a slash-command tool call back into the UI.
func (m *Model) finishToolRun(msg toolDoneMsg) tea.Cmd {
	m.turnActive = false
	if m.turnCancel != nil {
		m.turnCancel()
		m.turnCancel = nil
	}
	if m.approver != nil {
		m.approver.ResetTurnContext()
	}

	state := ToolOK
	if msg.isError {
		state = ToolFailed
		m.stats.ToolFailures++
	}
	m.stats.ToolCalls++
	m.transcript.FinishToolWithOutcome(msg.tool, state, msg.duration, msg.summary)
	if text := strings.TrimRight(sanitize(msg.content), "\n"); strings.TrimSpace(text) != "" {
		// Appended rather than attached: AttachToolOutput matches the oldest
		// unattached call of that name, which for a command typed long after
		// a model-issued call of the same tool would file the output under
		// the wrong line.
		kind := EntryOutput
		if msg.isError {
			kind = EntryError
		}
		m.transcript.Append(Entry{Kind: kind, Text: clipLines(text, maxToolOutputLines)})
	}

	switch {
	case msg.err != nil:
		m.status = StatusError
	default:
		m.status = StatusIdle
	}
	m.follow = true
	m.relayout()
	return nil
}
