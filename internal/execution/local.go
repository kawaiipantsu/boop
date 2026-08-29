package execution

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultTimeout bounds a command that specifies no timeout of its own. It
// mirrors the agent.command_timeout default from the specification: long enough
// for a build, short enough that a hung process cannot wedge a session.
const DefaultTimeout = 300 * time.Second

// DefaultKillGrace is how long a process is given to exit after the polite
// termination signal before it is killed outright.
const DefaultKillGrace = 250 * time.Millisecond

// readChunkSize is the read buffer used per output stream. Streaming consumers
// receive chunks of at most this size.
const readChunkSize = 32 * 1024

// ErrEmptyCommand is returned when a request carries no command to run.
var ErrEmptyCommand = errors.New("execution: empty command")

// LocalExecutor runs commands as child processes of the Boop process.
//
// It implements both Executor and StreamingExecutor. Every command that manages
// to start yields a populated RunResult with a nil error, including commands
// that fail, time out, or are cancelled: failure is data the model repairs
// from, not a Go error. An error is returned only when the process could not be
// started at all.
//
// The zero value is not usable; construct with NewLocalExecutor.
type LocalExecutor struct {
	defaultTimeout time.Duration
	maxOutputBytes int
	killGrace      time.Duration
}

// Option configures a LocalExecutor.
type Option func(*LocalExecutor)

// WithDefaultTimeout sets the timeout applied to requests that leave
// RunRequest.Timeout at zero. A negative value disables the default, letting
// such requests run until they finish or their context is cancelled.
func WithDefaultTimeout(d time.Duration) Option {
	return func(e *LocalExecutor) { e.defaultTimeout = d }
}

// WithMaxOutputBytes sets the per-stream capture cap used when a request leaves
// RunRequest.MaxOutputBytes at zero. A non-positive value restores
// DefaultMaxOutputBytes; capture is never unbounded, because a runaway command
// must not be able to exhaust memory.
func WithMaxOutputBytes(n int) Option {
	return func(e *LocalExecutor) {
		if n <= 0 {
			n = DefaultMaxOutputBytes
		}
		e.maxOutputBytes = n
	}
}

// WithKillGrace sets how long the executor waits between the graceful
// termination signal and the forced kill. A non-positive value restores
// DefaultKillGrace.
func WithKillGrace(d time.Duration) Option {
	return func(e *LocalExecutor) {
		if d <= 0 {
			d = DefaultKillGrace
		}
		e.killGrace = d
	}
}

