package logging_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kawaiipantsu/boop/internal/logging"
)

// readIfExists returns a file's contents and whether it exists.
func readIfExists(t *testing.T, path string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data), true
}

func TestFileWriterRotatesBySize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boop.log")
	// Room for exactly one 10-byte record per file.
	w, err := logging.NewFileWriter(path, 10, 3)
	if err != nil {
		t.Fatalf("NewFileWriter: %v", err)
	}
	defer w.Close()

	for i := 0; i < 4; i++ {
		if _, err := fmt.Fprintf(w, "record-%d\n", i); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	// Newest content stays in the active file; .1 is the previous one.
	tests := []struct {
		path string
		want string
	}{
		{path: path, want: "record-3"},
		{path: path + ".1", want: "record-2"},
		{path: path + ".2", want: "record-1"},
		{path: path + ".3", want: "record-0"},
	}
	for _, tc := range tests {
		got, ok := readIfExists(t, tc.path)
		if !ok {
			t.Errorf("%s does not exist", tc.path)
			continue
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s = %q, want it to contain %q", tc.path, got, tc.want)
		}
	}
}

func TestFileWriterPrunesOldFiles(t *testing.T) {
	tests := []struct {
		name        string
		maxBackups  int
		writes      int
		wantBackups int
	}{
		{name: "no history kept", maxBackups: 0, writes: 5, wantBackups: 0},
		{name: "one backup", maxBackups: 1, writes: 5, wantBackups: 1},
		{name: "two backups", maxBackups: 2, writes: 6, wantBackups: 2},
		{name: "window larger than the number of rotations", maxBackups: 5, writes: 3, wantBackups: 2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "boop.log")
			w, err := logging.NewFileWriter(path, 10, tc.maxBackups)
			if err != nil {
				t.Fatalf("NewFileWriter: %v", err)
			}
			defer w.Close()

			for i := 0; i < tc.writes; i++ {
				if _, err := fmt.Fprintf(w, "record-%d\n", i); err != nil {
					t.Fatalf("write %d: %v", i, err)
				}
			}

			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("readdir: %v", err)
			}
			backups := len(entries) - 1 // minus the active file
			if backups != tc.wantBackups {
				names := make([]string, 0, len(entries))
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Errorf("kept %d backups, want %d (files: %v)", backups, tc.wantBackups, names)
			}
			// Whatever is retained, the newest record is always in the
			// active file.
			active, _ := readIfExists(t, path)
			if !strings.Contains(active, fmt.Sprintf("record-%d", tc.writes-1)) {
				t.Errorf("active file %q lost the newest record", active)
			}
		})
	}
}

