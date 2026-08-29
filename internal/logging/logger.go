package logging

import (
	"bytes"
	"context"
	"fmt"
	"io"
	stdlog "log"
	"log/slog"
	"runtime"
	"strings"
	"time"
)

// Format selects the on-disk representation of a log record.
type Format string

// The supported formats. Text is for humans reading a log file or tailing it
// during development; JSON is for machines — log shippers, the future crash
// reporter, and tests that assert on fields rather than on formatting.
const (
	FormatText Format = "text"
	FormatJSON Format = "json"
)

// Formats lists the accepted format names for error messages and validation.
var Formats = []string{string(FormatText), string(FormatJSON)}

// ParseFormat converts a configured or command-line format name.
func ParseFormat(s string) (Format, error) {
	switch Format(strings.ToLower(strings.TrimSpace(s))) {
	case FormatText:
		return FormatText, nil
	case FormatJSON:
		return FormatJSON, nil
	}
	return FormatText, fmt.Errorf("logging: %q is not a valid format (want one of %s)",
		s, strings.Join(Formats, ", "))
}

// Options configures New.
//
// The zero value is intentionally the safe configuration except for the
// destination: level info, text format, redaction on. There is no default
// destination because writing logs to a path the caller did not choose is how
// debug output ends up on top of a full-screen TUI.
type Options struct {
	// Level is the minimum level emitted. The zero value is LevelInfo.
	Level slog.Level

	// Format selects text or JSON. The zero value is FormatText.
	Format Format

	// File is the log file path, typically config.LogFile(). Its directory is
	// created if missing and the file is rotated by size. Ignored when Writer
	// is set.
	//
	// This package takes a path instead of calling config.LogFile itself so
	// that internal/config remains free to depend on logging without an
	// import cycle.
	File string

	// Writer overrides File with a caller-owned destination — an in-memory
	// buffer in tests, or os.Stderr for `boop --log-level debug` in plain CLI
	// mode where no TUI owns the terminal. The caller closes it; Logger.Close
	// will not.
	Writer io.Writer

	// MaxSizeBytes is the rotation threshold; <= 0 means DefaultMaxSizeBytes.
	MaxSizeBytes int64

	// MaxBackups is how many rotated files to keep; negative means
	// DefaultMaxBackups, and 0 means keep none.
	MaxBackups int

	// AddSource records file:line on every record. It costs a runtime.Callers
	// frame lookup per record, so it is off by default and normally enabled
	// only together with debug or trace.
	AddSource bool

	// DisableRedaction removes the §45 redaction middleware. It exists so the
	// redactor itself can be tested and benchmarked against raw output; it is
	// phrased negatively so that the zero value is the safe one.
	DisableRedaction bool
}

// Logger is a configured slog.Logger together with ownership of its
// destination and a live level control.
//
// The embedded *slog.Logger is the value the rest of Boop should be handed:
// nothing outside startup needs Close or SetLevel, and depending on the plain
// stdlib type keeps this package out of every call site's imports.
type Logger struct {
	*slog.Logger

	level  *slog.LevelVar
	closer io.Closer
	path   string
}

// New builds a logger from opts.
//
// The handler stack is, outermost first: redaction, then the text or JSON
// handler, then the destination. Redaction has to be outermost so that
// attributes bound through Logger.With and groups opened with WithGroup pass
// through it too.
func New(opts Options) (*Logger, error) {
	w := opts.Writer
	var closer io.Closer
	var path string

	if w == nil {
		if opts.File == "" {
			return nil, fmt.Errorf("logging: Options needs File or Writer")
		}
		fw, err := NewFileWriter(opts.File, opts.MaxSizeBytes, opts.MaxBackups)
		if err != nil {
			return nil, err
		}
		w, closer, path = fw, fw, fw.Path()
	}

	level := new(slog.LevelVar)
	level.Set(opts.Level)

	handlerOpts := &slog.HandlerOptions{
		Level:       level,
		AddSource:   opts.AddSource,
		ReplaceAttr: replaceAttr,
	}

	var h slog.Handler
	switch opts.Format {
	case FormatJSON:
		h = slog.NewJSONHandler(w, handlerOpts)
	case FormatText, "":
		h = slog.NewTextHandler(w, handlerOpts)
	default:
		if closer != nil {
			_ = closer.Close()
		}
		return nil, fmt.Errorf("logging: %q is not a valid format (want one of %s)",
			opts.Format, strings.Join(Formats, ", "))
	}
	if !opts.DisableRedaction {
		h = Redact(h)
	}

	return &Logger{Logger: slog.New(h), level: level, closer: closer, path: path}, nil
}

