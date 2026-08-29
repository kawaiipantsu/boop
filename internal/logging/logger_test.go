package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	stdlog "log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/logging"
)

// emitAll logs one record at each of Boop's five levels.
func emitAll(lg *logging.Logger) {
	lg.Trace("trace-line")
	lg.Debug("debug-line")
	lg.Info("info-line")
	lg.Warn("warn-line")
	lg.Error("error-line")
}

func TestLevelFiltering(t *testing.T) {
	tests := []struct {
		name      string
		level     slog.Level
		wantLines []string
	}{
		{
			name:      "trace shows everything",
			level:     logging.LevelTrace,
			wantLines: []string{"trace-line", "debug-line", "info-line", "warn-line", "error-line"},
		},
		{
			name:      "debug suppresses trace",
			level:     logging.LevelDebug,
			wantLines: []string{"debug-line", "info-line", "warn-line", "error-line"},
		},
		{
			name:      "info",
			level:     logging.LevelInfo,
			wantLines: []string{"info-line", "warn-line", "error-line"},
		},
		{
			name:      "warn",
			level:     logging.LevelWarn,
			wantLines: []string{"warn-line", "error-line"},
		},
		{
			name:      "error",
			level:     logging.LevelError,
			wantLines: []string{"error-line"},
		},
	}

	all := []string{"trace-line", "debug-line", "info-line", "warn-line", "error-line"}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			lg, err := logging.New(logging.Options{Level: tc.level, Writer: &buf})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			emitAll(lg)

			out := buf.String()
			want := make(map[string]bool, len(tc.wantLines))
			for _, line := range tc.wantLines {
				want[line] = true
			}
			for _, line := range all {
				got := strings.Contains(out, line)
				if got != want[line] {
					t.Errorf("at level %s: %q present = %v, want %v\noutput:\n%s",
						logging.LevelName(tc.level), line, got, want[line], out)
				}
			}
		})
	}
}

