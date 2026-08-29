package tui

import (
	"sort"
	"strings"
)

// Command is a parsed slash command (§20).
type Command struct {
	// Name is the command word without its slash, lower-cased.
	Name string
	// Args are the whitespace-separated arguments.
	Args []string
	// Rest is everything after the command word, unsplit, for commands whose
	// argument is free text.
	Rest string
}

// Arg returns the nth argument or "".
func (c Command) Arg(n int) string {
	if n < len(c.Args) {
		return c.Args[n]
	}
	return ""
}

// ParseCommand recognises a slash command in a line of input.
//
// It reports false for anything that should be sent to the model, including a
// bare "/" and a doubled leading slash, which is the escape for a message that
// genuinely starts with one.
func ParseCommand(input string) (Command, bool) {
	trimmed := strings.TrimSpace(input)
	if len(trimmed) < 2 || trimmed[0] != '/' || trimmed[1] == '/' {
		return Command{}, false
	}
	body := trimmed[1:]
	name, rest, _ := strings.Cut(body, " ")
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || strings.ContainsAny(name, "/\\") {
		return Command{}, false
	}
	rest = strings.TrimSpace(rest)
	return Command{Name: name, Args: strings.Fields(rest), Rest: rest}, true
}

// UnescapeMessage strips the escaping slash from a message that starts with
// "//", so a user can send literal text beginning with a slash.
func UnescapeMessage(input string) string {
	trimmed := strings.TrimLeft(input, " \t")
	if strings.HasPrefix(trimmed, "//") {
		return strings.Replace(input, "//", "/", 1)
	}
	return input
}

// commandStatus says how far a command has been implemented, so /help can be
// honest and an unimplemented command can name what is missing rather than
// doing nothing (§20).
type commandStatus int

const (
	// cmdReady is fully implemented here.
	cmdReady commandStatus = iota
	// cmdPending needs a subsystem that is not wired into the TUI yet.
	cmdPending
)

// commandSpec documents one command.
type commandSpec struct {
	Name    string
	Args    string
	Summary string
	Status  commandStatus
	// Blocker names what a pending command is waiting for.
	Blocker string
}

// commandSpecs is the command table, in the order /help prints it.
var commandSpecs = []commandSpec{
	{Name: "help", Summary: "list commands", Status: cmdReady},
	{Name: "status", Summary: "runtime, provider health and session state", Status: cmdReady},
	{Name: "stats", Summary: "counters for this session", Status: cmdReady},
	{Name: "tokens", Summary: "token usage for this session", Status: cmdReady},
	{Name: "context", Summary: "what is currently in the conversation", Status: cmdReady},
	{Name: "provider", Args: "[name]", Summary: "show or switch the active provider", Status: cmdReady},
	{Name: "model", Args: "[id]", Summary: "show or switch the active model", Status: cmdReady},
	{Name: "models", Args: "[provider]", Summary: "list models a provider offers", Status: cmdReady},
	{Name: "permissions", Args: "[mode confirm|auto] [clear]", Summary: "show or adjust the permission policy", Status: cmdReady},
	{Name: "session", Args: "[new|list|save <title>|load <id>]", Summary: "manage sessions", Status: cmdReady},
	{Name: "clear", Summary: "clear the transcript, keep the conversation", Status: cmdReady},
	{Name: "reset", Summary: "clear the transcript and forget the conversation", Status: cmdReady},
	{Name: "boop", Summary: "boop", Status: cmdReady},
	{Name: "quit", Summary: "leave Boop", Status: cmdReady},
	{Name: "exit", Summary: "leave Boop", Status: cmdReady},

	{Name: "prep", Summary: "survey the project and write Boop.md", Status: cmdPending,
		Blocker: "the /prep driver in internal/project is not wired to the TUI yet"},
	{Name: "init", Summary: "create a starter Boop.md", Status: cmdPending,
		Blocker: "project bootstrap is not wired to the TUI yet"},
	{Name: "config", Summary: "edit configuration interactively", Status: cmdPending,
		Blocker: "the interactive config editor (§55) is not built yet"},
	{Name: "agents", Args: "[on|off|max <n>|list|stop <id>]", Summary: "control the agent scheduler", Status: cmdPending,
		Blocker: "the agent scheduler (§10–11) is not wired to the TUI yet"},
	{Name: "run", Args: "<command>", Summary: "run a shell command directly", Status: cmdPending,
		Blocker: "direct execution from the TUI is not wired yet; ask the model to run it and approve the request"},
	{Name: "files", Summary: "list project files", Status: cmdPending,
		Blocker: "the project file browser (§48) is not wired to the TUI yet"},
	{Name: "tree", Summary: "show the project tree", Status: cmdPending,
		Blocker: "the project tree view is not wired to the TUI yet"},
	{Name: "search", Args: "<query>", Summary: "search the project", Status: cmdPending,
		Blocker: "project search is not wired to the TUI yet; ask the model to search"},
	{Name: "test", Summary: "run the project test suite", Status: cmdPending,
		Blocker: "the test runner is not wired to the TUI yet; ask the model to run the tests"},
	{Name: "build", Summary: "build the project", Status: cmdPending,
		Blocker: "the build runner is not wired to the TUI yet; ask the model to build"},
	{Name: "web", Args: "[on|off]", Summary: "control the local WebUI", Status: cmdPending,
		Blocker: "the WebUI (§22) is not built yet"},
	{Name: "gui", Summary: "launch the native GUI", Status: cmdPending,
		Blocker: "the native GUI (§4.5) is a later milestone"},
}

