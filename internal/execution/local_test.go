//go:build unix

package execution

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testExecutor returns an executor tuned for fast tests: short kill grace so
// escalation paths do not dominate the suite runtime.
func testExecutor(opts ...Option) *LocalExecutor {
	return NewLocalExecutor(append([]Option{WithKillGrace(20 * time.Millisecond)}, opts...)...)
}

func TestRunExitCodes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		command  string
		wantCode int
		wantOK   bool
	}{
		{name: "success", command: "exit 0", wantCode: 0, wantOK: true},
		{name: "failure", command: "exit 3", wantCode: 3},
		{name: "command not found", command: "boop-no-such-binary-xyz", wantCode: 127},
		{name: "true", command: "true", wantCode: 0, wantOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res, err := testExecutor().Run(context.Background(), RunRequest{
				Command: tt.command,
				Timeout: 5 * time.Second,
			})
			if err != nil {
				t.Fatalf("Run returned error: %v", err)
			}
			if res.ExitCode != tt.wantCode {
				t.Errorf("ExitCode = %d, want %d (stderr=%q)", res.ExitCode, tt.wantCode, res.Stderr)
			}
			if res.Success() != tt.wantOK {
				t.Errorf("Success() = %v, want %v", res.Success(), tt.wantOK)
			}
			if res.TimedOut || res.Cancelled {
				t.Errorf("unexpected TimedOut=%v Cancelled=%v", res.TimedOut, res.Cancelled)
			}
			if res.Command != tt.command {
				t.Errorf("Command = %q, want %q", res.Command, tt.command)
			}
			if res.StartedAt.IsZero() {
				t.Error("StartedAt not populated")
			}
			if res.Duration <= 0 {
				t.Error("Duration not populated")
			}
		})
	}
}

func TestRunCapturesStreamsSeparately(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		command    string
		wantStdout string
		wantStderr string
		wantCode   int
	}{
		{
			name:       "stdout only",
			command:    "echo out",
			wantStdout: "out\n",
		},
		{
			name:       "stderr only",
			command:    "echo err >&2",
			wantStderr: "err\n",
		},
		{
			name:       "both streams and non-zero exit",
			command:    "echo out; echo err >&2; exit 2",
			wantStdout: "out\n",
			wantStderr: "err\n",
			wantCode:   2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res, err := testExecutor().Run(context.Background(), RunRequest{
				Command: tt.command,
				Timeout: 5 * time.Second,
			})
			if err != nil {
				t.Fatalf("Run returned error: %v", err)
			}
			if res.Stdout != tt.wantStdout {
				t.Errorf("Stdout = %q, want %q", res.Stdout, tt.wantStdout)
			}
			if res.Stderr != tt.wantStderr {
				t.Errorf("Stderr = %q, want %q", res.Stderr, tt.wantStderr)
			}
			if res.ExitCode != tt.wantCode {
				t.Errorf("ExitCode = %d, want %d", res.ExitCode, tt.wantCode)
			}
		})
	}
}