// TestTraceLevelIsNamed is the reason LevelTrace needs a ReplaceAttr hook:
// without it slog would print "DEBUG-4".
func TestTraceLevelIsNamed(t *testing.T) {
	tests := []struct {
		name   string
		format logging.Format
		want   string
	}{
		{name: "text", format: logging.FormatText, want: "level=TRACE"},
		{name: "json", format: logging.FormatJSON, want: `"level":"TRACE"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			lg, err := logging.New(logging.Options{
				Level:  logging.LevelTrace,
				Format: tc.format,
				Writer: &buf,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			lg.Trace("streaming chunk", "seq", 3)

			out := buf.String()
			if !strings.Contains(out, tc.want) {
				t.Errorf("output %q does not contain %q", out, tc.want)
			}
			if strings.Contains(out, "DEBUG-4") {
				t.Errorf("trace level rendered as an offset: %q", out)
			}
		})
	}
}

func TestTextOutputShape(t *testing.T) {
	var buf bytes.Buffer
	lg, err := logging.New(logging.Options{Format: logging.FormatText, Writer: &buf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lg.Info("model loaded", "model", "llama3.1:8b", "duration_ms", 312)

	out := strings.TrimSpace(buf.String())
	for _, want := range []string{"level=INFO", `msg="model loaded"`, "model=llama3.1:8b", "duration_ms=312", "time="} {
		if !strings.Contains(out, want) {
			t.Errorf("text output %q missing %q", out, want)
		}
	}
	if strings.Count(out, "\n") != 0 {
		t.Errorf("expected exactly one line, got %q", out)
	}
}

func TestJSONOutputShape(t *testing.T) {
	var buf bytes.Buffer
	lg, err := logging.New(logging.Options{Format: logging.FormatJSON, Writer: &buf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lg.Info("model loaded",
		logging.KeySessionID, "s-1",
		slog.Group("usage", slog.Int("total_tokens", 1280)),
	)

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("output is not JSON (%v): %s", err, buf.String())
	}
	if rec["msg"] != "model loaded" {
		t.Errorf("msg = %v, want %q", rec["msg"], "model loaded")
	}
	if rec["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", rec["level"])
	}
	if rec[logging.KeySessionID] != "s-1" {
		t.Errorf("%s = %v, want s-1", logging.KeySessionID, rec[logging.KeySessionID])
	}
	if _, ok := rec["time"]; !ok {
		t.Errorf("record has no time field: %v", rec)
	}
	usage, ok := rec["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage group missing or not an object: %v", rec["usage"])
	}
	if usage["total_tokens"] != float64(1280) {
		t.Errorf("usage.total_tokens = %v, want 1280", usage["total_tokens"])
	}
	if _, ok := rec["source"]; ok {
		t.Errorf("source must be absent unless AddSource is set: %v", rec)
	}
}

func TestAddSource(t *testing.T) {
	var buf bytes.Buffer
	lg, err := logging.New(logging.Options{Format: logging.FormatJSON, Writer: &buf, AddSource: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lg.Info("here")

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	source, ok := rec["source"].(map[string]any)
	if !ok {
		t.Fatalf("source missing: %v", rec)
	}
	file, _ := source["file"].(string)
	if filepath.Base(file) != "logger_test.go" {
		t.Errorf("source.file = %q, want this test file", file)
	}
}

func TestNewRequiresADestination(t *testing.T) {
	if _, err := logging.New(logging.Options{}); err == nil {
		t.Fatal("New with neither File nor Writer must fail rather than pick a destination")
	}
}

func TestNewRejectsUnknownFormat(t *testing.T) {
	var buf bytes.Buffer
	if _, err := logging.New(logging.Options{Format: "logfmt", Writer: &buf}); err == nil {
		t.Fatal("New with an unknown format must fail")
	}
}

func TestNewCreatesLogDirectoryAndFile(t *testing.T) {
	// A fresh machine has no log directory yet, so New must build the path.
	dir := filepath.Join(t.TempDir(), "state", "boop", "logs")
	path := filepath.Join(dir, "boop.log")

	lg, err := logging.New(logging.Options{Level: logging.LevelTrace, File: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = lg.Close() })

	if lg.Path() != path {
		t.Errorf("Path() = %q, want %q", lg.Path(), path)
	}
	lg.Info("hello file")
	if err := lg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(data), "hello file") {
		t.Errorf("log file does not contain the record: %q", data)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Logs can contain prompt fragments and paths from private trees.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("log file mode = %v, want 0600", perm)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("log dir mode = %v, want 0700", perm)
	}
}

// TestFileDestinationLeavesTerminalAlone is the §44 guarantee that a TUI is
// not polluted: nothing reaches the process's standard streams.
func TestFileDestinationLeavesTerminalAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boop.log")
	lg, err := logging.New(logging.Options{Level: logging.LevelTrace, File: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer lg.Close()

	stdout, stderr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout, os.Stderr = wOut, wErr
	emitAll(lg)
	_ = wOut.Close()
	_ = wErr.Close()
	os.Stdout, os.Stderr = stdout, stderr

	for name, r := range map[string]*os.File{"stdout": rOut, "stderr": rErr} {
		var got bytes.Buffer
		if _, err := got.ReadFrom(r); err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if got.Len() != 0 {
			t.Errorf("file-backed logger wrote to %s: %q", name, got.String())
		}
	}
}

func TestSetLevelAtRuntime(t *testing.T) {
	var buf bytes.Buffer
	lg, err := logging.New(logging.Options{Level: logging.LevelInfo, Writer: &buf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if lg.Level() != logging.LevelInfo {
		t.Fatalf("Level() = %v, want info", lg.Level())
	}

	derived := lg.With("component", "router")
	lg.Trace("before")
	derived.Debug("before-derived")
	if buf.Len() != 0 {
		t.Fatalf("expected nothing below info, got %q", buf.String())
	}

	lg.SetLevel(logging.LevelTrace)
	if lg.Level() != logging.LevelTrace {
		t.Errorf("Level() = %v, want trace", lg.Level())
	}
	lg.Trace("after")
	derived.Debug("after-derived")

	out := buf.String()
	if !strings.Contains(out, "after") {
		t.Errorf("raising the level did not take effect: %q", out)
	}
	// A derived logger shares the level variable, which is the point.
	if !strings.Contains(out, "after-derived") {
		t.Errorf("derived logger did not follow the level change: %q", out)
	}
}

func TestPackageTraceHelper(t *testing.T) {
	var buf bytes.Buffer
	lg, err := logging.New(logging.Options{Level: logging.LevelTrace, Writer: &buf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	logging.Trace(context.Background(), lg.Logger, "raw chunk", "bytes", 42)
	if !strings.Contains(buf.String(), "raw chunk") {
		t.Errorf("Trace helper emitted nothing: %q", buf.String())
	}

	// A nil logger must not panic: tracing is never worth a crash.
	logging.Trace(context.Background(), nil, "ignored")
}

func TestNewNopDiscards(t *testing.T) {
	lg := logging.NewNop()
	emitAll(lg)
	if lg.Path() != "" {
		t.Errorf("Path() = %q, want empty for a no-op logger", lg.Path())
	}
	if err := lg.Close(); err != nil {
		t.Errorf("Close on a no-op logger: %v", err)
	}
	if logging.Discard().Enabled(context.Background(), logging.LevelError) {
		t.Error("Discard() should report Enabled=false so callers skip work")
	}
}

func TestCloseIsIdempotentAndSkipsCallerWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boop.log")
	lg, err := logging.New(logging.Options{File: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("second Close must be a no-op, got: %v", err)
	}

	var buf bytes.Buffer
	owned, err := logging.New(logging.Options{Writer: &buf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := owned.Close(); err != nil {
		t.Fatalf("Close with a caller-supplied writer: %v", err)
	}
	owned.Info("still writable")
	if !strings.Contains(buf.String(), "still writable") {
		t.Error("Close must not close a caller-owned writer")
	}
}

func TestConcurrentWritesToOneLogger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boop.log")
	lg, err := logging.New(logging.Options{Level: logging.LevelTrace, File: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer lg.Close()

	const goroutines, perGoroutine = 8, 25
	done := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < perGoroutine; j++ {
				lg.Info("concurrent", "agent", i, "n", j, "api_key", "anything-at-all")
			}
		}(i)
	}
	for i := 0; i < goroutines; i++ {
		<-done
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != goroutines*perGoroutine {
		t.Errorf("got %d lines, want %d (interleaved writes)", len(lines), goroutines*perGoroutine)
	}
	if strings.Contains(string(data), "anything-at-all") {
		t.Error("redaction is not concurrency safe")
	}
}

// TestTraceSourceAttribution: AddSource is only worth its cost if it names the
// caller rather than this package's Trace wrapper.
func TestTraceSourceAttribution(t *testing.T) {
	tests := []struct {
		name string
		emit func(*logging.Logger)
	}{
		{name: "method", emit: func(lg *logging.Logger) { lg.Trace("m") }},
		{
			name: "method with context",
			emit: func(lg *logging.Logger) { lg.TraceContext(context.Background(), "mc") },
		},
		{
			name: "package helper",
			emit: func(lg *logging.Logger) { logging.Trace(context.Background(), lg.Logger, "p") },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			lg, err := logging.New(logging.Options{
				Level:     logging.LevelTrace,
				Format:    logging.FormatJSON,
				Writer:    &buf,
				AddSource: true,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			tc.emit(lg)

			var rec map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
				t.Fatalf("output is not JSON: %v", err)
			}
			source, ok := rec["source"].(map[string]any)
			if !ok {
				t.Fatalf("source missing: %v", rec)
			}
			file, _ := source["file"].(string)
			if filepath.Base(file) != "logger_test.go" {
				t.Errorf("source.file = %q, want the caller's file", file)
			}
		})
	}
}

// TestCaptureStandardLog: dependencies that use the standard log package must
// not paint over a running TUI.
func TestCaptureStandardLog(t *testing.T) {
	var buf bytes.Buffer
	lg, err := logging.New(logging.Options{Level: logging.LevelTrace, Format: logging.FormatJSON, Writer: &buf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Keep the process-global standard logger intact for other tests.
	prevWriter, prevFlags, prevPrefix := stdlog.Writer(), stdlog.Flags(), stdlog.Prefix()
	t.Cleanup(func() {
		stdlog.SetOutput(prevWriter)
		stdlog.SetFlags(prevFlags)
		stdlog.SetPrefix(prevPrefix)
	})

	var stderr bytes.Buffer
	stdlog.SetOutput(&stderr)
	restore := logging.CaptureStandardLog(lg.Logger, logging.LevelWarn)

	stdlog.Printf("http: TLS handshake error with key %s", fakeOpenAIKey)
	restore()
	stdlog.Print("after restore")

	out := buf.String()
	if !strings.Contains(out, "TLS handshake error") {
		t.Errorf("standard log output was not captured: %q", out)
	}
	if !strings.Contains(out, `"level":"WARN"`) {
		t.Errorf("captured record used the wrong level: %q", out)
	}
	if strings.Contains(out, fakeOpenAIKey) {
		t.Errorf("captured record was not redacted: %q", out)
	}
	if strings.Contains(out, "after restore") {
		t.Error("restore did not detach the standard logger")
	}
	if !strings.Contains(stderr.String(), "after restore") {
		t.Errorf("restore did not put the previous writer back: %q", stderr.String())
	}

	// A nil logger must be tolerated and its restore must be safe to call.
	logging.CaptureStandardLog(nil, logging.LevelWarn)()
}