// lookupCommand finds a spec by name.
func lookupCommand(name string) (commandSpec, bool) {
	for _, spec := range commandSpecs {
		if spec.Name == name {
			return spec, true
		}
	}
	return commandSpec{}, false
}

// suggestCommand returns the closest known command name to an unknown one, so
// a typo gets a pointer rather than a shrug.
func suggestCommand(name string) (string, bool) {
	best, bestScore := "", 0
	for _, spec := range commandSpecs {
		score := commonPrefix(spec.Name, name)
		if score > bestScore {
			best, bestScore = spec.Name, score
		}
	}
	if bestScore >= 2 {
		return best, true
	}
	return "", false
}

func commonPrefix(a, b string) int {
	n := minInt(len(a), len(b))
	i := 0
	for ; i < n; i++ {
		if a[i] != b[i] {
			break
		}
	}
	return i
}

// helpText renders the command list for /help.
func helpText() string {
	var ready, pending []commandSpec
	for _, spec := range commandSpecs {
		if spec.Status == cmdReady {
			ready = append(ready, spec)
			continue
		}
		pending = append(pending, spec)
	}
	sort.SliceStable(pending, func(i, j int) bool { return pending[i].Name < pending[j].Name })

	width := 0
	for _, spec := range commandSpecs {
		width = maxInt(width, len(commandUsage(spec)))
	}

	var b strings.Builder
	b.WriteString("commands\n")
	for _, spec := range ready {
		b.WriteString("  " + padRight(commandUsage(spec), width) + "  " + spec.Summary + "\n")
	}
	b.WriteString("\nnot wired up yet — these report what they are waiting for\n")
	for _, spec := range pending {
		b.WriteString("  " + padRight(commandUsage(spec), width) + "  " + spec.Summary + "\n")
	}
	b.WriteString("\nkeys\n")
	b.WriteString(keyHelpText())
	return strings.TrimRight(b.String(), "\n")
}

func commandUsage(spec commandSpec) string {
	if spec.Args == "" {
		return "/" + spec.Name
	}
	return "/" + spec.Name + " " + spec.Args
}

// keyHelpText documents the bindings, including the multi-line submit rules
// that are easy to get wrong (§19).
func keyHelpText() string {
	return strings.Join([]string{
		"  Enter          send (or insert a newline once the message spans several lines)",
		"  Alt+Enter      send from anywhere, including mid-message",
		"  Ctrl+S         send from anywhere (for terminals that eat Alt+Enter)",
		"  Ctrl+J         start a new line without sending",
		"  Up/Down        input history (when the cursor is on the first/last line)",
		"  Ctrl+R         search input history",
		"  PgUp/PgDn      scroll the transcript; Ctrl+Home/Ctrl+End jump to the ends",
		"  Ctrl+C         cancel the running turn; again with nothing running to quit",
		"  Esc            reject a pending approval, or clear the composer",
		"  Ctrl+O         toggle mouse capture, so the terminal can select text again",
		"  a / s / r      answer an approval; Enter confirms the highlighted button",
	}, "\n")
}

// boopText is the /boop easter egg. Harmless, brief, and it never touches the
// model or the filesystem.
func boopText() string {
	return strings.Join([]string{
		"   /\\_/\\   ",
		"  ( o.o )  boop.",
		"   > ^ <   ",
	}, "\n")
}
