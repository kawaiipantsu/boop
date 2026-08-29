// Package logging provides Boop's structured logging stack.
//
// It is built on log/slog from the standard library (§40: stdlib first) and
// adds the four things the specification asks for that slog does not give us:
//
//   - A trace level below debug (§44), parsed, filtered and rendered by name.
//   - A destination that is a rotating file in the platform log directory
//     rather than stderr, because a full-screen TUI owns the terminal and
//     §44 forbids debug noise in the transcript.
//   - Size-based rotation with a bounded number of retained files, so a
//     long-running agent runtime cannot fill a disk.
//   - Redaction of credentials (§45) as a slog.Handler middleware, so no code
//     path can log a secret by forgetting to sanitise it first.
//
// # Wiring
//
// The application constructs one logger at startup and installs it as the
// process default:
//
//	lg, err := logging.New(logging.Options{
//		Level:  level,           // from logging.ParseLevel(cfg.Logging.Level)
//		Format: logging.FormatText,
//		File:   path,            // from config.LogFile()
//	})
//	if err != nil { ... }
//	defer lg.Close()
//	slog.SetDefault(lg.Logger)
//	defer logging.CaptureStandardLog(lg.Logger, logging.LevelWarn)()
//
// Calling slog.SetDefault is not optional when a TUI is running: any code that
// reaches for slog.Default without a configured logger writes to stderr, and
// stderr is the terminal Bubble Tea is painting. CaptureStandardLog closes the
// same hole for the standard library's log package.
//
// This package deliberately does not import internal/config. It takes the log
// path as a parameter so that the configuration layer stays free to depend on
// logging later without creating an import cycle.
//
// # Redaction guarantees
//
// See [Redact] for exactly what is caught and, just as importantly, what
// cannot be caught.
package logging
