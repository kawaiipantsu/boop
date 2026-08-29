package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kawaiipantsu/boop/internal/execution"
	"github.com/kawaiipantsu/boop/internal/permissions"
)

// GitTool runs git through a subcommand allowlist.
//
// Git is not exposed as free-form shell: the subcommand decides the permission
// category (read, commit, push) and arguments are passed to git directly,
// without a shell, so a crafted argument cannot become a second command.
// Operations that rewrite or discard history are classified high so they can
// never happen silently.
type GitTool struct {
	executor execution.Executor
	ws       *Workspace

	// Binary is the git executable, overridable for tests and unusual installs.
	Binary string
	// DefaultTimeout applies when the caller does not specify one.
	DefaultTimeout time.Duration
	// MaxTimeout caps a caller-requested timeout.
	MaxTimeout time.Duration
	// MaxOutputBytes is the per-stream capture cap handed to the executor.
	MaxOutputBytes int
	// MaxOutputLines is the per-stream display cap applied when rendering.
	MaxOutputLines int
}

// NewGitTool returns a git tool backed by executor and confined to ws.
func NewGitTool(executor execution.Executor, ws *Workspace) *GitTool {
	return &GitTool{
		executor:       executor,
		ws:             ws,
		Binary:         "git",
		DefaultTimeout: DefaultCommandTimeout,
		MaxTimeout:     30 * time.Minute,
		MaxOutputBytes: DefaultMaxOutputBytes,
		MaxOutputLines: DefaultMaxOutputLines,
	}
}