// TestFileWriterPrunesLeftoversFromAWiderWindow covers a config change: a run
// with maxBackups=5 leaves .1..5 behind, and a later run with maxBackups=2
// must clean them up rather than leak files forever.
func TestFileWriterPrunesLeftoversFromAWiderWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "boop.log")
	for i := 1; i <= 5; i++ {
		if err := os.WriteFile(fmt.Sprintf("%s.%d", path, i), []byte("old\n"), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	w, err := logging.NewFileWriter(path, 10, 2)
	if err != nil {
		t.Fatalf("NewFileWriter: %v", err)
	}
	defer w.Close()
	for i := 0; i < 3; i++ {
		if _, err := fmt.Fprintf(w, "record-%d\n", i); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	for i := 3; i <= 5; i++ {
		if _, ok := readIfExists(t, fmt.Sprintf("%s.%d", path, i)); ok {
			t.Errorf("%s.%d should have been pruned", path, i)
		}
	}
}

func TestFileWriterAppendsToAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boop.log")
	if err := os.WriteFile(path, []byte("previous-run\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	w, err := logging.NewFileWriter(path, logging.DefaultMaxSizeBytes, logging.DefaultMaxBackups)
	if err != nil {
		t.Fatalf("NewFileWriter: %v", err)
	}
	if _, err := w.Write([]byte("this-run\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got, _ := readIfExists(t, path)
	if !strings.Contains(got, "previous-run") || !strings.Contains(got, "this-run") {
		t.Errorf("expected an append, got %q", got)
	}
}

// TestFileWriterOversizedRecord: a record bigger than the whole budget is
// written whole. A truncated JSON line would be worse than an oversized file.
func TestFileWriterOversizedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boop.log")
	w, err := logging.NewFileWriter(path, 16, 2)
	if err != nil {
		t.Fatalf("NewFileWriter: %v", err)
	}
	defer w.Close()

	big := strings.Repeat("x", 100) + "\n"
	n, err := w.Write([]byte(big))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(big) {
		t.Errorf("wrote %d bytes, want %d", n, len(big))
	}
	got, _ := readIfExists(t, path)
	if got != big {
		t.Errorf("oversized record was not written whole (%d bytes on disk)", len(got))
	}
}

func TestFileWriterExplicitRotate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boop.log")
	w, err := logging.NewFileWriter(path, logging.DefaultMaxSizeBytes, 2)
	if err != nil {
		t.Fatalf("NewFileWriter: %v", err)
	}
	defer w.Close()

	if _, err := w.Write([]byte("first\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Rotate(); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if _, err := w.Write([]byte("second\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got, _ := readIfExists(t, path); got != "second\n" {
		t.Errorf("active file = %q, want %q", got, "second\n")
	}
	if got, _ := readIfExists(t, path+".1"); got != "first\n" {
		t.Errorf("%s.1 = %q, want %q", path, got, "first\n")
	}
}

func TestFileWriterValidation(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{name: "empty path", path: "", wantErr: true},
		{name: "valid path", path: filepath.Join(t.TempDir(), "nested", "boop.log")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, err := logging.NewFileWriter(tc.path, 0, -1)
			if tc.wantErr {
				if err == nil {
					_ = w.Close()
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("NewFileWriter: %v", err)
			}
			defer w.Close()
			if !filepath.IsAbs(w.Path()) {
				t.Errorf("Path() = %q, want an absolute path", w.Path())
			}
		})
	}
}

func TestFileWriterWriteAfterClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boop.log")
	w, err := logging.NewFileWriter(path, 0, 1)
	if err != nil {
		t.Fatalf("NewFileWriter: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := w.Write([]byte("late\n")); err == nil {
		t.Error("Write after Close must fail rather than silently drop the record")
	}
	if err := w.Rotate(); err == nil {
		t.Error("Rotate after Close must fail")
	}
	if err := w.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestFileWriterConcurrentRotation is the -race case that matters: agents log
// while the file rotates underneath them.
func TestFileWriterConcurrentRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boop.log")
	w, err := logging.NewFileWriter(path, 64, 3)
	if err != nil {
		t.Fatalf("NewFileWriter: %v", err)
	}
	defer w.Close()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if _, err := fmt.Fprintf(w, "agent=%d n=%d padding\n", i, j); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	if _, ok := readIfExists(t, path); !ok {
		t.Fatal("active log file is missing after concurrent rotation")
	}
}

// TestLoggerRotatesEndToEnd checks the writer is actually wired into New.
func TestLoggerRotatesEndToEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boop.log")
	lg, err := logging.New(logging.Options{
		Level:        logging.LevelTrace,
		Format:       logging.FormatJSON,
		File:         path,
		MaxSizeBytes: 256,
		MaxBackups:   2,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer lg.Close()

	for i := 0; i < 50; i++ {
		lg.Info("rotating", "i", i, "padding", strings.Repeat("p", 40))
	}
	if _, ok := readIfExists(t, path+".1"); !ok {
		t.Error("logger did not rotate its file")
	}
	if _, ok := readIfExists(t, path+".3"); ok {
		t.Error("logger kept more backups than MaxBackups allows")
	}
}
