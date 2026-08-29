// Package execution runs local processes on behalf of the tool runtime.
//
// Platform differences between Windows and Unix shells are encapsulated here;
// no other package may branch on runtime.GOOS for command construction.
package execution

import (
	"time"
)

// RunRequest describes a command to execute.
type RunRequest struct {
	// Command is the command line, interpreted by the platform shell.
	Command string `json:"command"`
	// Args, when non-empty, bypasses shell interpretation and executes
	// Command directly with these arguments.
	Args       []string          `json:"args,omitempty"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Timeout    time.Duration     `json:"timeout,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	// Stdin is optional input written to the process.
	Stdin string `json:"stdin,omitempty"`
	// MaxOutputBytes caps captured stdout and stderr independently. Zero
	// selects the executor default. Truncation is reported on the result.
	MaxOutputBytes int `json:"max_output_bytes,omitempty"`
}

// RunResult is the structured outcome of a command.
//
// A non-zero ExitCode is information, not an error: it is returned to the model
// so it can diagnose and repair. Err is reserved for failures to run at all.
type RunResult struct {
	Command    string        `json:"command"`
	WorkingDir string        `json:"working_dir"`
	ExitCode   int           `json:"exit_code"`
	Stdout     string        `json:"stdout"`
	Stderr     string        `json:"stderr"`
	Duration   time.Duration `json:"duration"`
	TimedOut   bool          `json:"timed_out"`
	Cancelled  bool          `json:"cancelled"`
	// Signal is the terminating signal name where the platform reports one.
	Signal string `json:"signal,omitempty"`
	// StdoutTruncated and StderrTruncated report that output exceeded the cap.
	StdoutTruncated bool      `json:"stdout_truncated,omitempty"`
	StderrTruncated bool      `json:"stderr_truncated,omitempty"`
	StartedAt       time.Time `json:"started_at"`
}

// Success reports whether the command completed normally with exit code zero.
func (r RunResult) Success() bool {
	return r.ExitCode == 0 && !r.TimedOut && !r.Cancelled
}
