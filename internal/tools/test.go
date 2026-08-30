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

// execTaskArgs are the decoded arguments shared by the test and build tools.
type execTaskArgs struct {
	Command        string `json:"command,omitempty"`
	WorkingDir     string `json:"working_dir,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
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

// execTaskKind distinguishes the two detected project tasks.
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

// execTaskSchema is the shared JSON Schema of the test and build tools.
func execTaskSchema(kind string) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
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
		},
	}
}

// execTaskPermission classifies a detected or overridden task command.
func execTaskPermission(tool string, ws *Workspace, kind execTaskKind, args execTaskArgs, classify RiskClassifier) (permissions.Action, error) {
	dir, err := execResolveWorkspaceDir(ws, args.WorkingDir)
	if err != nil {
		dir = args.WorkingDir
	}
	command := strings.TrimSpace(args.Command)
	origin := "explicit"
	if command == "" {
		det, ok := execDetectTask(dir, kind)
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
	if classify == nil {
		classify = DefaultRiskClassifier
	}
	// A project's own test or build command is still an arbitrary command
	// line, so it is classified like any other rather than trusted for
	// being called "test".
	cls := classify(command)
	return permissions.Action{
		Category:   cls.Category,
		Risk:       cls.Risk,
		Tool:       tool,
		Summary:    fmt.Sprintf("Run %s (%s) in %s: %s", kind, origin, dir, execSummarize(command, 100)),
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

	det := execDetection{Ecosystem: "explicit", Command: strings.TrimSpace(args.Command), Reason: "command supplied by the caller"}
	if det.Command == "" {
		found, ok := execDetectTask(dir, cfg.kind)
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

	maxLines := cfg.maxOutputLines
	if maxLines <= 0 {
		maxLines = DefaultMaxOutputLines
	}
	return Result{
		CallID:   call.ID,
		Tool:     call.Name,
		Content:  execFormatTaskResult(cfg.kind, det, dir, res, maxLines),
		Data:     res,
		IsError:  !res.Success(),
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
// marker in a fixed order is used, so detection is deterministic.
func execDetectTask(root string, kind execTaskKind) (execDetection, bool) {
	if root == "" {
		return execDetection{}, false
	}
	target := string(kind)
	if path, ok := execFindMakefile(root); ok {
		targets := execMakeTargets(path)
		if targets[target] {
			return execDetection{
				Ecosystem: "make",
				Command:   "make " + target,
				Reason:    filepath.Base(path) + " defines a " + target + " target",
			}, true
		}
		var alts []string
		switch kind {
		case execTaskLint:
			alts = []string{"vet", "staticcheck", "check"}
		case execTaskFormat:
			alts = []string{"fmt", "gofmt"}
		case execTaskTest:
			alts = []string{"tests", "check", "test-unit"}
		case execTaskBuild:
			alts = []string{"compile", "all"}
		}
		for _, alt := range alts {
			if targets[alt] {
				return execDetection{
					Ecosystem: "make",
					Command:   "make " + alt,
					Reason:    filepath.Base(path) + " defines a " + alt + " target",
				}, true
			}
		}
	}

	if execFileExists(filepath.Join(root, "go.mod")) {
		switch kind {
		case execTaskBuild:
			return execDetection{Ecosystem: "go", Command: "go build ./...", Reason: "go.mod present"}, true
		case execTaskLint:
			for _, f := range []string{".golangci.yml", ".golangci.yaml", ".golangci.toml", ".golangci.json"} {
				if execFileExists(filepath.Join(root, f)) {
					return execDetection{Ecosystem: "golangci-lint", Command: "golangci-lint run", Reason: f + " present"}, true
				}
			}
			return execDetection{Ecosystem: "go", Command: "go vet ./...", Reason: "go.mod present"}, true
		case execTaskFormat:
			return execDetection{Ecosystem: "gofmt", Command: "gofmt -w .", Reason: "go.mod present"}, true
		default:
			return execDetection{Ecosystem: "go", Command: "go test ./...", Reason: "go.mod present"}, true
		}
	}

	if det, ok := execDetectNode(root, kind); ok {
		return det, true
	}

	if execFileExists(filepath.Join(root, "Cargo.toml")) {
		switch kind {
		case execTaskBuild:
			return execDetection{Ecosystem: "cargo", Command: "cargo build", Reason: "Cargo.toml present"}, true
		case execTaskLint:
			return execDetection{Ecosystem: "cargo", Command: "cargo clippy", Reason: "Cargo.toml present"}, true
		case execTaskFormat:
			return execDetection{Ecosystem: "cargo", Command: "cargo fmt", Reason: "Cargo.toml present"}, true
		default:
			return execDetection{Ecosystem: "cargo", Command: "cargo test", Reason: "Cargo.toml present"}, true
		}
	}

	if det, ok := execDetectPython(root, kind); ok {
		return det, true
	}

	return execDetection{}, false
}

// execPythonMarkers are files that identify a Python project.
var execPythonMarkers = []string{"pyproject.toml", "setup.py", "setup.cfg", "pytest.ini", "tox.ini", "requirements.txt"}

// execDetectPython recognises a Python project. Python has no universal build
// step, so only pyproject.toml offers one.
func execDetectPython(root string, kind execTaskKind) (execDetection, bool) {
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
	var pyproj string
	if data, err := os.ReadFile(filepath.Join(root, "pyproject.toml")); err == nil {
		pyproj = string(data)
	}

	switch kind {
	case execTaskBuild:
		if marker == "pyproject.toml" {
			return execDetection{Ecosystem: "python", Command: "python -m build", Reason: "pyproject.toml present"}, true
		}
		return execDetection{}, false
	case execTaskLint:
		if strings.Contains(pyproj, "[tool.ruff") {
			return execDetection{Ecosystem: "python", Command: "ruff check .", Reason: "pyproject.toml defines ruff"}, true
		}
		if strings.Contains(pyproj, "[tool.mypy") {
			return execDetection{Ecosystem: "python", Command: "mypy .", Reason: "pyproject.toml defines mypy"}, true
		}
		return execDetection{Ecosystem: "python", Command: "flake8", Reason: marker + " present"}, true
	case execTaskFormat:
		if strings.Contains(pyproj, "[tool.ruff") {
			return execDetection{Ecosystem: "python", Command: "ruff format .", Reason: "pyproject.toml defines ruff"}, true
		}
		if strings.Contains(pyproj, "[tool.black") {
			return execDetection{Ecosystem: "python", Command: "black .", Reason: "pyproject.toml defines black"}, true
		}
		return execDetection{Ecosystem: "python", Command: "black .", Reason: marker + " present"}, true
	default:
		return execDetection{Ecosystem: "python", Command: "pytest", Reason: marker + " present"}, true
	}
}

// execDetectNode reads package.json and only reports a command the project
// actually defines, so the tool never runs a script that does not exist.
func execDetectNode(root string, kind execTaskKind) (execDetection, bool) {
	path := filepath.Join(root, "package.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return execDetection{}, false
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return execDetection{}, false
	}

	var candidates []string
	switch kind {
	case execTaskLint:
		candidates = []string{"lint", "typecheck", "eslint"}
	case execTaskFormat:
		candidates = []string{"format", "fmt", "prettier"}
	default:
		candidates = []string{string(kind)}
	}

	foundScript := ""
	for _, c := range candidates {
		if _, ok := pkg.Scripts[c]; ok {
			foundScript = c
			break
		}
	}
	if foundScript == "" {
		return execDetection{}, false
	}

	manager := execNodePackageManager(root)
	command := manager + " run " + foundScript
	if foundScript == "test" && manager == "npm" {
		command = "npm test"
	}
	return execDetection{
		Ecosystem: manager,
		Command:   command,
		Reason:    "package.json defines a " + foundScript + " script",
	}, true
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