// NewLocalExecutor returns a LocalExecutor configured by opts.
func NewLocalExecutor(opts ...Option) *LocalExecutor {
	e := &LocalExecutor{
		defaultTimeout: DefaultTimeout,
		maxOutputBytes: DefaultMaxOutputBytes,
		killGrace:      DefaultKillGrace,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Run executes req and returns its structured result.
func (e *LocalExecutor) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	return e.run(ctx, req, nil)
}

// RunStreaming executes req, forwarding output to h as it arrives.
//
// Delivery is chunk-oriented, not line-oriented: a chunk is whatever a single
// read from the pipe produced, so it may contain several lines or a partial
// one. Consumers that need lines must reassemble them. Chunks are delivered in
// arrival order across both streams by a single dispatch goroutine, so a slow
// handler can never block the process's pipes or deadlock the run; if the
// handler falls far enough behind that the pending queue exceeds
// maxPendingStreamBytes, live chunks are dropped and a notice is emitted. The
// returned RunResult always contains the complete (capped) output regardless of
// what the handler saw.
func (e *LocalExecutor) RunStreaming(ctx context.Context, req RunRequest, h StreamHandler) (RunResult, error) {
	return e.run(ctx, req, h)
}

func (e *LocalExecutor) run(ctx context.Context, req RunRequest, h StreamHandler) (RunResult, error) {
	if strings.TrimSpace(req.Command) == "" {
		return RunResult{}, ErrEmptyCommand
	}

	workDir, err := resolveWorkingDir(req.WorkingDir)
	if err != nil {
		return RunResult{}, err
	}

	res := RunResult{
		Command:    displayCommand(req),
		WorkingDir: workDir,
		StartedAt:  time.Now(),
	}

	// A context that is already done must not spawn a process at all.
	if ctxErr := ctx.Err(); ctxErr != nil {
		res.ExitCode = -1
		markContextOutcome(&res, ctxErr)
		return res, nil
	}

	cmd := buildCommand(req)
	cmd.Dir = workDir
	cmd.Env = mergeEnv(req.Env)
	if req.Stdin != "" {
		cmd.Stdin = strings.NewReader(req.Stdin)
	}

	// Explicit pipes rather than cmd.StdoutPipe: owning both ends lets the
	// executor force them closed if a surviving grandchild keeps the write end
	// open, so a run can never hang after its process has exited.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return res, fmt.Errorf("execution: stdout pipe: %w", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		return res, fmt.Errorf("execution: stderr pipe: %w", err)
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	var pump *streamPump
	if h != nil {
		pump = newStreamPump(h)
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		if pump != nil {
			pump.close()
		}
		return res, fmt.Errorf("execution: start %s: %w", res.Command, err)
	}
	// The child owns the write ends now; the parent's copies must go, or the
	// readers below would never see EOF.
	stdoutW.Close()
	stderrW.Close()

	stdout := newBoundedCapture(pickOutputLimit(req.MaxOutputBytes, e.maxOutputBytes))
	stderr := newBoundedCapture(pickOutputLimit(req.MaxOutputBytes, e.maxOutputBytes))

	var readers sync.WaitGroup
	readers.Add(2)
	go drain(&readers, stdoutR, stdout, pump, false)
	go drain(&readers, stderrR, stderr, pump, true)
	readersDone := make(chan struct{})
	go func() {
		readers.Wait()
		close(readersDone)
	}()

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var timeoutCh <-chan time.Time
	if timeout := pickTimeout(req.Timeout, e.defaultTimeout); timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		timeoutCh = timer.C
	}

	var waitErr error
	select {
	case waitErr = <-waitCh:
	case <-timeoutCh:
		res.TimedOut = true
		waitErr = e.stop(cmd, waitCh)
	case <-ctx.Done():
		markContextOutcome(&res, ctx.Err())
		waitErr = e.stop(cmd, waitCh)
	}
	res.Duration = time.Since(start)

	e.drainReaders(cmd, readersDone, stdoutR, stderrR)
	stdoutR.Close()
	stderrR.Close()

	if pump != nil {
		pump.close()
	}

	res.Stdout, res.StdoutTruncated = stdout.Result()
	res.Stderr, res.StderrTruncated = stderr.Result()

	if cmd.ProcessState == nil {
		return res, fmt.Errorf("execution: wait %s: %w", res.Command, waitErr)
	}
	res.ExitCode, res.Signal = exitStatus(cmd.ProcessState)
	return res, nil
}

// stop terminates a running command, escalating from a polite signal to a
// forced kill after the grace period, and returns the process's wait error.
func (e *LocalExecutor) stop(cmd *exec.Cmd, waitCh <-chan error) error {
	_ = signalProcessTree(cmd, false)
	grace := time.NewTimer(e.killGrace)
	defer grace.Stop()
	select {
	case err := <-waitCh:
		return err
	case <-grace.C:
	}
	_ = signalProcessTree(cmd, true)
	// SIGKILL is not refusable, so this wait is bounded in practice.
	return <-waitCh
}

