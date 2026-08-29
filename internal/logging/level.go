package logging

import (
	"fmt"
	"log/slog"
	"strings"
)

// Boop's log levels (§44).
//
// slog ships debug/info/warn/error. Trace is added below debug because the
// spec requires five levels: trace carries protocol-grade detail (raw provider
// payloads, per-chunk stream events, process plumbing) that would drown an
// ordinary debug session, so it has to be separately selectable rather than
// folded into debug.
//
// The 4-point gap to LevelDebug mirrors slog's own spacing between its levels,
// which leaves room for intermediate levels without renumbering these.
const (
	LevelTrace = slog.Level(-8)
	LevelDebug = slog.LevelDebug
	LevelInfo  = slog.LevelInfo
	LevelWarn  = slog.LevelWarn
	LevelError = slog.LevelError
)

// LevelNames lists every accepted level name, least severe first. It is the
// canonical spelling used by config validation, the --log-level flag and the
// error returned by ParseLevel.
var LevelNames = []string{"trace", "debug", "info", "warn", "error"}

// levelsByName maps the accepted spellings to their level.
var levelsByName = map[string]slog.Level{
	"trace": LevelTrace,
	"debug": LevelDebug,
	"info":  LevelInfo,
	"warn":  LevelWarn,
	"error": LevelError,
}

// ParseLevel converts a configured or command-line level name to a slog.Level.
//
// Matching is case-insensitive and surrounding whitespace is ignored, because
// the value arrives from YAML and from `boop --log-level DEBUG` alike. Unknown
// names are an error rather than a silent fallback to info: a user who asked
// for trace and got info would draw wrong conclusions from the resulting logs.
func ParseLevel(s string) (slog.Level, error) {
	name := strings.ToLower(strings.TrimSpace(s))
	if lvl, ok := levelsByName[name]; ok {
		return lvl, nil
	}
	return LevelInfo, fmt.Errorf("logging: %q is not a valid level (want one of %s)",
		s, strings.Join(LevelNames, ", "))
}

// LevelName renders a level the way Boop names it.
//
// It exists because slog.Level.String prints unknown levels as an offset from
// the nearest built-in one, so LevelTrace would appear as "DEBUG-4" in every
// log line and in every grep the user writes.
func LevelName(l slog.Level) string {
	if l == LevelTrace {
		return "TRACE"
	}
	if l < LevelDebug {
		// Still below debug but not exactly trace: keep slog's offset style so
		// a custom level is visibly custom instead of masquerading as trace.
		return fmt.Sprintf("TRACE%+d", int(l-LevelTrace))
	}
	return l.String()
}
