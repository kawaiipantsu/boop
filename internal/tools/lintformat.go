package tools

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/kawaiipantsu/boop/internal/execution"
	"github.com/kawaiipantsu/boop/internal/permissions"
)

// LintTool runs the project's own linter or static analysis.
//
// Like TestTool and BuildTool it detects the command rather than guessing: a
// model that has to invent a lint command gets it wrong on an unfamiliar
// project, and routing that guess through the run tool hides the mistake behind
// shell classification. A detected linter only reads the tree, so it is gated
// as filesystem.read; an explicit command override is still classified in full.
type LintTool struct {
	executor execution.Executor
	ws       *Workspace

	// Classify assesses risk for an explicit command override. Defaults to
	// DefaultRiskClassifier.
	Classify RiskClassifier
	// DefaultTimeout applies when the caller does not specify one.
	DefaultTimeout time.Duration
	// MaxTimeout caps a caller-requested timeout.
	MaxTimeout time.Duration
	// MaxOutputBytes is the per-stream capture cap handed to the executor.
	MaxOutputBytes int
	// MaxOutputLines is the per-stream display cap applied when rendering.
	MaxOutputLines int
}

// NewLintTool returns a lint tool backed by executor and confined to ws.
func NewLintTool(executor execution.Executor, ws *Workspace) *LintTool {
	return &LintTool{
		executor:       executor,
		ws:             ws,
		Classify:       DefaultRiskClassifier,
		DefaultTimeout: 10 * time.Minute,
		MaxTimeout:     30 * time.Minute,
		MaxOutputBytes: DefaultMaxOutputBytes,
		MaxOutputLines: DefaultMaxOutputLines,
	}
}

// Name implements Tool.
func (t *LintTool) Name() string { return "lint" }

// Description implements Tool.
func (t *LintTool) Description() string {
	return "Run the project's linter or static analysis. The command is detected from the project " +
		"(Makefile lint target, golangci-lint or go vet, eslint, ruff or flake8, cargo clippy, phpstan) " +
		"unless you pass an explicit command. Findings are returned as output to fix, not as an error."
}

// Schema implements Tool.
func (t *LintTool) Schema() map[string]any { return execTaskSchema("lint") }

// Permission classifies the detected or supplied lint command.
func (t *LintTool) Permission(call Call) (permissions.Action, error) {
	var args execTaskArgs
	if err := call.Bind(&args); err != nil {
		return permissions.Action{}, err
	}
	return execTaskPermission(t.Name(), t.ws, execTaskLint, args, t.Classify)
}

// Execute detects and runs the lint command.
func (t *LintTool) Execute(ctx context.Context, call Call) (Result, error) {
	return execRunTask(ctx, execTaskConfig{
		kind:           execTaskLint,
		tool:           t.Name(),
		executor:       t.executor,
		ws:             t.ws,
		defaultTimeout: t.DefaultTimeout,
		maxTimeout:     t.MaxTimeout,
		maxOutputBytes: t.MaxOutputBytes,
		maxOutputLines: t.MaxOutputLines,
	}, call)
}

// FormatTool runs the project's code formatter.
//
// "Is this formatted" and "format it" are different questions with different
// permission implications, so the tool exposes a `check` flag: a detected check
// run is gated as filesystem.read, a detected rewrite as filesystem.write.
type FormatTool struct {
	executor execution.Executor
	ws       *Workspace

	Classify       RiskClassifier
	DefaultTimeout time.Duration
	MaxTimeout     time.Duration
	MaxOutputBytes int
	MaxOutputLines int
}

// NewFormatTool returns a format tool backed by executor and confined to ws.
func NewFormatTool(executor execution.Executor, ws *Workspace) *FormatTool {
	return &FormatTool{
		executor:       executor,
		ws:             ws,
		Classify:       DefaultRiskClassifier,
		DefaultTimeout: 5 * time.Minute,
		MaxTimeout:     20 * time.Minute,
		MaxOutputBytes: DefaultMaxOutputBytes,
		MaxOutputLines: DefaultMaxOutputLines,
	}
}

// Name implements Tool.
func (t *FormatTool) Name() string { return "format" }

// Description implements Tool.
func (t *FormatTool) Description() string {
	return "Format the project's code, or with check=true report whether it is already formatted " +
		"without changing anything. The command is detected from the project (Makefile fmt target, " +
		"gofmt, prettier, black or ruff format, cargo fmt, php-cs-fixer) unless you pass an explicit command."
}

// Schema implements Tool.
func (t *FormatTool) Schema() map[string]any { return execTaskSchema("format") }

