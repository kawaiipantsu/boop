package logging_test

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/logging"
)

func TestParseLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    slog.Level
		wantErr bool
	}{
		{name: "trace", in: "trace", want: logging.LevelTrace},
		{name: "debug", in: "debug", want: logging.LevelDebug},
		{name: "info", in: "info", want: logging.LevelInfo},
		{name: "warn", in: "warn", want: logging.LevelWarn},
		{name: "error", in: "error", want: logging.LevelError},
		{name: "uppercase", in: "TRACE", want: logging.LevelTrace},
		{name: "mixed case", in: "WaRn", want: logging.LevelWarn},
		{name: "surrounding space", in: "  debug\n", want: logging.LevelDebug},
		{name: "empty", in: "", wantErr: true},
		{name: "unknown", in: "verbose", wantErr: true},
		{name: "warning is not an accepted spelling", in: "warning", wantErr: true},
		{name: "numeric", in: "-8", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := logging.ParseLevel(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseLevel(%q) = %v, want error", tc.in, got)
				}
				// The error must tell the user what is valid.
				for _, name := range logging.LevelNames {
					if !strings.Contains(err.Error(), name) {
						t.Errorf("error %q does not mention valid level %q", err, name)
					}
				}
				if got != logging.LevelInfo {
					t.Errorf("ParseLevel(%q) level = %v, want the safe LevelInfo fallback", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseLevel(%q): unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseLevel(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseLevelAcceptsEveryDocumentedName(t *testing.T) {
	t.Parallel()

	for _, name := range logging.LevelNames {
		if _, err := logging.ParseLevel(name); err != nil {
			t.Errorf("LevelNames advertises %q but ParseLevel rejects it: %v", name, err)
		}
	}
}

func TestLevelOrdering(t *testing.T) {
	t.Parallel()

	ordered := []slog.Level{
		logging.LevelTrace,
		logging.LevelDebug,
		logging.LevelInfo,
		logging.LevelWarn,
		logging.LevelError,
	}
	for i := 1; i < len(ordered); i++ {
		if ordered[i-1] >= ordered[i] {
			t.Fatalf("levels not strictly increasing at index %d: %v >= %v", i, ordered[i-1], ordered[i])
		}
	}
}

func TestLevelName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   slog.Level
		want string
	}{
		{name: "trace renders by name, not as DEBUG-4", in: logging.LevelTrace, want: "TRACE"},
		{name: "debug", in: logging.LevelDebug, want: "DEBUG"},
		{name: "info", in: logging.LevelInfo, want: "INFO"},
		{name: "warn", in: logging.LevelWarn, want: "WARN"},
		{name: "error", in: logging.LevelError, want: "ERROR"},
		{name: "custom below trace keeps an offset", in: logging.LevelTrace - 2, want: "TRACE-2"},
		{name: "custom between trace and debug", in: logging.LevelTrace + 1, want: "TRACE+1"},
		{name: "custom above error", in: logging.LevelError + 4, want: "ERROR+4"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := logging.LevelName(tc.in); got != tc.want {
				t.Errorf("LevelName(%d) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseFormat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    logging.Format
		wantErr bool
	}{
		{name: "text", in: "text", want: logging.FormatText},
		{name: "json", in: "json", want: logging.FormatJSON},
		{name: "uppercase json", in: "JSON", want: logging.FormatJSON},
		{name: "padded", in: " text ", want: logging.FormatText},
		{name: "empty", in: "", wantErr: true},
		{name: "unknown", in: "logfmt", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := logging.ParseFormat(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseFormat(%q) = %q, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseFormat(%q): unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseFormat(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
