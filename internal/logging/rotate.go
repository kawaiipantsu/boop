package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Rotation defaults. A long-running agent runtime writes logs unattended for
// days, so the total on-disk footprint has to be bounded by construction:
// DefaultMaxSizeBytes * (DefaultMaxBackups + 1) is the ceiling.
const (
	// DefaultMaxSizeBytes is the size at which the active log file rotates.
	DefaultMaxSizeBytes int64 = 10 << 20 // 10 MiB
	// DefaultMaxBackups is how many rotated files are kept besides the active
	// one. Rotated files are named <path>.1 (newest) .. <path>.N (oldest).
	DefaultMaxBackups = 5

	// logDirPerm and logFilePerm keep logs private. Boop logs can contain file
	// paths, command lines and prompt fragments from private source trees, so
	// they are not world-readable on shared machines. This matches the 0700
	// that config.EnsureDirs uses for the same reason.
	logDirPerm  os.FileMode = 0o700
	logFilePerm os.FileMode = 0o600
)

// FileWriter is an io.WriteCloser that appends to a log file and rotates it by
// size, keeping a bounded number of previous files.
//
// It is the destination Boop uses whenever a terminal UI is running: §44
// forbids debug noise in the TUI transcript, and a writer that never touches
// the terminal is the only way to guarantee that a full-screen Bubble Tea
// program keeps sole ownership of stdout/stderr. Every write is serialised by
// a mutex, so it is also safe to share one FileWriter between the main loop
// and any number of agent goroutines.
type FileWriter struct {
	path       string
	maxSize    int64
	maxBackups int

	mu   sync.Mutex
	file *os.File
	size int64
}

// NewFileWriter opens (creating as needed) the log file at path.
//
// The parent directory is created too, so callers can pass config.LogFile()
// on a fresh machine without a separate mkdir step. A non-positive maxSize or
// a negative maxBackups falls back to the package defaults; maxBackups of 0 is
// honoured and means "rotate by truncation, keep no history".
func NewFileWriter(path string, maxSize int64, maxBackups int) (*FileWriter, error) {
	if path == "" {
		return nil, fmt.Errorf("logging: empty log file path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("logging: resolve %q: %w", path, err)
	}
	if maxSize <= 0 {
		maxSize = DefaultMaxSizeBytes
	}
	if maxBackups < 0 {
		maxBackups = DefaultMaxBackups
	}
	w := &FileWriter{path: abs, maxSize: maxSize, maxBackups: maxBackups}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

// Path reports the absolute path of the active log file, which the app shows
// in `boop status` output so a user can find their logs.
func (w *FileWriter) Path() string { return w.path }

// open creates the directory and opens the file for appending.
func (w *FileWriter) open() error {
	dir := filepath.Dir(w.path)
	if err := os.MkdirAll(dir, logDirPerm); err != nil {
		return fmt.Errorf("logging: create log directory %s: %w", dir, err)
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, logFilePerm)
	if err != nil {
		return fmt.Errorf("logging: open log file %s: %w", w.path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("logging: stat log file %s: %w", w.path, err)
	}
	w.file = f
	w.size = info.Size()
	return nil
}

// Write appends p, rotating first if it would push the file past its limit.
//
// A single record larger than maxSize is written whole rather than split or
// dropped: a truncated JSON line is worse than an oversized file, and the next
// write rotates anyway.
func (w *FileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, fmt.Errorf("logging: write to closed log file %s", w.path)
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxSize {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// Rotate closes the active file, shifts the history and starts a new file. It
// is exported so a future `boop logs rotate` or a SIGHUP handler can force it.
func (w *FileWriter) Rotate() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return fmt.Errorf("logging: rotate closed log file %s", w.path)
	}
	return w.rotateLocked()
}

// rotateLocked performs the rename cascade with w.mu held.
//
// Renaming is used rather than copy-and-truncate so that rotation is atomic
// per file and cannot lose records that arrive mid-rotation.
func (w *FileWriter) rotateLocked() error {
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("logging: close log file %s: %w", w.path, err)
	}
	w.file = nil

	if w.maxBackups == 0 {
		// No history wanted: drop the current file and start clean.
		if err := os.Remove(w.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("logging: remove log file %s: %w", w.path, err)
		}
		return w.open()
	}

	// Prune anything beyond the retention window, including files left behind
	// by an earlier run configured with a larger maxBackups.
	for i := w.maxBackups; ; i++ {
		name := w.backupPath(i)
		if _, err := os.Stat(name); err != nil {
			break
		}
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("logging: prune %s: %w", name, err)
		}
	}

	// Shift .N-1 -> .N, oldest first, then the active file to .1.
	for i := w.maxBackups - 1; i >= 1; i-- {
		src := w.backupPath(i)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := os.Rename(src, w.backupPath(i+1)); err != nil {
			return fmt.Errorf("logging: rotate %s: %w", src, err)
		}
	}
	if err := os.Rename(w.path, w.backupPath(1)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("logging: rotate %s: %w", w.path, err)
	}
	return w.open()
}

// backupPath names the i-th rotated file (1 is the most recent).
func (w *FileWriter) backupPath(i int) string {
	return fmt.Sprintf("%s.%d", w.path, i)
}

// Close flushes and closes the active file. It is safe to call twice, which
// matters because shutdown (§58) may run from more than one path.
func (w *FileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	if err != nil {
		return fmt.Errorf("logging: close log file %s: %w", w.path, err)
	}
	return nil
}