// drainReaders waits for the output readers to finish once the process has
// exited.
//
// Output normally ends the moment the last holder of the write end goes away.
// A backgrounded grandchild that inherited the pipe can keep it open forever,
// though, so the wait escalates: kill the process group, then force the read
// ends closed. Losing a few trailing bytes beats hanging the session.
func (e *LocalExecutor) drainReaders(cmd *exec.Cmd, done <-chan struct{}, pipes ...*os.File) {
	first := time.NewTimer(e.killGrace)
	defer first.Stop()
	select {
	case <-done:
		return
	case <-first.C:
	}

	_ = signalProcessTree(cmd, true)

	second := time.NewTimer(e.killGrace)
	defer second.Stop()
	select {
	case <-done:
		return
	case <-second.C:
	}

	for _, p := range pipes {
		p.Close()
	}
	<-done
}

// drain copies a pipe into its capture buffer and, when streaming, forwards
// each chunk to the pump.
func drain(wg *sync.WaitGroup, r io.Reader, sink *boundedCapture, pump *streamPump, isStderr bool) {
	defer wg.Done()
	buf := make([]byte, readChunkSize)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			_, _ = sink.Write(chunk)
			if pump != nil {
				pump.push(isStderr, string(chunk))
			}
		}
		if err != nil {
			return
		}
	}
}

// exitStatus derives the reportable exit code and signal name from a finished
// process.
//
// Go reports -1 for a signalled process. The shell convention of 128+signal is
// substituted instead, because that is what a model has seen in every terminal
// transcript it was trained on and what the same command would report when run
// under a shell.
func exitStatus(ps *os.ProcessState) (int, string) {
	name, num := terminationSignal(ps.Sys())
	code := ps.ExitCode()
	if code < 0 && num > 0 {
		code = 128 + num
	}
	return code, name
}

// markContextOutcome records a context failure on the result. A deadline is
// reported as a timeout rather than a cancellation: to the caller, a context
// deadline and RunRequest.Timeout mean the same thing.
func markContextOutcome(res *RunResult, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		res.TimedOut = true
		return
	}
	res.Cancelled = true
}

// pickTimeout resolves the effective timeout: the request wins, zero falls back
// to the executor default, and a negative value means no timeout.
func pickTimeout(req, def time.Duration) time.Duration {
	if req != 0 {
		return req
	}
	return def
}

// pickOutputLimit resolves the effective per-stream capture cap.
func pickOutputLimit(req, def int) int {
	if req > 0 {
		return req
	}
	if def > 0 {
		return def
	}
	return DefaultMaxOutputBytes
}

// resolveWorkingDir validates the requested working directory, defaulting to
// the Boop process's own directory. It returns an absolute path so the result
// is unambiguous in transcripts.
func resolveWorkingDir(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("execution: resolve working directory: %w", err)
		}
		return wd, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("execution: working directory %q: %w", dir, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("execution: working directory %q: %w", dir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("execution: working directory %q: not a directory", dir)
	}
	return abs, nil
}

// mergeEnv layers the request's variables over the process environment.
//
// The child inherits PATH, HOME, and everything else it needs; Env only
// overrides. Returning nil for an empty override set lets os/exec inherit
// directly. Duplicate names are collapsed because getenv in the child resolves
// the first match, so a naive append of overrides would silently do nothing.
func mergeEnv(overrides map[string]string) []string {
	if len(overrides) == 0 {
		return nil
	}

	base := os.Environ()
	out := make([]string, 0, len(base)+len(overrides))
	index := make(map[string]int, len(base)+len(overrides))

	for _, entry := range base {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			out = append(out, entry)
			continue
		}
		key := envKey(name)
		if i, dup := index[key]; dup {
			out[i] = entry
			continue
		}
		index[key] = len(out)
		out = append(out, entry)
	}

	names := make([]string, 0, len(overrides))
	for name := range overrides {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic ordering keeps results reproducible

	for _, name := range names {
		entry := name + "=" + overrides[name]
		key := envKey(name)
		if i, ok := index[key]; ok {
			out[i] = entry
			continue
		}
		index[key] = len(out)
		out = append(out, entry)
	}
	return out
}

