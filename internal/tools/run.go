package tools

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/kawaiipantsu/boop/internal/execution"
	"github.com/kawaiipantsu/boop/internal/permissions"
)

// DefaultCommandTimeout bounds a single command. It mirrors the
// agent.command_timeout default from the specification so that a runaway
// process cannot stall the error-repair loop forever.
const DefaultCommandTimeout = 300 * time.Second

// DefaultMaxOutputBytes caps stdout and stderr independently. Output is
// evidence for the model, but an unbounded build log would evict the rest of
// the conversation from the context window.
const DefaultMaxOutputBytes = 256 << 10

// DefaultMaxOutputLines caps how many lines of a captured stream are shown to
// the model. Excess lines are elided from the middle, keeping the start (what
// was attempted) and the end (where it failed), which is where diagnostic
// information almost always lives.
const DefaultMaxOutputLines = 400

// RiskClassifier assesses how dangerous a shell command line is.
//
// It exists as a function type so the conservative built-in heuristic can be
// replaced by the permission engine's full classifier without changing any
// tool.
type RiskClassifier func(command string) permissions.Classification

// RunTool executes a shell command and returns its full structured outcome.
//
// It is the centre of Boop's error-repair loop: a non-zero exit is never a Go
// error and never aborts the exchange. The command, exit code, stdout and
// stderr come back as delimited text the model can read, diagnose and act on.
type RunTool struct {
	executor execution.Executor
	ws       *Workspace

	// Classify assesses command risk for the permission prompt. Defaults to
	// DefaultRiskClassifier; replace it with the permission engine's
	// classifier when one is wired in.
	Classify RiskClassifier
	// DefaultTimeout applies when the caller does not specify one.
	DefaultTimeout time.Duration
	// MaxTimeout caps a model-requested timeout so a tool call cannot pin a
	// process open indefinitely.
	MaxTimeout time.Duration
	// MaxOutputBytes is the per-stream capture cap handed to the executor.
	MaxOutputBytes int
	// MaxOutputLines is the per-stream display cap applied when rendering.
	MaxOutputLines int
}

// NewRunTool returns a run tool backed by executor and confined to ws.
//
// ws is required: it resolves and confines the working directory so a model
// cannot run commands outside the project it was pointed at.
func NewRunTool(executor execution.Executor, ws *Workspace) *RunTool {
	return &RunTool{
		executor:       executor,
		ws:             ws,
		Classify:       DefaultRiskClassifier,
		DefaultTimeout: DefaultCommandTimeout,
		MaxTimeout:     30 * time.Minute,
		MaxOutputBytes: DefaultMaxOutputBytes,
		MaxOutputLines: DefaultMaxOutputLines,
	}
}

