package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// appDir is the per-application directory name appended to every OS base
// directory. It is deliberately lowercase on all platforms so that a config
// directory copied between machines keeps working on case-sensitive systems.
const appDir = "boop"

// EnvConfigDir names the environment variable that relocates every Boop
// directory under a single root.
//
// It exists for two reasons: tests need a disposable tree that never touches
// the developer's real configuration, and portable installs (a USB stick, a
// checked-out dotfiles repo, a container image) need everything in one place.
// When it is set, the OS-native layout below is bypassed entirely.
const EnvConfigDir = "BOOP_CONFIG_DIR"

// File and subdirectory names inside the resolved directories. §18 fixes this
// layout: config.yaml, boop.db, sessions/, logs/, cache/.
const (
	configFileName = "config.yaml"
	databaseName   = "boop.db"
	sessionsName   = "sessions"
	logsName       = "logs"
	cacheName      = "cache"
	logFileName    = "boop.log"
)

// resolver resolves base directories for one host environment.
//
// The operating system, environment lookup and home directory are injected
// rather than read directly so the platform rules can be unit-tested from any
// host; hostResolver binds them to the real process.
type resolver struct {
	goos   string
	getenv func(string) string
	home   func() (string, error)
}

// hostResolver returns a resolver bound to the running process.
func hostResolver() resolver {
	return resolver{goos: runtime.GOOS, getenv: os.Getenv, home: os.UserHomeDir}
}

// override returns the absolute BOOP_CONFIG_DIR root, if one is set.
func (r resolver) override() (string, bool, error) {
	raw := strings.TrimSpace(r.getenv(EnvConfigDir))
	if raw == "" {
		return "", false, nil
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", true, fmt.Errorf("%s=%q: %w", EnvConfigDir, raw, err)
	}
	return abs, true, nil
}

// underHome joins the app directory onto a path relative to the user's home.
func (r resolver) underHome(parts ...string) (string, error) {
	home, err := r.home()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	if home == "" {
		return "", fmt.Errorf("locate home directory: empty path")
	}
	return filepath.Join(append([]string{home}, parts...)...), nil
}

// xdg resolves an XDG base directory variable, falling back to its
// specification-mandated default beneath the home directory.
//
// Per the XDG Base Directory Specification a relative value must be ignored,
// so only absolute paths are honoured.
func (r resolver) xdg(name string, fallback ...string) (string, error) {
	if v := strings.TrimSpace(r.getenv(name)); filepath.IsAbs(v) {
		return filepath.Join(v, appDir), nil
	}
	return r.underHome(append(fallback, appDir)...)
}

// windowsBase resolves a Windows application-data variable, falling back to
// the conventional location beneath the home directory when the variable is
// unset (which happens in stripped service environments).
func (r resolver) windowsBase(name string, fallback ...string) (string, error) {
	if v := strings.TrimSpace(r.getenv(name)); v != "" {
		return filepath.Join(v, appDir), nil
	}
	return r.underHome(append(fallback, appDir)...)
}

// configDir resolves the directory holding config.yaml.
func (r resolver) configDir() (string, error) {
	if root, ok, err := r.override(); ok || err != nil {
		return root, err
	}
	switch r.goos {
	case "darwin":
		return r.underHome("Library", "Application Support", appDir)
	case "windows":
		return r.windowsBase("AppData", "AppData", "Roaming")
	default:
		return r.xdg("XDG_CONFIG_HOME", ".config")
	}
}

// dataDir resolves the directory holding durable state: the database and
// stored sessions.
func (r resolver) dataDir() (string, error) {
	if root, ok, err := r.override(); ok || err != nil {
		return root, err
	}
	switch r.goos {
	case "darwin":
		// macOS has no separate data location; Application Support is it.
		return r.underHome("Library", "Application Support", appDir)
	case "windows":
		return r.windowsBase("AppData", "AppData", "Roaming")
	default:
		return r.xdg("XDG_DATA_HOME", ".local", "share")
	}
}

