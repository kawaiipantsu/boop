//go:build unix

package execution

import (
	"errors"
	"os/exec"
	"syscall"
)

// unixShell is the shell used to interpret RunRequest.Command.
//
// /bin/sh is used rather than $SHELL: command execution must be reproducible
// across machines and must not inherit a user's interactive shell functions,
// aliases, or startup files.
const unixShell = "/bin/sh"

// buildCommand constructs the process for req.
//
// When Args is empty the command line is handed to /bin/sh -c, so quoting,
// pipelines, redirection, and globbing behave as the user typed them. When Args
// is non-empty the binary is executed directly with those arguments and no
// shell is involved, so shell metacharacters are passed through literally.
//
// The child is placed in its own process group so that the executor can signal
// the whole tree, not just the immediate child.
func buildCommand(req RunRequest) *exec.Cmd {
	var cmd *exec.Cmd
	if len(req.Args) > 0 {
		cmd = exec.Command(req.Command, req.Args...)
	} else {
		cmd = exec.Command(unixShell, "-c", req.Command)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// signalProcessTree delivers a termination signal to the command's process
// group, escalating to SIGKILL when force is set.
//
// Because buildCommand called setpgid, the group id equals the child's pid, so
// the group can still be addressed after the leader itself has been reaped.
// Signalling the negated pid reaches every descendant that has not deliberately
// left the group, which is what stops `make`, `npm`, or a shell background job
// from outliving the run.
func signalProcessTree(cmd *exec.Cmd, force bool) error {
	if cmd.Process == nil {
		return errors.New("execution: process not started")
	}
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		// The group could not be addressed (for example setpgid failed);
		// fall back to the direct child.
		return cmd.Process.Signal(sig)
	}
	return nil
}

// signalNames maps the portable signals to their conventional names. Go's
// syscall.Signal.String returns prose such as "killed"; models and logs are
// better served by the canonical SIG* spelling.
var signalNames = map[syscall.Signal]string{
	syscall.SIGABRT: "SIGABRT",
	syscall.SIGALRM: "SIGALRM",
	syscall.SIGBUS:  "SIGBUS",
	syscall.SIGFPE:  "SIGFPE",
	syscall.SIGHUP:  "SIGHUP",
	syscall.SIGILL:  "SIGILL",
	syscall.SIGINT:  "SIGINT",
	syscall.SIGKILL: "SIGKILL",
	syscall.SIGPIPE: "SIGPIPE",
	syscall.SIGQUIT: "SIGQUIT",
	syscall.SIGSEGV: "SIGSEGV",
	syscall.SIGSYS:  "SIGSYS",
	syscall.SIGTERM: "SIGTERM",
	syscall.SIGTRAP: "SIGTRAP",
	syscall.SIGXCPU: "SIGXCPU",
	syscall.SIGXFSZ: "SIGXFSZ",
}

// terminationSignal reports the signal that killed the process, if any, as a
// name and its numeric value. It returns ("", 0) for a normal exit.
func terminationSignal(status any) (string, int) {
	ws, ok := status.(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return "", 0
	}
	sig := ws.Signal()
	if name, ok := signalNames[sig]; ok {
		return name, int(sig)
	}
	return sig.String(), int(sig)
}

// envKey normalises an environment variable name for override matching. Unix
// environments are case-sensitive, so the name is used as-is.
func envKey(name string) string { return name }