// Permission classifies the detected or supplied format command.
func (t *FormatTool) Permission(call Call) (permissions.Action, error) {
	var args execTaskArgs
	if err := call.Bind(&args); err != nil {
		return permissions.Action{}, err
	}
	return execTaskPermission(t.Name(), t.ws, execTaskFormat, args, t.Classify)
}

// Execute detects and runs the format command.
func (t *FormatTool) Execute(ctx context.Context, call Call) (Result, error) {
	return execRunTask(ctx, execTaskConfig{
		kind:           execTaskFormat,
		tool:           t.Name(),
		executor:       t.executor,
		ws:             t.ws,
		defaultTimeout: t.DefaultTimeout,
		maxTimeout:     t.MaxTimeout,
		maxOutputBytes: t.MaxOutputBytes,
		maxOutputLines: t.MaxOutputLines,
	}, call)
}

// ---------------------------------------------------------------------------
// detection helpers shared with execDetectTask in test.go
// ---------------------------------------------------------------------------

// execReadFile returns a file's contents, or "" if it cannot be read. Detection
// only ever inspects small manifests, so the whole file is fine to read.
func execReadFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// hasKey reports whether m contains key. It tolerates a nil map.
func hasKey(m map[string]string, key string) bool {
	_, ok := m[key]
	return ok
}

// execNodeExec renders "run this locally-installed binary" for a package
// manager: npm has npx, bun has bunx, pnpm and yarn call it through the manager.
func execNodeExec(manager, command string) string {
	switch manager {
	case "pnpm":
		return "pnpm exec " + command
	case "yarn":
		return "yarn " + command
	case "bun":
		return "bunx " + command
	default:
		return "npx " + command
	}
}

// execNodePrettierConfig reports whether a Prettier configuration file is present.
func execNodePrettierConfig(root string) bool {
	names := []string{
		".prettierrc", ".prettierrc.json", ".prettierrc.yml", ".prettierrc.yaml",
		".prettierrc.json5", ".prettierrc.js", ".prettierrc.cjs", ".prettierrc.mjs",
		".prettierrc.toml", "prettier.config.js", "prettier.config.cjs", "prettier.config.mjs",
	}
	for _, n := range names {
		if execFileExists(filepath.Join(root, n)) {
			return true
		}
	}
	return false
}

// execNodeESLintConfig reports whether an ESLint configuration file is present,
// covering both the legacy .eslintrc family and the flat eslint.config.* form.
func execNodeESLintConfig(root string) bool {
	names := []string{
		".eslintrc", ".eslintrc.js", ".eslintrc.cjs", ".eslintrc.json",
		".eslintrc.yml", ".eslintrc.yaml",
		"eslint.config.js", "eslint.config.mjs", "eslint.config.cjs", "eslint.config.ts",
	}
	for _, n := range names {
		if execFileExists(filepath.Join(root, n)) {
			return true
		}
	}
	return false
}

// execDetectPHP recognises a PHP project's lint and format tooling. It does not
// report test or build commands: PHP's are project-specific and the test/build
// tools do not claim PHP support today.
func execDetectPHP(root string, kind execTaskKind, checkOnly bool) (execDetection, bool) {
	isPHP := execFileExists(filepath.Join(root, "composer.json")) ||
		execFileExists(filepath.Join(root, "phpstan.neon")) ||
		execFileExists(filepath.Join(root, "phpstan.neon.dist")) ||
		execFileExists(filepath.Join(root, ".php-cs-fixer.php")) ||
		execFileExists(filepath.Join(root, ".php-cs-fixer.dist.php"))
	if !isPHP {
		return execDetection{}, false
	}

	vendorBin := func(name string) string {
		if execFileExists(filepath.Join(root, "vendor", "bin", name)) {
			return "vendor/bin/" + name
		}
		return name
	}

	switch kind {
	case execTaskLint:
		if execFileExists(filepath.Join(root, "phpstan.neon")) || execFileExists(filepath.Join(root, "phpstan.neon.dist")) {
			return execDetection{Ecosystem: "phpstan", Command: vendorBin("phpstan") + " analyse", Reason: "phpstan.neon present"}, true
		}
		return execDetection{}, false
	case execTaskFormat:
		if execFileExists(filepath.Join(root, ".php-cs-fixer.php")) || execFileExists(filepath.Join(root, ".php-cs-fixer.dist.php")) {
			cmd := vendorBin("php-cs-fixer") + " fix"
			if checkOnly {
				cmd += " --dry-run --diff"
			}
			return execDetection{Ecosystem: "php-cs-fixer", Command: cmd, Reason: ".php-cs-fixer config present"}, true
		}
		return execDetection{}, false
	default:
		return execDetection{}, false
	}
}