// NewNop returns a logger that discards everything, for tests and for code
// paths that run before configuration has been read.
func NewNop() *Logger {
	level := new(slog.LevelVar)
	level.Set(LevelError)
	return &Logger{Logger: Discard(), level: level}
}

// Discard returns a plain *slog.Logger that drops every record. Unlike a
// handler writing to io.Discard it also reports Enabled=false, so callers skip
// the cost of building attributes.
func Discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// Path reports the active log file, or "" when the caller supplied a Writer.
// §54 status output uses it to tell the user where the logs are.
func (l *Logger) Path() string { return l.path }

// Level reports the current minimum level.
func (l *Logger) Level() slog.Level { return l.level.Level() }

// SetLevel changes the minimum level of this logger and every logger derived
// from it, at runtime and without reopening the file. That is what lets the
// `/config` editor (§55) raise verbosity mid-session.
func (l *Logger) SetLevel(level slog.Level) { l.level.Set(level) }

// Trace logs at LevelTrace. slog has no Trace method, and callers should not
// have to remember the numeric level.
func (l *Logger) Trace(msg string, args ...any) {
	logAt(context.Background(), l.Logger, LevelTrace, msg, args...)
}

// TraceContext logs at LevelTrace, carrying ctx to the handler.
func (l *Logger) TraceContext(ctx context.Context, msg string, args ...any) {
	logAt(ctx, l.Logger, LevelTrace, msg, args...)
}

// Close releases the log file. It is a no-op when the destination was a
// caller-supplied Writer, which the caller owns. Safe to call more than once.
func (l *Logger) Close() error {
	if l.closer == nil {
		return nil
	}
	return l.closer.Close()
}

// Trace logs at LevelTrace on a plain *slog.Logger, which is what most of Boop
// holds (typically one obtained from FromContext). A nil logger is ignored so
// that tracing cannot panic a code path that was never given a logger.
func Trace(ctx context.Context, l *slog.Logger, msg string, args ...any) {
	if l == nil {
		return
	}
	logAt(ctx, l, LevelTrace, msg, args...)
}

// logAt emits one record with the caller's own source location.
//
// slog.Logger.Log would attribute the record to this file, because it captures
// the program counter of its immediate caller. Building the record here and
// skipping the two frames this package adds keeps AddSource pointing at the
// code that actually logged, which is the only reason to enable it.
func logAt(ctx context.Context, l *slog.Logger, level slog.Level, msg string, args ...any) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !l.Enabled(ctx, level) {
		return
	}
	// Skip runtime.Callers, logAt and the exported wrapper.
	var pcs [1]uintptr
	runtime.Callers(3, pcs[:])
	r := slog.NewRecord(time.Now(), level, msg, pcs[0])
	r.Add(args...)
	_ = l.Handler().Handle(ctx, r)
}

// replaceAttr renders the level by Boop's names.
//
// Without it slog prints LevelTrace as "DEBUG-4", which is unreadable and
// ungreppable; the four built-in levels are left exactly as slog spells them.
func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.LevelKey {
		if lvl, ok := a.Value.Any().(slog.Level); ok {
			return slog.String(slog.LevelKey, LevelName(lvl))
		}
	}
	return a
}

// slogWriter adapts an io.Writer to a slog.Logger, one record per Write.
type slogWriter struct {
	logger *slog.Logger
	level  slog.Level
}

// Write emits p as a single record with its trailing newline removed.
func (w *slogWriter) Write(p []byte) (int, error) {
	msg := string(bytes.TrimRight(p, "\n"))
	if msg != "" {
		w.logger.Log(context.Background(), w.level, msg)
	}
	return len(p), nil
}

// CaptureStandardLog redirects the standard library's log package into l and
// returns a function that restores the previous configuration.
//
// This matters for §44 more than it looks: net/http writes server errors to
// log.Default by default, and any dependency may do the same. Those writes go
// to stderr, and stderr is the terminal that Bubble Tea is painting — a single
// stray line corrupts the whole frame. Routing them into the log file keeps
// the transcript clean without losing the message.
//
// It mutates process-global state, so the application should call it once at
// startup, after slog.SetDefault.
func CaptureStandardLog(l *slog.Logger, level slog.Level) (restore func()) {
	if l == nil {
		return func() {}
	}
	prevFlags := stdlog.Flags()
	prevPrefix := stdlog.Prefix()
	prevWriter := stdlog.Writer()

	// The record already carries a timestamp and the source of truth for
	// formatting is the handler, so strip the standard logger's own decoration.
	stdlog.SetFlags(0)
	stdlog.SetPrefix("")
	stdlog.SetOutput(&slogWriter{logger: l, level: level})

	return func() {
		stdlog.SetFlags(prevFlags)
		stdlog.SetPrefix(prevPrefix)
		stdlog.SetOutput(prevWriter)
	}
}
