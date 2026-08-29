package tui

// Status is the coarse activity state shown in the header (§19).
//
// It is derived from bus events rather than set by hand at each call site, so
// the header cannot drift out of step with what the runtime is actually doing.
type Status string

const (
	// StatusIdle means Boop is waiting for the operator.
	StatusIdle Status = "IDLE"
	// StatusThinking means a model request is open and text is streaming.
	StatusThinking Status = "THINKING"
	// StatusPlanning means the model is producing reasoning rather than an
	// answer.
	StatusPlanning Status = "PLANNING"
	// StatusWorking means a tool call is being prepared or evaluated.
	StatusWorking Status = "WORKING"
	// StatusRunning means a command is executing.
	StatusRunning Status = "RUNNING"
	// StatusTesting means a test tool is executing.
	StatusTesting Status = "TESTING"
	// StatusWaiting means Boop is blocked on the operator: an approval.
	StatusWaiting Status = "WAITING"
	// StatusError means the last turn failed.
	StatusError Status = "ERROR"
)

// statusForTool maps a tool name to the activity it represents, so the header
// distinguishes running a command from thinking about one.
func statusForTool(tool string) Status {
	switch tool {
	case "run", "git", "build":
		return StatusRunning
	case "test":
		return StatusTesting
	default:
		return StatusWorking
	}
}

// Busy reports whether the status represents work in flight, which is what
// decides whether Ctrl+C cancels or arms a quit (§51).
func (s Status) Busy() bool {
	switch s {
	case StatusIdle, StatusError:
		return false
	default:
		return true
	}
}
