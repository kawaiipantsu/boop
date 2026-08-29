package execution

import "context"

// Executor runs commands on behalf of the tool runtime.
//
// Implementations must return a populated RunResult for any command that ran,
// including one that failed: a non-zero exit is data for the model, not an
// error. A non-nil error means the command could not be started at all.
type Executor interface {
	Run(ctx context.Context, req RunRequest) (RunResult, error)
}

// StreamHandler receives incremental output while a command runs.
//
// It is called from the executor's goroutines and must not block.
type StreamHandler interface {
	OnStdout(chunk string)
	OnStderr(chunk string)
}

// StreamingExecutor is implemented by executors that can report output as it
// arrives, so the TUI and WebUI can show a command live.
type StreamingExecutor interface {
	Executor
	RunStreaming(ctx context.Context, req RunRequest, h StreamHandler) (RunResult, error)
}
