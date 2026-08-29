package tools

import (
	"context"
	"time"

	"github.com/kawaiipantsu/boop/internal/execution"
	"github.com/kawaiipantsu/boop/internal/permissions"
)

// BuildTool compiles the project using the project's own build command.
//
// Like TestTool it detects rather than assumes, preferring a Makefile target
// because that is the entry point the project declared for itself. Detection,
// formatting and the shared task plumbing live in test.go; the two tools
// differ only in which target they look for.
type BuildTool struct {
	executor execution.Executor
	ws       *Workspace

	// Classify assesses risk for the permission prompt. Defaults to
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

// NewBuildTool returns a build tool backed by executor and confined to ws.
func NewBuildTool(executor execution.Executor, ws *Workspace) *BuildTool {
	return &BuildTool{
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
func (t *BuildTool) Name() string { return "build" }

// Description implements Tool.
func (t *BuildTool) Description() string {
	return "Build the project. The command is detected from the project (Makefile target, go build, " +
		"npm run build, cargo build) unless you pass an explicit command. " +
		"Compilation errors are returned as output to fix, not as an error."
}

// Schema implements Tool.
func (t *BuildTool) Schema() map[string]any {
	return execTaskSchema("build")
}

// Permission classifies the detected or supplied build command.
func (t *BuildTool) Permission(call Call) (permissions.Action, error) {
	var args execTaskArgs
	if err := call.Bind(&args); err != nil {
		return permissions.Action{}, err
	}
	return execTaskPermission(t.Name(), t.ws, execTaskBuild, args, t.Classify)
}

// Execute detects and runs the build command.
func (t *BuildTool) Execute(ctx context.Context, call Call) (Result, error) {
	return execRunTask(ctx, execTaskConfig{
		kind:           execTaskBuild,
		tool:           t.Name(),
		executor:       t.executor,
		ws:             t.ws,
		defaultTimeout: t.DefaultTimeout,
		maxTimeout:     t.MaxTimeout,
		maxOutputBytes: t.MaxOutputBytes,
		maxOutputLines: t.MaxOutputLines,
	}, call)
}