func TestRunShellQuotingAndExpansion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    string
	}{
		{name: "double quoted spaces", command: `echo "a  b"`, want: "a  b\n"},
		{name: "single quotes suppress expansion", command: `printf '%s\n' '$HOME'`, want: "$HOME\n"},
		{name: "pipeline", command: `printf 'b\na\n' | sort | tr -d '\n'`, want: "ab"},
		{name: "semicolon sequencing", command: `printf one; printf two`, want: "onetwo"},
		{name: "escaped quote", command: `printf '%s' "say \"hi\""`, want: `say "hi"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res, err := testExecutor().Run(context.Background(), RunRequest{
				Command: tt.command,
				Timeout: 5 * time.Second,
			})
			if err != nil {
				t.Fatalf("Run returned error: %v", err)
			}
			if res.Stdout != tt.want {
				t.Errorf("Stdout = %q, want %q", res.Stdout, tt.want)
			}
		})
	}
}

func TestRunArgsBypassShell(t *testing.T) {
	t.Parallel()
	sentinel := filepath.Join(t.TempDir(), "written-by-shell")
	req := RunRequest{
		Command: "/bin/echo",
		Args:    []string{"$HOME", "a;b", "*", "> " + sentinel},
		Timeout: 5 * time.Second,
	}
	res, err := testExecutor().Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	want := "$HOME a;b * > " + sentinel + "\n"
	if res.Stdout != want {
		t.Fatalf("Stdout = %q, want %q", res.Stdout, want)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("redirection was interpreted by a shell: %v", err)
	}
	if !strings.Contains(res.Command, "/bin/echo") {
		t.Errorf("Command = %q, want it to mention the binary", res.Command)
	}
}

func TestRunMissingBinaryWithArgsReturnsError(t *testing.T) {
	t.Parallel()
	_, err := testExecutor().Run(context.Background(), RunRequest{
		Command: "boop-no-such-binary-xyz",
		Args:    []string{"--version"},
		Timeout: 5 * time.Second,
	})
	if err == nil {
		t.Fatal("expected an error when the binary cannot be started")
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("error = %v, want exec.ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "boop-no-such-binary-xyz") {
		t.Errorf("error = %v, want it to name the binary", err)
	}
}

func TestRunTimeoutKillsProcess(t *testing.T) {
	t.Parallel()
	start := time.Now()
	res, err := testExecutor().Run(context.Background(), RunRequest{
		Command: "sleep 10",
		Timeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !res.TimedOut {
		t.Error("TimedOut = false, want true")
	}
	if res.Cancelled {
		t.Error("Cancelled = true, want false")
	}
	if res.Success() {
		t.Error("Success() = true for a timed-out command")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("run took %v, the process was not killed promptly", elapsed)
	}
	if res.Signal == "" {
		t.Error("Signal not reported for a killed process")
	}
	if res.ExitCode <= 128 {
		t.Errorf("ExitCode = %d, want 128+signal", res.ExitCode)
	}
}

func TestRunContextCancellation(t *testing.T) {
	t.Parallel()
	t.Run("cancelled while running", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(30 * time.Millisecond)
			cancel()
		}()
		defer cancel()
		res, err := testExecutor().Run(ctx, RunRequest{Command: "sleep 10", Timeout: 5 * time.Second})
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if !res.Cancelled {
			t.Error("Cancelled = false, want true")
		}
		if res.TimedOut {
			t.Error("TimedOut = true, want false")
		}
	})

	t.Run("already cancelled does not start", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		marker := filepath.Join(t.TempDir(), "ran")
		res, err := testExecutor().Run(ctx, RunRequest{Command: "touch " + marker})
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if !res.Cancelled {
			t.Error("Cancelled = false, want true")
		}
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Error("command ran despite a cancelled context")
		}
	})

	t.Run("context deadline reports timeout", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		res, err := testExecutor().Run(ctx, RunRequest{Command: "sleep 10"})
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if !res.TimedOut {
			t.Error("TimedOut = false, want true for a context deadline")
		}
		if res.Cancelled {
			t.Error("Cancelled = true, want false for a context deadline")
		}
	})
}

func TestRunKillsGrandchildOnTimeout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	marker := filepath.Join(dir, "grandchild-survived")
	// The background subshell outlives its parent unless the whole process
	// group is killed.
	command := "( sleep 0.2; echo alive > '" + marker + "' ) & sleep 10"

	res, err := testExecutor().Run(context.Background(), RunRequest{
		Command: command,
		Timeout: 40 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !res.TimedOut {
		t.Fatal("TimedOut = false, want true")
	}

	time.Sleep(400 * time.Millisecond)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("grandchild survived the parent's timeout (stat err=%v)", err)
	}
}

func TestRunWorkingDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	t.Run("honoured", func(t *testing.T) {
		res, err := testExecutor().Run(context.Background(), RunRequest{
			Command:    "pwd -P",
			WorkingDir: dir,
			Timeout:    5 * time.Second,
		})
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if got := strings.TrimSpace(res.Stdout); got != resolved {
			t.Errorf("pwd = %q, want %q", got, resolved)
		}
		if res.WorkingDir != dir {
			t.Errorf("WorkingDir = %q, want %q", res.WorkingDir, dir)
		}
	})

	t.Run("defaults to process directory", func(t *testing.T) {
		wd, err := os.Getwd()
		if err != nil {
			t.Fatalf("Getwd: %v", err)
		}
		res, err := testExecutor().Run(context.Background(), RunRequest{Command: "true", Timeout: 5 * time.Second})
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if res.WorkingDir != wd {
			t.Errorf("WorkingDir = %q, want %q", res.WorkingDir, wd)
		}
	})

	t.Run("missing directory is an error", func(t *testing.T) {
		_, err := testExecutor().Run(context.Background(), RunRequest{
			Command:    "true",
			WorkingDir: filepath.Join(dir, "nope"),
		})
		if err == nil {
			t.Fatal("expected an error for a missing working directory")
		}
	})

	t.Run("file instead of directory is an error", func(t *testing.T) {
		file := filepath.Join(dir, "a-file")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		_, err := testExecutor().Run(context.Background(), RunRequest{Command: "true", WorkingDir: file})
		if err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Fatalf("err = %v, want a not-a-directory error", err)
		}
	})
}

func TestRunEnvironmentMerging(t *testing.T) {
	t.Setenv("BOOP_INHERITED", "inherited-value")
	t.Setenv("BOOP_OVERRIDDEN", "original")

	res, err := testExecutor().Run(context.Background(), RunRequest{
		Command: `printf '%s|%s|%s|%s' "$BOOP_INHERITED" "$BOOP_OVERRIDDEN" "$BOOP_EXTRA" "${PATH:+haspath}"`,
		Env: map[string]string{
			"BOOP_OVERRIDDEN": "replaced",
			"BOOP_EXTRA":      "extra-value",
		},
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	want := "inherited-value|replaced|extra-value|haspath"
	if res.Stdout != want {
		t.Errorf("Stdout = %q, want %q", res.Stdout, want)
	}
}

func TestRunStdin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		stdin string
		want  string
	}{
		{name: "text", stdin: "hello stdin", want: "hello stdin"},
		{name: "multiline", stdin: "a\nb\n", want: "a\nb\n"},
		{name: "empty", stdin: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res, err := testExecutor().Run(context.Background(), RunRequest{
				Command: "cat",
				Stdin:   tt.stdin,
				Timeout: 5 * time.Second,
			})
			if err != nil {
				t.Fatalf("Run returned error: %v", err)
			}
			if res.Stdout != tt.want {
				t.Errorf("Stdout = %q, want %q", res.Stdout, tt.want)
			}
			if res.ExitCode != 0 {
				t.Errorf("ExitCode = %d, want 0", res.ExitCode)
			}
		})
	}
}

func TestRunLargeOutputTruncation(t *testing.T) {
	t.Parallel()
	const limit = 1024
	command := `printf HEAD; head -c 200000 /dev/zero | tr '\0' 'x'; printf TAIL; printf HEAD >&2; head -c 200000 /dev/zero | tr '\0' 'y' >&2; printf TAIL >&2`

	res, err := testExecutor().Run(context.Background(), RunRequest{
		Command:        command,
		Timeout:        10 * time.Second,
		MaxOutputBytes: limit,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	for _, tc := range []struct {
		name      string
		got       string
		truncated bool
	}{
		{name: "stdout", got: res.Stdout, truncated: res.StdoutTruncated},
		{name: "stderr", got: res.Stderr, truncated: res.StderrTruncated},
	} {
		if !tc.truncated {
			t.Errorf("%s: truncated flag not set", tc.name)
		}
		if !strings.HasPrefix(tc.got, "HEAD") {
			t.Errorf("%s: head lost, got %.16q", tc.name, tc.got)
		}
		if !strings.HasSuffix(tc.got, "TAIL") {
			t.Errorf("%s: tail lost, got %.16q", tc.name, tc.got[max(0, len(tc.got)-16):])
		}
		if !strings.Contains(tc.got, "bytes elided") {
			t.Errorf("%s: missing elision marker", tc.name)
		}
		if len(tc.got) > limit+len(elisionFormat)+32 {
			t.Errorf("%s: length %d exceeds limit %d plus marker", tc.name, len(tc.got), limit)
		}
	}
}

func TestRunSmallOutputIsNotTruncated(t *testing.T) {
	t.Parallel()
	res, err := testExecutor(WithMaxOutputBytes(64)).Run(context.Background(), RunRequest{
		Command: "printf hello",
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.StdoutTruncated || res.StderrTruncated {
		t.Error("output wrongly reported as truncated")
	}
	if res.Stdout != "hello" {
		t.Errorf("Stdout = %q", res.Stdout)
	}
}

func TestRunDefaultTimeoutOption(t *testing.T) {
	t.Parallel()
	res, err := testExecutor(WithDefaultTimeout(50*time.Millisecond)).
		Run(context.Background(), RunRequest{Command: "sleep 10"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !res.TimedOut {
		t.Error("TimedOut = false, want true from the executor default timeout")
	}
}

func TestRunEmptyCommand(t *testing.T) {
	t.Parallel()
	for _, command := range []string{"", "   ", "\t\n"} {
		if _, err := testExecutor().Run(context.Background(), RunRequest{Command: command}); !errors.Is(err, ErrEmptyCommand) {
			t.Errorf("Run(%q) error = %v, want ErrEmptyCommand", command, err)
		}
	}
}

func TestRunStreaming(t *testing.T) {
	t.Parallel()
	h := &collectHandler{}
	res, err := testExecutor().RunStreaming(context.Background(), RunRequest{
		Command: `for i in 1 2 3; do printf "out%s\n" "$i"; printf "err%s\n" "$i" >&2; sleep 0.01; done`,
		Timeout: 5 * time.Second,
	}, h)
	if err != nil {
		t.Fatalf("RunStreaming returned error: %v", err)
	}

	gotOut, gotErr := h.snapshot()
	if gotOut != res.Stdout {
		t.Errorf("streamed stdout %q != result stdout %q", gotOut, res.Stdout)
	}
	if gotErr != res.Stderr {
		t.Errorf("streamed stderr %q != result stderr %q", gotErr, res.Stderr)
	}
	if res.Stdout != "out1\nout2\nout3\n" {
		t.Errorf("Stdout = %q", res.Stdout)
	}
	if res.Stderr != "err1\nerr2\nerr3\n" {
		t.Errorf("Stderr = %q", res.Stderr)
	}
}

func TestRunStreamingWithSlowHandlerDoesNotDeadlock(t *testing.T) {
	t.Parallel()
	h := &collectHandler{delay: func() { time.Sleep(5 * time.Millisecond) }}
	done := make(chan struct{})
	var res RunResult
	var err error
	go func() {
		defer close(done)
		res, err = testExecutor().RunStreaming(context.Background(), RunRequest{
			Command: `head -c 300000 /dev/zero | tr '\0' 'z'`,
			Timeout: 10 * time.Second,
		}, h)
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("RunStreaming deadlocked behind a slow handler")
	}
	if err != nil {
		t.Fatalf("RunStreaming returned error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if !res.StdoutTruncated {
		t.Error("expected the captured stdout to be truncated")
	}
}

func TestRunStreamingReportsTimeout(t *testing.T) {
	t.Parallel()
	h := &collectHandler{}
	res, err := testExecutor().RunStreaming(context.Background(), RunRequest{
		Command: `printf started; sleep 10`,
		Timeout: 60 * time.Millisecond,
	}, h)
	if err != nil {
		t.Fatalf("RunStreaming returned error: %v", err)
	}
	if !res.TimedOut {
		t.Error("TimedOut = false, want true")
	}
	if out, _ := h.snapshot(); out != "started" {
		t.Errorf("streamed stdout = %q, want %q", out, "started")
	}
}

func TestDisplayCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		req  RunRequest
		want string
	}{
		{name: "shell form is verbatim", req: RunRequest{Command: `echo "a  b" | cat`}, want: `echo "a  b" | cat`},
		{name: "direct args", req: RunRequest{Command: "git", Args: []string{"status", "--short"}}, want: "git status --short"},
		{name: "quoted arg", req: RunRequest{Command: "git", Args: []string{"commit", "-m", "a message"}}, want: `git commit -m "a message"`},
		{name: "empty arg", req: RunRequest{Command: "sh", Args: []string{""}}, want: `sh ""`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := displayCommand(tt.req); got != tt.want {
				t.Errorf("displayCommand() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMergeEnv(t *testing.T) {
	t.Setenv("BOOP_MERGE_TEST", "base")

	t.Run("nil overrides inherit", func(t *testing.T) {
		if got := mergeEnv(nil); got != nil {
			t.Errorf("mergeEnv(nil) = %v, want nil so os/exec inherits", got)
		}
	})

	t.Run("override replaces in place without duplicates", func(t *testing.T) {
		env := mergeEnv(map[string]string{"BOOP_MERGE_TEST": "override", "BOOP_NEW": "new"})
		var seen int
		for _, entry := range env {
			if strings.HasPrefix(entry, "BOOP_MERGE_TEST=") {
				seen++
				if entry != "BOOP_MERGE_TEST=override" {
					t.Errorf("entry = %q, want the override", entry)
				}
			}
		}
		if seen != 1 {
			t.Errorf("BOOP_MERGE_TEST appears %d times, want exactly 1", seen)
		}
		if !containsEntry(env, "BOOP_NEW=new") {
			t.Error("new variable missing from merged environment")
		}
		if len(env) < len(os.Environ()) {
			t.Error("merged environment dropped inherited variables")
		}
	})
}

func containsEntry(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

func TestPickHelpers(t *testing.T) {
	t.Parallel()
	timeouts := []struct {
		name     string
		req, def time.Duration
		want     time.Duration
	}{
		{name: "request wins", req: time.Second, def: time.Minute, want: time.Second},
		{name: "zero falls back", req: 0, def: time.Minute, want: time.Minute},
		{name: "negative request disables", req: -1, def: time.Minute, want: -1},
	}
	for _, tt := range timeouts {
		if got := pickTimeout(tt.req, tt.def); got != tt.want {
			t.Errorf("pickTimeout(%v, %v) = %v, want %v", tt.req, tt.def, got, tt.want)
		}
	}

	limits := []struct {
		name     string
		req, def int
		want     int
	}{
		{name: "request wins", req: 10, def: 20, want: 10},
		{name: "zero falls back", req: 0, def: 20, want: 20},
		{name: "both unset uses default", req: 0, def: 0, want: DefaultMaxOutputBytes},
		{name: "negative request falls back", req: -5, def: 20, want: 20},
	}
	for _, tt := range limits {
		if got := pickOutputLimit(tt.req, tt.def); got != tt.want {
			t.Errorf("pickOutputLimit(%d, %d) = %d, want %d", tt.req, tt.def, got, tt.want)
		}
	}
}

func TestRunReturnsWhenBackgroundChildHoldsPipeOpen(t *testing.T) {
	t.Parallel()
	// The shell exits immediately but leaves a sleeper holding the inherited
	// stdout pipe. Without the drain escalation the read would never see EOF
	// and the run would block for the sleeper's lifetime.
	start := time.Now()
	res, err := testExecutor().Run(context.Background(), RunRequest{
		Command: `( sleep 30 ) & printf done`,
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("run blocked for %v on a lingering background child", elapsed)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if res.Stdout != "done" {
		t.Errorf("Stdout = %q, want %q", res.Stdout, "done")
	}
	if res.TimedOut || res.Cancelled {
		t.Errorf("TimedOut=%v Cancelled=%v, want both false", res.TimedOut, res.Cancelled)
	}
}
