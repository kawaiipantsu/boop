//go:build windows

package execution

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
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

// signalProcessTree terminates the command and its full process tree.
//
// On Windows, child processes spawned by cmd /C or shell scripts can orphan
// if only the direct child is killed. This terminates the full process tree
// via taskkill /T /F and Windows process termination handles.
func signalProcessTree(cmd *exec.Cmd, force bool) error {
	if cmd.Process == nil {
		return errors.New("execution: process not started")
	}

	pid := cmd.Process.Pid
	if pid > 0 {
		// Use taskkill /T (tree) /F (force) to kill the process and all spawned grandchildren.
		killCmd := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
		if err := killCmd.Run(); err == nil {
			return nil
		}
	}

	// Fallback to direct handle termination via Windows API or Process.Kill
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err == nil {
		defer windows.CloseHandle(h)
		_ = windows.TerminateProcess(h, 1)
		return nil
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