// cacheDir resolves the directory for regenerable data.
func (r resolver) cacheDir() (string, error) {
	if root, ok, err := r.override(); ok || err != nil {
		if err != nil {
			return "", err
		}
		return filepath.Join(root, cacheName), nil
	}
	switch r.goos {
	case "darwin":
		return r.underHome("Library", "Caches", appDir)
	case "windows":
		// Caches are machine-local and must not roam.
		return r.windowsBase("LocalAppData", "AppData", "Local")
	default:
		return r.xdg("XDG_CACHE_HOME", ".cache")
	}
}

// logDir resolves the directory for log files.
//
// Logs are state, not configuration and not cache, so on Linux they follow
// XDG_STATE_HOME; on macOS they use the native ~/Library/Logs; on Windows they
// stay machine-local alongside the cache.
func (r resolver) logDir() (string, error) {
	if root, ok, err := r.override(); ok || err != nil {
		if err != nil {
			return "", err
		}
		return filepath.Join(root, logsName), nil
	}
	switch r.goos {
	case "darwin":
		return r.underHome("Library", "Logs", appDir)
	case "windows":
		base, err := r.windowsBase("LocalAppData", "AppData", "Local")
		if err != nil {
			return "", err
		}
		return filepath.Join(base, logsName), nil
	default:
		state, err := r.xdg("XDG_STATE_HOME", ".local", "state")
		if err != nil {
			return "", err
		}
		return filepath.Join(state, logsName), nil
	}
}

// ConfigDir returns the directory containing config.yaml.
//
// Linux uses $XDG_CONFIG_HOME/boop (default ~/.config/boop), macOS uses
// ~/Library/Application Support/boop, and Windows uses %AppData%\boop.
// Setting BOOP_CONFIG_DIR overrides all of them.
func ConfigDir() (string, error) { return hostResolver().configDir() }

// DataDir returns the directory holding durable state such as boop.db and
// stored sessions.
//
// Linux uses $XDG_DATA_HOME/boop (default ~/.local/share/boop), macOS uses
// ~/Library/Application Support/boop, and Windows uses the roaming
// %AppData%\boop.
func DataDir() (string, error) { return hostResolver().dataDir() }

// CacheDir returns the directory for regenerable data, which a user may delete
// at any time without losing work.
//
// Linux uses $XDG_CACHE_HOME/boop (default ~/.cache/boop), macOS uses
// ~/Library/Caches/boop, and Windows uses %LocalAppData%\boop.
func CacheDir() (string, error) { return hostResolver().cacheDir() }

// LogDir returns the directory for log files.
//
// Linux uses $XDG_STATE_HOME/boop/logs (default ~/.local/state/boop/logs),
// macOS uses ~/Library/Logs/boop, and Windows uses %LocalAppData%\boop\logs.
func LogDir() (string, error) { return hostResolver().logDir() }

// ConfigFile returns the full path to config.yaml.
func ConfigFile() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, configFileName), nil
}

// DatabasePath returns the full path to the SQLite database.
func DatabasePath() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, databaseName), nil
}

// SessionsDir returns the directory holding persisted session files.
func SessionsDir() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sessionsName), nil
}

// LogFile returns the full path to the default log file.
func LogFile() (string, error) {
	dir, err := LogDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, logFileName), nil
}

// EnsureDirs creates every directory Boop writes to.
//
// They are created 0700: session transcripts, the database and the logs can
// all contain the contents of private source trees, so they are not readable
// by other users on shared machines.
func EnsureDirs() error {
	dirs := make([]string, 0, 5)
	for _, fn := range []func() (string, error){ConfigDir, DataDir, CacheDir, LogDir, SessionsDir} {
		dir, err := fn()
		if err != nil {
			return err
		}
		dirs = append(dirs, dir)
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}