// displayCommand renders the command for the result and for transcripts. It is
// presentation only: the direct-args form is quoted for readability and must
// never be re-parsed as a shell command.
func displayCommand(req RunRequest) string {
	if len(req.Args) == 0 {
		return req.Command
	}
	parts := make([]string, 0, len(req.Args)+1)
	parts = append(parts, quoteArg(req.Command))
	for _, arg := range req.Args {
		parts = append(parts, quoteArg(arg))
	}
	return strings.Join(parts, " ")
}

func quoteArg(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, " \t\n\"'\\") {
		return strconv.Quote(s)
	}
	return s
}

// maxPendingStreamBytes bounds the queue of live chunks awaiting a slow
// StreamHandler. Beyond it, live output is dropped rather than buffered without
// limit; the complete output is still captured in the RunResult.
const maxPendingStreamBytes = 1 << 20

// streamDropNotice is emitted once a backlog has been cleared, so a consumer
// never silently believes it saw everything.
const streamDropNotice = "\n... [boop: %d bytes of live output dropped] ...\n"

type streamChunk struct {
	stderr bool
	text   string
}

// streamPump decouples the pipe readers from the StreamHandler.
//
// The Executor contract says a handler must not block, but the executor must
// stay correct when one does anyway: a handler that blocks inside a pipe reader
// would stop draining the pipe, fill the OS buffer, and wedge the child
// process. All delivery therefore happens on one dedicated goroutine fed by a
// bounded queue, and pushes never block.
type streamPump struct {
	handler StreamHandler

	mu      sync.Mutex
	cond    *sync.Cond
	queue   []streamChunk
	pending int
	dropped [2]int // indexed by stream: 0 stdout, 1 stderr
	closed  bool

	done chan struct{}
}

func newStreamPump(h StreamHandler) *streamPump {
	p := &streamPump{handler: h, done: make(chan struct{})}
	p.cond = sync.NewCond(&p.mu)
	go p.loop()
	return p
}

// push queues a chunk for delivery. It never blocks and never fails.
func (p *streamPump) push(isStderr bool, text string) {
	if text == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	if p.pending+len(text) > maxPendingStreamBytes {
		p.dropped[streamIndex(isStderr)] += len(text)
		p.cond.Signal()
		return
	}
	p.queue = append(p.queue, streamChunk{stderr: isStderr, text: text})
	p.pending += len(text)
	p.cond.Signal()
}

// close flushes queued chunks and waits for the dispatch goroutine to finish,
// so the handler has seen everything before the RunResult is returned.
func (p *streamPump) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		<-p.done
		return
	}
	p.closed = true
	p.cond.Broadcast()
	p.mu.Unlock()
	<-p.done
}

func (p *streamPump) loop() {
	defer close(p.done)
	for {
		p.mu.Lock()
		for len(p.queue) == 0 && p.dropped == [2]int{} && !p.closed {
			p.cond.Wait()
		}
		batch := p.queue
		dropped := p.dropped
		closed := p.closed
		p.queue = nil
		p.pending = 0
		p.dropped = [2]int{}
		p.mu.Unlock()

		for _, c := range batch {
			p.deliver(c.stderr, c.text)
		}
		if dropped[0] > 0 {
			p.deliver(false, fmt.Sprintf(streamDropNotice, dropped[0]))
		}
		if dropped[1] > 0 {
			p.deliver(true, fmt.Sprintf(streamDropNotice, dropped[1]))
		}
		if closed && len(batch) == 0 && dropped == [2]int{} {
			return
		}
	}
}

func (p *streamPump) deliver(isStderr bool, text string) {
	if isStderr {
		p.handler.OnStderr(text)
		return
	}
	p.handler.OnStdout(text)
}

func streamIndex(isStderr bool) int {
	if isStderr {
		return 1
	}
	return 0
}

// Compile-time proof that LocalExecutor satisfies both execution contracts.
var (
	_ Executor          = (*LocalExecutor)(nil)
	_ StreamingExecutor = (*LocalExecutor)(nil)
)
