package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kawaiipantsu/boop/internal/execution"
	"github.com/kawaiipantsu/boop/internal/permissions"
)

// TestTool runs the project's own test suite.
//
// It detects the command rather than assuming one: a repository that defines
// `make test` has already stated how it wants to be tested, and second-guessing
// that is how a tool ends up running the wrong suite. Detection is overridable
// with an explicit command.
type TestTool struct {
	executor execution.Executor
	ws       *Workspace

	// Classify assesses risk for the permission prompt. Defaults to
	// DefaultRiskClassifier.
	Classify RiskClassifier
	// DefaultTimeout applies when the caller does not specify one. Test runs
	// are allowed longer than an ordinary command.
	DefaultTimeout time.Duration
	// MaxTimeout caps a caller-requested timeout.
	MaxTimeout time.Duration
	// MaxOutputBytes is the per-stream capture cap handed to the executor.
	MaxOutputBytes int
	// MaxOutputLines is the per-stream display cap applied when rendering.
	MaxOutputLines int
}

// NewTestTool returns a test tool backed by executor and confined to ws.
func NewTestTool(executor execution.Executor, ws *Workspace) *TestTool {
	return &TestTool{
		executor:       executor,
		ws:             ws,
		Classify:       DefaultRiskClassifier,
		DefaultTimeout: 15 * time.Minute,
		MaxTimeout:     time.Hour,
		MaxOutputBytes: DefaultMaxOutputBytes,
		MaxOutputLines: DefaultMaxOutputLines,
	}
}