// execGitArgs are the decoded arguments of the git tool.
type execGitArgs struct {
	Subcommand     string   `json:"subcommand"`
	Args           []string `json:"args,omitempty"`
	WorkingDir     string   `json:"working_dir,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

// execGitRule is the baseline classification of one allowed subcommand.
type execGitRule struct {
	category permissions.Category
	risk     permissions.Risk
	// what describes the effect for an approval prompt.
	what string
}

// execGitAllowlist is the complete set of subcommands the model may invoke.
// Anything absent — filter-branch, reflog expire, update-ref, gc --prune,
// arbitrary aliases — is refused rather than classified.
var execGitAllowlist = map[string]execGitRule{
	// Read-only inspection.
	"status":    {permissions.CatGitRead, permissions.RiskLow, "inspect working tree status"},
	"log":       {permissions.CatGitRead, permissions.RiskLow, "read commit history"},
	"diff":      {permissions.CatGitRead, permissions.RiskLow, "read changes"},
	"show":      {permissions.CatGitRead, permissions.RiskLow, "read an object"},
	"branch":    {permissions.CatGitRead, permissions.RiskLow, "list branches"},
	"remote":    {permissions.CatGitRead, permissions.RiskLow, "inspect remotes"},
	"rev-parse": {permissions.CatGitRead, permissions.RiskLow, "resolve a revision"},
	"ls-files":  {permissions.CatGitRead, permissions.RiskLow, "list tracked files"},
	"blame":     {permissions.CatGitRead, permissions.RiskLow, "attribute lines"},
	"shortlog":  {permissions.CatGitRead, permissions.RiskLow, "summarise history"},
	"describe":  {permissions.CatGitRead, permissions.RiskLow, "describe a revision"},
	// Network read: touches a remote but does not publish anything.
	"fetch": {permissions.CatGitRead, permissions.RiskMedium, "fetch from a remote"},

	// Local history changes.
	"add":         {permissions.CatGitCommit, permissions.RiskLow, "stage changes"},
	"commit":      {permissions.CatGitCommit, permissions.RiskMedium, "create a commit"},
	"tag":         {permissions.CatGitCommit, permissions.RiskMedium, "create a tag"},
	"stash":       {permissions.CatGitCommit, permissions.RiskMedium, "stash changes"},
	"merge":       {permissions.CatGitCommit, permissions.RiskMedium, "merge branches"},
	"revert":      {permissions.CatGitCommit, permissions.RiskMedium, "revert a commit"},
	"cherry-pick": {permissions.CatGitCommit, permissions.RiskMedium, "apply a commit"},
	"pull":        {permissions.CatGitCommit, permissions.RiskMedium, "pull from a remote"},
	"reset":       {permissions.CatGitCommit, permissions.RiskMedium, "move the branch pointer"},
	"rebase":      {permissions.CatGitCommit, permissions.RiskHigh, "rewrite local history"},

	// Working-tree changes.
	"checkout": {permissions.CatFilesystemWrite, permissions.RiskMedium, "switch or restore files"},
	"switch":   {permissions.CatFilesystemWrite, permissions.RiskMedium, "switch branch"},
	"restore":  {permissions.CatFilesystemWrite, permissions.RiskMedium, "restore files"},
	"clean":    {permissions.CatFilesystemWrite, permissions.RiskMedium, "remove untracked files"},

	// Publishing.
	"push": {permissions.CatGitPush, permissions.RiskMedium, "publish commits to a remote"},
}

// Name implements Tool.
func (t *GitTool) Name() string { return "git" }

// Description implements Tool.
func (t *GitTool) Description() string {
	return "Run an allowed git subcommand (" + strings.Join(execGitSubcommands(), ", ") + "). " +
		"Arguments are passed to git directly, not through a shell. History-rewriting or " +
		"destructive operations always require explicit approval."
}

// Schema implements Tool.
func (t *GitTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"subcommand": map[string]any{
				"type":        "string",
				"enum":        execGitSubcommands(),
				"description": "The git subcommand to run.",
			},
			"args": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Arguments passed to git verbatim, one array element per argument.",
			},
			"working_dir": map[string]any{
				"type":        "string",
				"description": "Directory to run in, relative to the workspace root. Defaults to the root.",
			},
			"timeout_seconds": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"description": "Maximum run time in seconds.",
			},
		},
		"required": []string{"subcommand"},
	}
}

// Permission classifies the git invocation.
//
// A subcommand outside the allowlist is classified at the highest risk with a
// summary saying it will be refused, rather than returning an error: the
// refusal itself happens in Execute so the model gets a repairable result.
func (t *GitTool) Permission(call Call) (permissions.Action, error) {
	var args execGitArgs
	if err := call.Bind(&args); err != nil {
		return permissions.Action{}, err
	}
	sub := strings.TrimSpace(args.Subcommand)
	if sub == "" {
		return permissions.Action{}, fmt.Errorf("git: subcommand is required")
	}
	dir, err := execResolveWorkspaceDir(t.ws, args.WorkingDir)
	if err != nil {
		dir = args.WorkingDir
	}
	display := execDisplayCommand(t.binary(), execGitArgv(sub, args.Args))

	rule, ok := execGitAllowlist[sub]
	if !ok {
		return permissions.Action{
			Category: permissions.CatShellExecute,
			Risk:     permissions.RiskCritical,
			Tool:     t.Name(),
			Summary:  fmt.Sprintf("git %s is not in the boop allowlist and will be refused", sub),
			Detail:   display,
			Paths:    []string{dir},
		}, nil
	}

	category, risk, what := execGitClassify(sub, rule, args.Args)
	return permissions.Action{
		Category: category,
		Risk:     risk,
		Tool:     t.Name(),
		Summary:  fmt.Sprintf("git: %s in %s", what, dir),
		Detail:   display,
		Paths:    []string{dir},
	}, nil
}

// Execute runs the git subcommand.
func (t *GitTool) Execute(ctx context.Context, call Call) (Result, error) {
	started := time.Now()
	var args execGitArgs
	if err := call.Bind(&args); err != nil {
		return Errorf(call, "git: %v", err), nil
	}
	sub := strings.TrimSpace(args.Subcommand)
	if sub == "" {
		return Errorf(call, "git: subcommand is required"), nil
	}
	if _, ok := execGitAllowlist[sub]; !ok {
		return Errorf(call, "git: subcommand %q is not permitted.\nallowed: %s\nUse the run tool only if the user has explicitly asked for that git operation.",
			sub, strings.Join(execGitSubcommands(), ", ")), nil
	}
	if flag, ok := execGitInteractiveFlag(args.Args); ok {
		return Errorf(call, "git: %s cannot be used here — boop runs git without a terminal, so an interactive session would hang. Use the non-interactive form.", flag), nil
	}
	dir, err := execResolveWorkspaceDir(t.ws, args.WorkingDir)
	if err != nil {
		return Errorf(call, "git: %v", err), nil
	}

	argv := execGitArgv(sub, args.Args)
	display := execDisplayCommand(t.binary(), argv)
	req := execution.RunRequest{
		Command:    t.binary(),
		Args:       argv,
		WorkingDir: dir,
		Timeout:    execTimeout(args.TimeoutSeconds, t.DefaultTimeout, t.MaxTimeout),
		Env: map[string]string{
			// Never block on a credential or passphrase prompt: without a
			// terminal that would hang the tool call until the timeout.
			"GIT_TERMINAL_PROMPT": "0",
			"GIT_PAGER":           "cat",
		},
		MaxOutputBytes: t.MaxOutputBytes,
	}

	res, runErr := t.executor.Run(ctx, req)
	if runErr != nil {
		return Result{
			CallID:   call.ID,
			Tool:     call.Name,
			Content:  fmt.Sprintf("$ %s\nfailed to start: %v", display, runErr),
			Data:     res,
			IsError:  true,
			Duration: time.Since(started),
		}, nil
	}

	maxLines := t.MaxOutputLines
	if maxLines <= 0 {
		maxLines = DefaultMaxOutputLines
	}
	return Result{
		CallID:   call.ID,
		Tool:     call.Name,
		Content:  execFormatRunResult(display, dir, res, maxLines),
		Data:     res,
		IsError:  !res.Success(),
		Duration: time.Since(started),
	}, nil
}

func (t *GitTool) binary() string {
	if t.Binary != "" {
		return t.Binary
	}
	return "git"
}

// execGitArgv builds the argument vector, disabling the pager so output is
// captured rather than waiting for a terminal that does not exist.
func execGitArgv(sub string, args []string) []string {
	argv := make([]string, 0, len(args)+2)
	argv = append(argv, "--no-pager", sub)
	argv = append(argv, args...)
	return argv
}

// execGitSubcommands lists the allowlist in stable order.
func execGitSubcommands() []string {
	subs := make([]string, 0, len(execGitAllowlist))
	for sub := range execGitAllowlist {
		subs = append(subs, sub)
	}
	sort.Strings(subs)
	return subs
}

// execGitClassify escalates the baseline rule when the arguments make the
// operation destructive.
//
// The returned string is a human phrase for the approval prompt; destructive
// operations say so plainly, because "git push" and "git push --force" look
// almost identical in a prompt but are not remotely the same act.
func execGitClassify(sub string, rule execGitRule, args []string) (permissions.Category, permissions.Risk, string) {
	category, risk, what := rule.category, rule.risk, rule.what
	has := func(names ...string) bool { return execHasFlag(args, names...) }

	switch sub {
	case "push":
		if has("--force", "-f", "--force-with-lease", "--mirror", "--delete", "-d") {
			return category, permissions.RiskHigh, "FORCE-PUSH, rewriting history on the remote"
		}
	case "reset":
		if has("--hard") {
			return category, permissions.RiskHigh, "HARD RESET, permanently discarding uncommitted changes"
		}
	case "clean":
		if has("-f", "--force", "-fd", "-fdx", "-fx", "-df", "-xdf", "-dfx") {
			return category, permissions.RiskHigh, "DELETE untracked files from the working tree"
		}
	case "tag":
		if has("-d", "--delete") {
			return category, permissions.RiskHigh, "DELETE a tag"
		}
	case "branch":
		if has("-d", "-D", "--delete", "-m", "-M", "--move") {
			return permissions.CatGitCommit, permissions.RiskHigh, "DELETE or rename a branch"
		}
		if len(args) > 0 && !execAllFlags(args) {
			return permissions.CatGitCommit, permissions.RiskMedium, "create a branch"
		}
	case "commit":
		if has("--amend") {
			return category, permissions.RiskHigh, "AMEND the last commit, rewriting history"
		}
	case "checkout", "restore", "switch":
		if has("-f", "--force", "--hard", "--") {
			return category, permissions.RiskHigh, "OVERWRITE local modifications"
		}
	case "stash":
		if execHasSubcommand(args, "drop", "clear", "pop") {
			return category, permissions.RiskHigh, "DISCARD stashed changes"
		}
	}
	return category, risk, what
}

// execHasFlag reports whether any of names appears as an argument. Combined
// short flags such as -fdx are matched by their individual letters as well.
func execHasFlag(args []string, names ...string) bool {
	for _, arg := range args {
		for _, name := range names {
			if arg == name {
				return true
			}
			// -f matches -fdx, but --force must match exactly.
			if len(name) == 2 && name[0] == '-' && name[1] != '-' &&
				strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") &&
				strings.ContainsRune(arg[1:], rune(name[1])) {
				return true
			}
		}
	}
	return false
}

// execAllFlags reports whether every argument is an option rather than an
// operand.
func execAllFlags(args []string) bool {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			return false
		}
	}
	return true
}

// execHasSubcommand reports whether the first non-flag argument is one of names.
func execHasSubcommand(args []string, names ...string) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		for _, name := range names {
			if arg == name {
				return true
			}
		}
		return false
	}
	return false
}

// execGitInteractiveFlag reports an interactive flag that would hang without a
// terminal.
func execGitInteractiveFlag(args []string) (string, bool) {
	for _, arg := range args {
		switch {
		case arg == "-i" || arg == "--interactive" || arg == "-p" || arg == "--patch":
			return arg, true
		case strings.HasPrefix(arg, "--edit") && arg != "--edit-description":
			return arg, true
		}
	}
	return "", false
}

// execDisplayCommand renders an argv as a copyable command line, quoting only
// what needs it. It is for display and approval prompts; nothing is executed
// from this string.
func execDisplayCommand(name string, argv []string) string {
	parts := make([]string, 0, len(argv)+1)
	parts = append(parts, execShellQuote(name))
	for _, a := range argv {
		parts = append(parts, execShellQuote(a))
	}
	return strings.Join(parts, " ")
}

// execShellQuote single-quotes an argument when it contains anything a shell
// would interpret.
func execShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n\"'\\$`&;|<>()*?![]{}#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
