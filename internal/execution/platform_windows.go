//go:build windows

package execution

import (
	"errors"
	"os/exec"
	"strings"
	"syscall"
)

// windowsShell is the interpreter used for RunRequest.Command.
const windowsShell = "cmd"

// buildCommand constructs the process for req.
//
// When Args is empty the command line is handed to `cmd /C`. The raw line is
// installed via SysProcAttr.CmdLine rather than as an argument, because Go's
// standard argument quoting follows the C runtime rules that cmd.exe does not
// honour; passing the line verbatim is the only way to keep user quoting,
// pipes, and redirection intact. When Args is non-empty the binary is executed
// directly with those arguments and no interpreter is involved.
//
// Windows has no process groups usable without a job object, so no equivalent
// of setpgid is configured here; see signalProcessTree.
func buildCommand(req RunRequest) *exec.Cmd {
	if len(req.Args) > 0 {
		return exec.Command(req.Command, req.Args...)
	}
	cmd := exec.Command(windowsShell)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: windowsShell + ".exe /C " + req.Command,
	}
	return cmd
}

// signalProcessTree terminates the command.
//
// Windows offers no portable, dependency-free equivalent of killing a process
// group: doing it properly requires assigning the child to a job object via
// golang.org/x/sys/windows, which Boop deliberately does not depend on. The
// guarantee here is therefore weaker than on Unix: the direct child is killed,
// but grandchildren it spawned may survive as orphans. Callers that need a hard
// guarantee on Windows should have the command clean up after itself.
//
// force is ignored because TerminateProcess has no graceful counterpart; the
// executor's grace period simply elapses without effect.
func signalProcessTree(cmd *exec.Cmd, force bool) error {
	if cmd.Process == nil {
		return errors.New("execution: process not started")
	}
	return cmd.Process.Kill()
}

// terminationSignal always reports no signal: Windows terminates processes with
// an exit status rather than a signal.
func terminationSignal(status any) (string, int) { return "", 0 }

// envKey normalises an environment variable name for override matching.
// Windows environment names are case-insensitive, so PATH and Path must be
// treated as the same variable when merging overrides.
func envKey(name string) string { return strings.ToUpper(name) }