// execTaskArgs are the decoded arguments shared by the task tools (test,
// build, lint and format).
type execTaskArgs struct {
	Command        string `json:"command,omitempty"`
	WorkingDir     string `json:"working_dir,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	// Check requests a non-mutating run. It is meaningful only for the format
	// task, where "is this formatted" is a read and "format it" is a write.
	Check bool `json:"check,omitempty"`
}

// Name implements Tool.
func (t *TestTool) Name() string { return "test" }

// Description implements Tool.
func (t *TestTool) Description() string {
	return "Run the project's test suite. The command is detected from the project (Makefile target, " +
		"go test, npm test, pytest, cargo test) unless you pass an explicit command. " +
		"Failures are returned as output to diagnose, not as an error."
}

// Schema implements Tool.
func (t *TestTool) Schema() map[string]any {
	return execTaskSchema("test")
}

// Permission classifies the detected or supplied test command.
func (t *TestTool) Permission(call Call) (permissions.Action, error) {
	var args execTaskArgs
	if err := call.Bind(&args); err != nil {
		return permissions.Action{}, err
	}
	return execTaskPermission(t.Name(), t.ws, execTaskTest, args, t.Classify)
}

// Execute detects and runs the test command.
func (t *TestTool) Execute(ctx context.Context, call Call) (Result, error) {
	return execRunTask(ctx, execTaskConfig{
		kind:           execTaskTest,
		tool:           t.Name(),
		executor:       t.executor,
		ws:             t.ws,
		defaultTimeout: t.DefaultTimeout,
		maxTimeout:     t.MaxTimeout,
		maxOutputBytes: t.MaxOutputBytes,
		maxOutputLines: t.MaxOutputLines,
	}, call)
}

// execTaskKind distinguishes the detected project tasks.
type execTaskKind string

const (
	execTaskTest   execTaskKind = "test"
	execTaskBuild  execTaskKind = "build"
	execTaskLint   execTaskKind = "lint"
	execTaskFormat execTaskKind = "format"
)

// execDetection is a project command chosen by detection.
//
// Reason is reported to the model so a wrong guess is visible and correctable
// rather than mysterious.
type execDetection struct {
	Ecosystem string `json:"ecosystem"`
	Command   string `json:"command"`
	Reason    string `json:"reason"`
}

// execTaskConfig carries the per-tool settings into the shared task runner.
type execTaskConfig struct {
	kind           execTaskKind
	tool           string
	executor       execution.Executor
	ws             *Workspace
	defaultTimeout time.Duration
	maxTimeout     time.Duration
	maxOutputBytes int
	maxOutputLines int
}

// execTaskSchema is the shared JSON Schema of the task tools. The format tool
// additionally exposes a `check` flag, since checking and rewriting are
// different questions with different permission implications.
func execTaskSchema(kind string) map[string]any {
	props := map[string]any{
		"command": map[string]any{
			"type": "string",
			"description": "Override the detected " + kind +
				" command. Omit it to use the project's own command.",
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
	}
	if kind == "format" {
		props["check"] = map[string]any{
			"type": "boolean",
			"description": "Report whether the code is formatted without changing any files. " +
				"This is a read-only check; omit it or pass false to actually format.",
		}
	}
	return map[string]any{"type": "object", "properties": props}
}

// execTaskPermission classifies a detected or overridden task command.
//
// A command supplied by the caller is always run through the risk classifier —
// the tool has no idea what it is. A command the tool detected for itself is
// known: a detected lint or format check only reads the tree, and a detected
// format run rewrites files in place, so those are filed as filesystem.read and
// filesystem.write rather than sent through shell classification. test and
// build stay classified because their commands do arbitrary work.
func execTaskPermission(tool string, ws *Workspace, kind execTaskKind, args execTaskArgs, classify RiskClassifier) (permissions.Action, error) {
	dir, err := execResolveWorkspaceDir(ws, args.WorkingDir)
	if err != nil {
		dir = args.WorkingDir
	}
	if classify == nil {
		classify = DefaultRiskClassifier
	}
	checkOnly := args.Check && kind == execTaskFormat

	command := strings.TrimSpace(args.Command)
	explicit := command != ""
	origin := "explicit"
	if !explicit {
		det, ok := execDetectTask(dir, kind, checkOnly)
		if !ok {
			// Nothing detected: the call will fail in Execute, but it must
			// still classify, so treat it as an ordinary shell action.
			return permissions.Action{
				Category: permissions.CatShellExecute,
				Risk:     permissions.RiskMedium,
				Tool:     tool,
				Summary:  fmt.Sprintf("No %s command detected in %s", kind, dir),
				Paths:    []string{dir},
			}, nil
		}
		command, origin = det.Command, det.Ecosystem
	}

	summary := fmt.Sprintf("Run %s (%s) in %s: %s", kind, origin, dir, execSummarize(command, 100))
	if checkOnly {
		summary = fmt.Sprintf("Check %s (%s) in %s: %s", kind, origin, dir, execSummarize(command, 100))
	}

	if !explicit && (kind == execTaskLint || (kind == execTaskFormat && checkOnly)) {
		return permissions.Action{
			Category: permissions.CatFilesystemRead,
			Risk:     permissions.RiskLow,
			Tool:     tool,
			Summary:  summary,
			Detail:   command,
			Paths:    []string{dir},
		}, nil
	}
	if !explicit && kind == execTaskFormat {
		return permissions.Action{
			Category: permissions.CatFilesystemWrite,
			Risk:     permissions.RiskMedium,
			Tool:     tool,
			Summary:  summary,
			Detail:   command,
			Paths:    []string{dir},
		}, nil
	}

	// Explicit override, or test/build: an arbitrary command line, classified
	// like any other rather than trusted for the tool it was passed to.
	cls := classify(command)
	return permissions.Action{
		Category:   cls.Category,
		Risk:       cls.Risk,
		Tool:       tool,
		Summary:    summary,
		Detail:     command,
		Paths:      []string{dir},
		Production: cls.Production,
	}, nil
}

// execRunTask is the shared body of the test and build tools.
func execRunTask(ctx context.Context, cfg execTaskConfig, call Call) (Result, error) {
	started := time.Now()
	var args execTaskArgs
	if err := call.Bind(&args); err != nil {
		return Errorf(call, "%s: %v", cfg.kind, err), nil
	}
	dir, err := execResolveWorkspaceDir(cfg.ws, args.WorkingDir)
	if err != nil {
		return Errorf(call, "%s: %v", cfg.kind, err), nil
	}
	checkOnly := args.Check && cfg.kind == execTaskFormat

	det := execDetection{Ecosystem: "explicit", Command: strings.TrimSpace(args.Command), Reason: "command supplied by the caller"}
	if det.Command == "" {
		found, ok := execDetectTask(dir, cfg.kind, checkOnly)
		if !ok {
			return Errorf(call, "%s: no %s command could be detected in %s.\n"+
				"Looked for: a Makefile %s target, go.mod, package.json scripts, Cargo.toml, and Python project markers.\n"+
				"Pass an explicit \"command\" argument to say how this project is %s.",
				cfg.kind, cfg.kind, dir, cfg.kind, execTaskParticiple(cfg.kind)), nil
		}
		det = found
	}

	req := execution.RunRequest{
		Command:        det.Command,
		WorkingDir:     dir,
		Timeout:        execTimeout(args.TimeoutSeconds, cfg.defaultTimeout, cfg.maxTimeout),
		MaxOutputBytes: cfg.maxOutputBytes,
	}
	res, runErr := cfg.executor.Run(ctx, req)
	if runErr != nil {
		return Result{
			CallID:   call.ID,
			Tool:     call.Name,
			Content:  fmt.Sprintf("%s: %s\n$ %s\nfailed to start: %v", cfg.kind, det.Ecosystem, det.Command, runErr),
			Data:     res,
			IsError:  true,
			Duration: time.Since(started),
		}, nil
	}

	// Some formatters report "would reformat" on stdout and still exit 0
	// (`gofmt -l`, notably). In check mode a non-empty listing means the tree
	// is not formatted, which the caller needs to see as a failure.
	unformatted := checkOnly && res.Success() && strings.TrimSpace(res.Stdout) != ""

	maxLines := cfg.maxOutputLines
	if maxLines <= 0 {
		maxLines = DefaultMaxOutputLines
	}
	content := execFormatTaskResult(cfg.kind, det, dir, res, maxLines)
	if unformatted {
		content = "format: NEEDS FORMATTING\n" +
			"runner: " + det.Ecosystem + " (" + det.Reason + ")\n" +
			"the following files are not formatted:\n" +
			execTrimLines(res.Stdout, maxLines) +
			"\nrun format without check to fix them"
	}
	return Result{
		CallID:   call.ID,
		Tool:     call.Name,
		Content:  content,
		Data:     res,
		IsError:  !res.Success() || unformatted,
		Duration: time.Since(started),
	}, nil
}

func execTaskParticiple(kind execTaskKind) string {
	switch kind {
	case execTaskBuild:
		return "built"
	case execTaskLint:
		return "linted"
	case execTaskFormat:
		return "formatted"
	default:
		return "tested"
	}
}

// execFormatTaskResult renders a task outcome, leading with the verdict and
// how the command was chosen.
func execFormatTaskResult(kind execTaskKind, det execDetection, dir string, res execution.RunResult, maxLines int) string {
	verdict := "PASS"
	if !res.Success() {
		verdict = "FAIL"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n", kind, verdict)
	fmt.Fprintf(&b, "runner: %s (%s)\n", det.Ecosystem, det.Reason)
	b.WriteString(execFormatRunResult(det.Command, dir, res, maxLines))
	return b.String()
}

// execDetectTask picks the command this project uses for the given task.
//
// Make wins when it defines the target: a Makefile target is the project's own
// declared entry point and usually wraps flags, code generation or environment
// the raw toolchain command would skip. Otherwise the first matching ecosystem
// marker in a fixed order is used, so detection is deterministic. checkOnly
// selects the non-mutating form and applies only to the format task.
func execDetectTask(root string, kind execTaskKind, checkOnly bool) (execDetection, bool) {
	if root == "" {
		return execDetection{}, false
	}
	if path, ok := execFindMakefile(root); ok {
		if det, ok := execMakeTaskTarget(filepath.Base(path), execMakeTargets(path), kind, checkOnly); ok {
			return det, true
		}
	}

	if execFileExists(filepath.Join(root, "go.mod")) {
		return execGoTask(root, kind, checkOnly), true
	}

	if det, ok := execDetectNode(root, kind, checkOnly); ok {
		return det, true
	}

	if execFileExists(filepath.Join(root, "Cargo.toml")) {
		return execCargoTask(kind, checkOnly), true
	}

	if det, ok := execDetectPython(root, kind, checkOnly); ok {
		return det, true
	}

	if det, ok := execDetectPHP(root, kind, checkOnly); ok {
		return det, true
	}

	return execDetection{}, false
}

// execMakeTargetNames lists the Makefile target names that stand for a task
// kind, most conventional first. A format check has its own names because a
// bare `format` target usually rewrites files.
func execMakeTargetNames(kind execTaskKind, checkOnly bool) []string {
	switch {
	case kind == execTaskFormat && checkOnly:
		return []string{"format-check", "fmt-check", "check-format", "check-fmt"}
	case kind == execTaskFormat:
		return []string{"format", "fmt"}
	case kind == execTaskLint:
		return []string{"lint", "vet"}
	default:
		return []string{string(kind)}
	}
}

// execMakeTaskTarget returns the first declared Make target that fits the task.
func execMakeTaskTarget(makefile string, targets map[string]bool, kind execTaskKind, checkOnly bool) (execDetection, bool) {
	for _, name := range execMakeTargetNames(kind, checkOnly) {
		if targets[name] {
			return execDetection{
				Ecosystem: "make",
				Command:   "make " + name,
				Reason:    makefile + " defines a " + name + " target",
			}, true
		}
	}
	return execDetection{}, false
}

// execGoTask maps a task kind to the Go toolchain command. Go always has one.
func execGoTask(root string, kind execTaskKind, checkOnly bool) execDetection {
	switch kind {
	case execTaskBuild:
		return execDetection{Ecosystem: "go", Command: "go build ./...", Reason: "go.mod present"}
	case execTaskLint:
		for _, f := range []string{".golangci.yml", ".golangci.yaml", ".golangci.toml", ".golangci.json"} {
			if execFileExists(filepath.Join(root, f)) {
				return execDetection{Ecosystem: "golangci-lint", Command: "golangci-lint run", Reason: f + " present"}
			}
		}
		return execDetection{Ecosystem: "go", Command: "go vet ./...", Reason: "go.mod present, no golangci-lint config"}
	case execTaskFormat:
		if checkOnly {
			return execDetection{Ecosystem: "gofmt", Command: "gofmt -l .", Reason: "go.mod present"}
		}
		return execDetection{Ecosystem: "gofmt", Command: "gofmt -w .", Reason: "go.mod present"}
	default:
		return execDetection{Ecosystem: "go", Command: "go test ./...", Reason: "go.mod present"}
	}
}

// execCargoTask maps a task kind to the Cargo command. Cargo always has one.
func execCargoTask(kind execTaskKind, checkOnly bool) execDetection {
	switch kind {
	case execTaskBuild:
		return execDetection{Ecosystem: "cargo", Command: "cargo build", Reason: "Cargo.toml present"}
	case execTaskLint:
		return execDetection{Ecosystem: "cargo", Command: "cargo clippy", Reason: "Cargo.toml present"}
	case execTaskFormat:
		if checkOnly {
			return execDetection{Ecosystem: "cargo", Command: "cargo fmt --check", Reason: "Cargo.toml present"}
		}
		return execDetection{Ecosystem: "cargo", Command: "cargo fmt", Reason: "Cargo.toml present"}
	default:
		return execDetection{Ecosystem: "cargo", Command: "cargo test", Reason: "Cargo.toml present"}
	}
}

// execPythonMarkers are files that identify a Python project.
var execPythonMarkers = []string{"pyproject.toml", "setup.py", "setup.cfg", "pytest.ini", "tox.ini", "requirements.txt"}

// execDetectPython recognises a Python project. Python has no universal build
// step, so only pyproject.toml offers one; lint and format are reported only
// when a tool is actually configured, since guessing between ruff, black and
// flake8 is worse than saying nothing.
func execDetectPython(root string, kind execTaskKind, checkOnly bool) (execDetection, bool) {
	marker := ""
	for _, name := range execPythonMarkers {
		if execFileExists(filepath.Join(root, name)) {
			marker = name
			break
		}
	}
	if marker == "" {
		return execDetection{}, false
	}
	pyproj := execReadFile(filepath.Join(root, "pyproject.toml"))

	switch kind {
	case execTaskBuild:
		if marker == "pyproject.toml" {
			return execDetection{Ecosystem: "python", Command: "python -m build", Reason: "pyproject.toml present"}, true
		}
		return execDetection{}, false
	case execTaskLint:
		if strings.Contains(pyproj, "[tool.ruff") {
			return execDetection{Ecosystem: "ruff", Command: "ruff check .", Reason: "pyproject.toml configures ruff"}, true
		}
		if execFileExists(filepath.Join(root, ".flake8")) ||
			strings.Contains(execReadFile(filepath.Join(root, "setup.cfg")), "[flake8]") ||
			strings.Contains(pyproj, "[tool.flake8") {
			return execDetection{Ecosystem: "flake8", Command: "flake8", Reason: "flake8 configuration present"}, true
		}
		return execDetection{}, false
	case execTaskFormat:
		if strings.Contains(pyproj, "[tool.ruff") {
			cmd := "ruff format ."
			if checkOnly {
				cmd = "ruff format --check ."
			}
			return execDetection{Ecosystem: "ruff", Command: cmd, Reason: "pyproject.toml configures ruff"}, true
		}
		if strings.Contains(pyproj, "[tool.black") {
			cmd := "black ."
			if checkOnly {
				cmd = "black --check ."
			}
			return execDetection{Ecosystem: "black", Command: cmd, Reason: "pyproject.toml configures black"}, true
		}
		return execDetection{}, false
	default:
		return execDetection{Ecosystem: "python", Command: "pytest", Reason: marker + " present"}, true
	}
}

// execDetectNode reads package.json and only reports a command the project
// actually defines, so the tool never runs a script that does not exist. For a
// format check with no declared check script it falls back to Prettier's own
// --check when Prettier is configured.
func execDetectNode(root string, kind execTaskKind, checkOnly bool) (execDetection, bool) {
	path := filepath.Join(root, "package.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return execDetection{}, false
	}
	var pkg struct {
		Scripts         map[string]string `json:"scripts"`
		DevDependencies map[string]string `json:"devDependencies"`
		Dependencies    map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return execDetection{}, false
	}
	manager := execNodePackageManager(root)
	runScript := func(name string) execDetection {
		command := manager + " run " + name
		if name == "test" && manager == "npm" {
			command = "npm test"
		}
		return execDetection{Ecosystem: manager, Command: command, Reason: "package.json defines a " + name + " script"}
	}

	if kind == execTaskFormat && checkOnly {
		for _, name := range []string{"format:check", "fmt:check", "format-check"} {
			if _, ok := pkg.Scripts[name]; ok {
				return runScript(name), true
			}
		}
		if _, dev := pkg.DevDependencies["prettier"]; dev || hasKey(pkg.Dependencies, "prettier") || execNodePrettierConfig(root) {
			return execDetection{
				Ecosystem: manager,
				Command:   execNodeExec(manager, "prettier --check ."),
				Reason:    "Prettier configured; no format:check script",
			}, true
		}
		return execDetection{}, false
	}

	if _, ok := pkg.Scripts[string(kind)]; ok {
		return runScript(string(kind)), true
	}

	// eslint with a flat or legacy config but no lint script is still a linter
	// the project clearly opted into.
	if kind == execTaskLint && execNodeESLintConfig(root) {
		return execDetection{
			Ecosystem: manager,
			Command:   execNodeExec(manager, "eslint ."),
			Reason:    "ESLint configuration present; no lint script",
		}, true
	}
	return execDetection{}, false
}

// execNodePackageManager infers the package manager from the lockfile, so the
// project's own toolchain is used rather than whatever happens to be on PATH.
func execNodePackageManager(root string) string {
	switch {
	case execFileExists(filepath.Join(root, "pnpm-lock.yaml")):
		return "pnpm"
	case execFileExists(filepath.Join(root, "yarn.lock")):
		return "yarn"
	case execFileExists(filepath.Join(root, "bun.lockb")), execFileExists(filepath.Join(root, "bun.lock")):
		return "bun"
	default:
		return "npm"
	}
}

// execMakefileNames are the filenames GNU make itself looks for, in order.
var execMakefileNames = []string{"GNUmakefile", "makefile", "Makefile"}

// execFindMakefile returns the makefile in root, if any.
func execFindMakefile(root string) (string, bool) {
	for _, name := range execMakefileNames {
		path := filepath.Join(root, name)
		if execFileExists(path) {
			return path, true
		}
	}
	return "", false
}

// execMakeTargetLine matches a rule line, capturing the target names.
//
// It ignores pattern rules, variable assignments and recipe lines. This is a
// text scan, not a make parser: targets produced by include files or variable
// expansion are not seen, which is why detection falls back to the toolchain.
var execMakeTargetLine = regexp.MustCompile(`^([A-Za-z0-9_][A-Za-z0-9_.\-/ ]*):(?:[^=]|$)`)

// execMakeTargets returns the set of explicit targets declared in a makefile,
// including those named only in .PHONY.
func execMakeTargets(path string) map[string]bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	targets := make(map[string]bool)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "\t") || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, ".PHONY:"); ok {
			for _, name := range strings.Fields(rest) {
				targets[name] = true
			}
			continue
		}
		m := execMakeTargetLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		for _, name := range strings.Fields(m[1]) {
			if strings.ContainsAny(name, "%$") || strings.HasPrefix(name, ".") {
				continue
			}
			targets[name] = true
		}
	}
	return targets
}

// execFileExists reports whether path exists as a regular file.
func execFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
