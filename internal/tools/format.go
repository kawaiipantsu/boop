package tools

import (
	"context"
	"time"

	"github.com/kawaiipantsu/boop/internal/execution"
	"github.com/kawaiipantsu/boop/internal/permissions"
)

// FormatTool runs the project's source code formatter.
//
// Like TestTool, BuildTool and LintTool, it detects rather than assumes,
// preferring a Makefile target (fmt, format, gofmt) or configured formatter
// (gofmt, ruff format, black, cargo fmt, prettier).
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
		DefaultTimeout: 15 * time.Minute,
		MaxTimeout:     time.Hour,
		MaxOutputBytes: DefaultMaxOutputBytes,
		MaxOutputLines: DefaultMaxOutputLines,
	}
}

// Name implements Tool.
func (t *FormatTool) Name() string { return "format" }

// Description implements Tool.
func (t *FormatTool) Description() string {
	return "Format the project source code or check formatting. The command is detected from the project " +
		"(Makefile target, gofmt, npm run format, cargo fmt, ruff format, black) unless you pass an explicit command."
}

// Schema implements Tool.
func (t *FormatTool) Schema() map[string]any {
	return execTaskSchema("format")
}

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
