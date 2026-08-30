package tools

import (
	"context"
	"time"

	"github.com/kawaiipantsu/boop/internal/execution"
	"github.com/kawaiipantsu/boop/internal/permissions"
)

// LintTool runs the project's linter or static analysis.
//
// Like TestTool and BuildTool, it detects rather than assumes, preferring a
// Makefile target (lint, vet, staticcheck) or configured linter (golangci-lint,
// ruff, clippy, eslint, phpstan).
type LintTool struct {
	executor execution.Executor
	ws       *Workspace

	Classify       RiskClassifier
	DefaultTimeout time.Duration
	MaxTimeout     time.Duration
	MaxOutputBytes int
	MaxOutputLines int
}

// NewLintTool returns a lint tool backed by executor and confined to ws.
func NewLintTool(executor execution.Executor, ws *Workspace) *LintTool {
	return &LintTool{
		executor:       executor,
		ws:             ws,
		Classify:       DefaultRiskClassifier,
		DefaultTimeout: 15 * time.Minute,
		MaxTimeout:     time.Hour,
		MaxOutputBytes: DefaultMaxOutputBytes,
		MaxOutputLines: DefaultMaxOutputLines,
	}
}

// Name implements Tool.
func (t *LintTool) Name() string { return "lint" }

// Description implements Tool.
func (t *LintTool) Description() string {
	return "Run the project's linter and static analysis. The command is detected from the project " +
		"(Makefile target, golangci-lint, go vet, npm run lint, cargo clippy, ruff check) unless you pass an explicit command. " +
		"Lint warnings are returned as output to diagnose, not as an error."
}

// Schema implements Tool.
func (t *LintTool) Schema() map[string]any {
	return execTaskSchema("lint")
}

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