// execRunArgs are the decoded arguments of the run tool.
type execRunArgs struct {
	Command        string            `json:"command"`
	WorkingDir     string            `json:"working_dir,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
}

// Name implements Tool.
func (t *RunTool) Name() string { return "run" }

// Description implements Tool.
func (t *RunTool) Description() string {
	return "Run a shell command in the project workspace and return its exit code, stdout and stderr. " +
		"A non-zero exit is returned as data, not an error: read the output, fix the cause and retry."
}

// Schema implements Tool.
func (t *RunTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "Command line to execute with the platform shell.",
			},
			"working_dir": map[string]any{
				"type":        "string",
				"description": "Directory to run in, relative to the workspace root. Defaults to the root.",
			},
			"timeout_seconds": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"description": "Maximum run time in seconds. Defaults to the configured command timeout.",
			},
			"env": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "string"},
				"description":          "Extra environment variables for this command only.",
			},
		},
		"required": []string{"command"},
	}
}

// Permission classifies the command without running it.
//
// Detail carries the exact command line so the approval UI shows precisely
// what would execute; the risk comes from Classify.
func (t *RunTool) Permission(call Call) (permissions.Action, error) {
	var args execRunArgs
	if err := call.Bind(&args); err != nil {
		return permissions.Action{}, err
	}
	command := strings.TrimSpace(args.Command)
	if command == "" {
		return permissions.Action{}, fmt.Errorf("run: command is required")
	}
	dir := t.execWorkingDirLabel(args.WorkingDir)
	// The classifier decides the category as well as the risk: a command run
	// through this tool may mechanically be a git push or a production
	// change, and reporting everything as shell.execute would apply the
	// wrong rule and silently drop the production gate.
	cls := t.execClassify(command)
	return permissions.Action{
		Category:   cls.Category,
		Risk:       cls.Risk,
		Tool:       t.Name(),
		Summary:    fmt.Sprintf("Run in %s: %s", dir, execSummarize(command, 120)),
		Detail:     command,
		Paths:      []string{dir},
		Production: cls.Production,
	}, nil
}

// Execute runs the command and renders the outcome for the model.
//
// It returns a Go error only when the request itself is unusable. Everything
// the model could plausibly repair — a bad working directory, a non-zero exit,
// a timeout — comes back as Result{IsError: true} with explanatory content.
func (t *RunTool) Execute(ctx context.Context, call Call) (Result, error) {
	started := time.Now()
	var args execRunArgs
	if err := call.Bind(&args); err != nil {
		return Errorf(call, "run: %v", err), nil
	}
	command := strings.TrimSpace(args.Command)
	if command == "" {
		return Errorf(call, "run: command is required"), nil
	}
	dir, err := t.execResolveDir(args.WorkingDir)
	if err != nil {
		return Errorf(call, "run: %v", err), nil
	}

	req := execution.RunRequest{
		Command:        command,
		WorkingDir:     dir,
		Timeout:        execTimeout(args.TimeoutSeconds, t.DefaultTimeout, t.MaxTimeout),
		Env:            args.Env,
		MaxOutputBytes: t.MaxOutputBytes,
	}
	res, runErr := t.executor.Run(ctx, req)
	if runErr != nil {
		// The process never started (missing binary, unusable directory).
		// That is diagnosable, so report it as content rather than aborting.
		return Result{
			CallID:   call.ID,
			Tool:     call.Name,
			Content:  fmt.Sprintf("$ %s\nfailed to start: %v", command, runErr),
			Data:     res,
			IsError:  true,
			Duration: time.Since(started),
		}, nil
	}

	return Result{
		CallID:   call.ID,
		Tool:     call.Name,
		Content:  execFormatRunResult(command, dir, res, t.execMaxLines()),
		Data:     res,
		Display:  execOutcome(res),
		IsError:  !res.Success(),
		Duration: time.Since(started),
	}, nil
}

// execOutcome summarises a command result for a watching user. Exit status is
// the thing worth seeing at a glance; the full output is in Content.
func execOutcome(res execution.RunResult) string {
	switch {
	case res.TimedOut:
		return "timed out"
	case res.Cancelled:
		return "cancelled"
	case res.Signal != "":
		return "killed by " + res.Signal
	case res.ExitCode != 0:
		return fmt.Sprintf("exit %d", res.ExitCode)
	}
	if n := countLines(res.Stdout); n > 0 {
		return fmt.Sprintf("exit 0 · %s", plural(n, "line"))
	}
	return "exit 0"
}

// countLines counts non-empty output lines.
func countLines(s string) int {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// plural renders a count with its noun pluralised.
//
// It handles the -es case, because the nouns here include "match" and a
// summary reading "2 matchs" undermines confidence in everything around it.
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	suffix := "s"
	switch {
	case strings.HasSuffix(noun, "ch"), strings.HasSuffix(noun, "sh"),
		strings.HasSuffix(noun, "s"), strings.HasSuffix(noun, "x"),
		strings.HasSuffix(noun, "z"):
		suffix = "es"
	}
	return fmt.Sprintf("%d %s%s", n, noun, suffix)
}

// execClassify applies the configured classifier, defaulting to the
// conservative built-in.
func (t *RunTool) execClassify(command string) permissions.Classification {
	if t.Classify != nil {
		return t.Classify(command)
	}
	return DefaultRiskClassifier(command)
}

func (t *RunTool) execMaxLines() int {
	if t.MaxOutputLines > 0 {
		return t.MaxOutputLines
	}
	return DefaultMaxOutputLines
}

// execResolveDir confines a requested working directory to the workspace.
func (t *RunTool) execResolveDir(dir string) (string, error) {
	return execResolveWorkspaceDir(t.ws, dir)
}

// execWorkingDirLabel renders the working directory for an approval prompt,
// falling back to the raw request when it cannot be resolved so the user still
// sees what was asked for.
func (t *RunTool) execWorkingDirLabel(dir string) string {
	resolved, err := t.execResolveDir(dir)
	if err != nil {
		if dir == "" {
			return "."
		}
		return dir
	}
	return resolved
}

// execResolveWorkspaceDir resolves dir against ws, defaulting to the workspace
// root. A nil workspace means no confinement is configured, in which case the
// caller-supplied directory is used verbatim.
func execResolveWorkspaceDir(ws *Workspace, dir string) (string, error) {
	if ws == nil {
		return dir, nil
	}
	if strings.TrimSpace(dir) == "" {
		return ws.Root(), nil
	}
	resolved, err := ws.Resolve(dir)
	if err != nil {
		return "", fmt.Errorf("working_dir: %w", err)
	}
	return resolved, nil
}

// execTimeout turns a requested number of seconds into a duration, applying
// the default when unset and the ceiling when absurd.
func execTimeout(seconds int, def, max time.Duration) time.Duration {
	d := def
	if d <= 0 {
		d = DefaultCommandTimeout
	}
	if seconds > 0 {
		d = time.Duration(seconds) * time.Second
	}
	if max > 0 && d > max {
		d = max
	}
	return d
}

// execFormatRunResult renders a RunResult as the delimited text handed to the
// model.
//
// The layout is deliberately machine-parseable — `key: value` headers followed
// by fenced stream sections — because this text is the input to the model's
// diagnose/repair step, and ambiguity there costs a whole iteration.
func execFormatRunResult(display, workingDir string, res execution.RunResult, maxLines int) string {
	// The caller's display string wins: it is the full command line as
	// requested, whereas RunResult.Command may hold only the program name
	// when the executor was given an explicit argv.
	command := display
	if command == "" {
		command = res.Command
	}
	dir := workingDir
	if dir == "" {
		dir = res.WorkingDir
	}

	var b strings.Builder
	fmt.Fprintf(&b, "$ %s\n", command)
	fmt.Fprintf(&b, "exit_code: %d\n", res.ExitCode)
	fmt.Fprintf(&b, "duration: %s\n", execFormatDuration(res.Duration))
	if dir != "" {
		fmt.Fprintf(&b, "working_dir: %s\n", dir)
	}
	if res.TimedOut {
		b.WriteString("timed_out: true\n")
	}
	if res.Cancelled {
		b.WriteString("cancelled: true\n")
	}
	if res.Signal != "" {
		fmt.Fprintf(&b, "signal: %s\n", res.Signal)
	}
	b.WriteString(execStreamSection("stdout", res.Stdout, res.StdoutTruncated, maxLines))
	b.WriteString(execStreamSection("stderr", res.Stderr, res.StderrTruncated, maxLines))
	b.WriteString("--- end ---")
	return b.String()
}

// execStreamSection renders one captured stream between stable delimiters.
func execStreamSection(name, body string, truncated bool, maxLines int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "--- %s ---\n", name)
	body = execTrimLines(body, maxLines)
	if body == "" {
		b.WriteString("(empty)\n")
	} else {
		b.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			b.WriteString("\n")
		}
	}
	if truncated {
		fmt.Fprintf(&b, "[%s was truncated at the executor output cap]\n", name)
	}
	return b.String()
}

// execTrimLines keeps the head and tail of s when it exceeds maxLines,
// eliding the middle. Failures are usually explained at the very start or the
// very end of a log, so both ends are preserved.
func execTrimLines(s string, maxLines int) string {
	if maxLines <= 0 || s == "" {
		return s
	}
	trailingNewline := strings.HasSuffix(s, "\n")
	body := strings.TrimSuffix(s, "\n")
	lines := strings.Split(body, "\n")
	if len(lines) <= maxLines {
		return s
	}
	head := maxLines / 2
	tail := maxLines - head
	omitted := len(lines) - head - tail
	kept := make([]string, 0, maxLines+1)
	kept = append(kept, lines[:head]...)
	kept = append(kept, fmt.Sprintf("... [%d lines omitted by boop] ...", omitted))
	kept = append(kept, lines[len(lines)-tail:]...)
	out := strings.Join(kept, "\n")
	if trailingNewline {
		out += "\n"
	}
	return out
}

// execFormatDuration renders a duration at a resolution a human can read.
func execFormatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d < time.Millisecond {
		return d.String()
	}
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(10 * time.Millisecond).String()
}

// execSummarize collapses a command to a single short line for an approval
// prompt, without hiding that it was shortened.
func execSummarize(s string, max int) string {
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
	if max > 0 && len(s) > max {
		return s[:max-1] + "…"
	}
	return s
}

// Conservative fallback risk classification.
//
// These patterns intentionally over-report rather than under-report: a command
// wrongly rated medium only costs an extra confirmation, while one wrongly
// rated low can delete a machine.
var (
	execCriticalPatterns = []*regexp.Regexp{
		regexp.MustCompile(`\brm\s+(-[a-zA-Z]+\s+)*-[a-zA-Z]*[rR][a-zA-Z]*f|\brm\s+(-[a-zA-Z]+\s+)*-[a-zA-Z]*f[a-zA-Z]*[rR]`),
		regexp.MustCompile(`\bmkfs(\.[a-z0-9]+)?\b`),
		regexp.MustCompile(`\bdd\b[^|;&]*\bof=\s*/dev/`),
		regexp.MustCompile(`>\s*/dev/(sd|nvme|hd|disk|vd)`),
		regexp.MustCompile(`:\s*\(\s*\)\s*\{.*\|\s*:\s*&`),
		regexp.MustCompile(`\b(shutdown|reboot|halt|poweroff|init\s+0)\b`),
		regexp.MustCompile(`\b(curl|wget)\b[^|;&]*\|\s*(sudo\s+)?(ba|z|k|da)?sh\b`),
		regexp.MustCompile(`(?i)\bdrop\s+(database|schema)\b`),
	}

	execHighPatterns = []*regexp.Regexp{
		regexp.MustCompile(`\b(sudo|doas)\b`),
		regexp.MustCompile(`\bsu\s+-`),
		regexp.MustCompile(`\brm\s+-`),
		regexp.MustCompile(`\b(shred|truncate)\b`),
		regexp.MustCompile(`\bchmod\s+-R\b|\bchown\s+-R\b`),
		regexp.MustCompile(`\bgit\s+push\b[^;&|]*(--force\b|--force-with-lease\b|\s-f\b|--mirror\b|--delete\b)`),
		regexp.MustCompile(`\bgit\s+reset\b[^;&|]*--hard\b`),
		regexp.MustCompile(`\bgit\s+clean\b[^;&|]*-[a-zA-Z]*[fdx]`),
		regexp.MustCompile(`\bgit\s+(filter-branch|filter-repo)\b`),
		regexp.MustCompile(`\bgit\s+(tag|branch)\b[^;&|]*(-[dD]\b|--delete\b)`),
		regexp.MustCompile(`\b(systemctl|service|launchctl)\b`),
		regexp.MustCompile(`\bdocker\b[^;&|]*\b(rm|rmi|prune|down)\b`),
		regexp.MustCompile(`\bkubectl\b[^;&|]*\b(delete|apply|drain|cordon)\b`),
		regexp.MustCompile(`\b(helm)\b[^;&|]*\b(uninstall|delete|upgrade)\b`),
		regexp.MustCompile(`\bterraform\b[^;&|]*\b(apply|destroy)\b`),
		regexp.MustCompile(`\b(kill|killall|pkill)\b`),
		regexp.MustCompile(`\bcrontab\b`),
		regexp.MustCompile(`\b(iptables|nft|ufw)\b`),
		regexp.MustCompile(`\b(npm|yarn|pnpm|cargo|gem|twine)\s+publish\b`),
		regexp.MustCompile(`(?i)\b(drop|truncate)\s+table\b`),
		regexp.MustCompile(`>\s*/(etc|boot|usr|bin|sbin|var)/`),
		regexp.MustCompile(`\.ssh/(authorized_keys|id_[a-z0-9]+)\b`),
	}

	// execReadOnlyCommands are commands that only observe state. A command
	// qualifies for RiskLow only when every pipeline segment is one of these
	// and nothing redirects, substitutes or deletes.
	execReadOnlyCommands = map[string]bool{
		"basename": true, "cat": true, "cksum": true, "date": true, "df": true,
		"dirname": true, "du": true, "echo": true, "file": true, "grep": true,
		"head": true, "hostname": true, "id": true, "less": true, "ls": true,
		"md5sum": true, "more": true, "nl": true, "printf": true, "pwd": true,
		"readlink": true, "realpath": true, "rg": true, "sha256sum": true,
		"sort": true, "stat": true, "tail": true, "tree": true, "true": true,
		"uname": true, "uniq": true, "wc": true, "which": true, "whoami": true,
	}

	// execReadOnlySubcommands covers tools whose safety depends on the
	// subcommand rather than the binary.
	execReadOnlySubcommands = map[string]map[string]bool{
		"git": {"status": true, "log": true, "diff": true, "show": true,
			"branch": true, "remote": true, "rev-parse": true, "ls-files": true,
			"blame": true, "shortlog": true, "describe": true},
		"go":     {"version": true, "env": true, "list": true, "doc": true},
		"npm":    {"ls": true, "view": true, "outdated": true},
		"cargo":  {"tree": true, "metadata": true},
		"docker": {"ps": true, "images": true, "logs": true},
	}
)

// DefaultRiskClassifier is Boop's conservative fallback command classifier.
//
// It is deliberately simple and pessimistic: obviously destructive patterns
// escalate to high or critical, a small allowlist of read-only commands drops
// to low, and everything else stays at medium. It is a safety net, not a
// sandbox — replace it with the permission engine's classifier when available.
func DefaultRiskClassifier(command string) permissions.Classification {
	c := strings.TrimSpace(command)
	if c == "" {
		return permissions.Classification{Category: permissions.CatShellExecute, Risk: permissions.RiskMedium}
	}
	for _, re := range execCriticalPatterns {
		if re.MatchString(c) {
			return permissions.Classification{Category: permissions.CatShellExecute, Risk: permissions.RiskCritical}
		}
	}
	for _, re := range execHighPatterns {
		if re.MatchString(c) {
			return permissions.Classification{Category: permissions.CatShellExecute, Risk: permissions.RiskHigh}
		}
	}
	if execIsReadOnlyCommand(c) {
		return permissions.Classification{Category: permissions.CatShellExecute, Risk: permissions.RiskLow}
	}
	return permissions.Classification{Category: permissions.CatShellExecute, Risk: permissions.RiskMedium}
}

// execShellSeparators splits a command line into pipeline segments. It is a
// syntactic approximation: it does not honour quoting, which is acceptable
// because misparsing can only ever make the classification more pessimistic.
var execShellSeparators = regexp.MustCompile(`\|\||&&|[|;\n]`)

// execIsReadOnlyCommand reports whether every segment of the command line is a
// known observation-only invocation.
func execIsReadOnlyCommand(command string) bool {
	if strings.ContainsAny(command, "`>") || strings.Contains(command, "$(") {
		return false
	}
	segments := execShellSeparators.Split(command, -1)
	for _, segment := range segments {
		fields := strings.Fields(segment)
		// Skip leading VAR=value assignments.
		for len(fields) > 0 && strings.Contains(fields[0], "=") && !strings.HasPrefix(fields[0], "-") {
			fields = fields[1:]
		}
		if len(fields) == 0 {
			return false
		}
		name := execBaseCommand(fields[0])
		if subs, ok := execReadOnlySubcommands[name]; ok {
			sub := ""
			for _, f := range fields[1:] {
				if !strings.HasPrefix(f, "-") {
					sub = f
					break
				}
			}
			if !subs[sub] {
				return false
			}
			continue
		}
		if name == "find" {
			if strings.Contains(segment, "-delete") || strings.Contains(segment, "-exec") ||
				strings.Contains(segment, "-fprint") {
				return false
			}
			continue
		}
		if !execReadOnlyCommands[name] {
			return false
		}
	}
	return true
}

// execBaseCommand strips any directory prefix from an invoked command.
func execBaseCommand(s string) string {
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		return s[i+1:]
	}
	return s
}
