package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fakeEnv builds a getenv function backed by a map, so platform rules can be
// tested from any host.
func fakeEnv(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func fakeResolver(goos, home string, vars map[string]string) resolver {
	return resolver{
		goos:   goos,
		getenv: fakeEnv(vars),
		home:   func() (string, error) { return home, nil },
	}
}

func TestResolverPlatformLayout(t *testing.T) {
	const home = "/home/u"
	const winHome = `C:\Users\u`

	tests := []struct {
		name    string
		res     resolver
		config  string
		data    string
		cache   string
		logs    string
		windows bool
	}{
		{
			name:   "linux defaults",
			res:    fakeResolver("linux", home, nil),
			config: "/home/u/.config/boop",
			data:   "/home/u/.local/share/boop",
			cache:  "/home/u/.cache/boop",
			logs:   "/home/u/.local/state/boop/logs",
		},
		{
			name: "linux xdg overrides",
			res: fakeResolver("linux", home, map[string]string{
				"XDG_CONFIG_HOME": "/xdg/cfg",
				"XDG_DATA_HOME":   "/xdg/data",
				"XDG_CACHE_HOME":  "/xdg/cache",
				"XDG_STATE_HOME":  "/xdg/state",
			}),
			config: "/xdg/cfg/boop",
			data:   "/xdg/data/boop",
			cache:  "/xdg/cache/boop",
			logs:   "/xdg/state/boop/logs",
		},
		{
			name: "linux ignores relative xdg values",
			res: fakeResolver("linux", home, map[string]string{
				"XDG_CONFIG_HOME": "relative/cfg",
			}),
			config: "/home/u/.config/boop",
			data:   "/home/u/.local/share/boop",
			cache:  "/home/u/.cache/boop",
			logs:   "/home/u/.local/state/boop/logs",
		},
		{
			name:   "darwin",
			res:    fakeResolver("darwin", home, nil),
			config: "/home/u/Library/Application Support/boop",
			data:   "/home/u/Library/Application Support/boop",
			cache:  "/home/u/Library/Caches/boop",
			logs:   "/home/u/Library/Logs/boop",
		},
		{
			name: "windows",
			res: fakeResolver("windows", winHome, map[string]string{
				"AppData":      `C:\Users\u\AppData\Roaming`,
				"LocalAppData": `C:\Users\u\AppData\Local`,
			}),
			config:  `C:\Users\u\AppData\Roaming\boop`,
			data:    `C:\Users\u\AppData\Roaming\boop`,
			cache:   `C:\Users\u\AppData\Local\boop`,
			logs:    `C:\Users\u\AppData\Local\boop\logs`,
			windows: true,
		},
		{
			name:    "windows without env falls back to home",
			res:     fakeResolver("windows", winHome, nil),
			config:  `C:\Users\u\AppData\Roaming\boop`,
			data:    `C:\Users\u\AppData\Roaming\boop`,
			cache:   `C:\Users\u\AppData\Local\boop`,
			logs:    `C:\Users\u\AppData\Local\boop\logs`,
			windows: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.windows && runtime.GOOS != "windows" {
				// filepath.Join uses the host separator, so only compare the
				// Windows expectations on Windows.
				t.Skip("path separators differ from the host")
			}
			checks := []struct {
				what string
				got  func() (string, error)
				want string
			}{
				{"config", tc.res.configDir, tc.config},
				{"data", tc.res.dataDir, tc.data},
				{"cache", tc.res.cacheDir, tc.cache},
				{"logs", tc.res.logDir, tc.logs},
			}
			for _, chk := range checks {
				got, err := chk.got()
				if err != nil {
					t.Fatalf("%s: unexpected error: %v", chk.what, err)
				}
				if got != filepath.FromSlash(chk.want) {
					t.Errorf("%s = %q, want %q", chk.what, got, chk.want)
				}
			}
		})
	}
}

func TestResolverOverrideRelocatesEverything(t *testing.T) {
	root := t.TempDir()
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			res := fakeResolver(goos, "/home/u", map[string]string{
				EnvConfigDir:      root,
				"XDG_CONFIG_HOME": "/xdg/cfg",
				"AppData":         `C:\roaming`,
			})
			got := map[string]string{}
			for what, fn := range map[string]func() (string, error){
				"config": res.configDir, "data": res.dataDir,
				"cache": res.cacheDir, "logs": res.logDir,
			} {
				v, err := fn()
				if err != nil {
					t.Fatalf("%s: %v", what, err)
				}
				got[what] = v
			}
			want := map[string]string{
				"config": root,
				"data":   root,
				"cache":  filepath.Join(root, cacheName),
				"logs":   filepath.Join(root, logsName),
			}
			for k, w := range want {
				if got[k] != w {
					t.Errorf("%s = %q, want %q", k, got[k], w)
				}
			}
		})
	}
}

func TestOverrideMakesRelativePathsAbsolute(t *testing.T) {
	t.Setenv(EnvConfigDir, "relative-boop-dir")
	dir, err := ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir: %v", err)
	}
	if !filepath.IsAbs(dir) {
		t.Errorf("ConfigDir = %q, want an absolute path", dir)
	}
}

func TestExportedPathsUseOverride(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvConfigDir, root)

	tests := []struct {
		name string
		got  func() (string, error)
		want string
	}{
		{"ConfigDir", ConfigDir, root},
		{"DataDir", DataDir, root},
		{"CacheDir", CacheDir, filepath.Join(root, cacheName)},
		{"LogDir", LogDir, filepath.Join(root, logsName)},
		{"ConfigFile", ConfigFile, filepath.Join(root, configFileName)},
		{"DatabasePath", DatabasePath, filepath.Join(root, databaseName)},
		{"SessionsDir", SessionsDir, filepath.Join(root, sessionsName)},
		{"LogFile", LogFile, filepath.Join(root, logsName, logFileName)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.got()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("= %q, want %q", got, tc.want)
			}
		})
	}
}

func TestConfigDirHonoursXDGConfigHome(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("XDG layout is Linux-only")
	}
	base := t.TempDir()
	t.Setenv(EnvConfigDir, "")
	t.Setenv("XDG_CONFIG_HOME", base)

	got, err := ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir: %v", err)
	}
	if want := filepath.Join(base, appDir); got != want {
		t.Errorf("ConfigDir = %q, want %q", got, want)
	}
}

func TestEnsureDirs(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvConfigDir, filepath.Join(root, "boop"))

	if err := EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	// Idempotent: a second call on an existing tree must succeed.
	if err := EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs (second call): %v", err)
	}

	for _, fn := range []func() (string, error){ConfigDir, DataDir, CacheDir, LogDir, SessionsDir} {
		dir, err := fn()
		if err != nil {
			t.Fatalf("resolve dir: %v", err)
		}
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", dir)
		}
		if runtime.GOOS != "windows" {
			if perm := info.Mode().Perm(); perm != 0o700 {
				t.Errorf("%s mode = %o, want 700", dir, perm)
			}
		}
	}
}

func TestResolverHomeFailurePropagates(t *testing.T) {
	res := resolver{
		goos:   "linux",
		getenv: fakeEnv(nil),
		home:   func() (string, error) { return "", os.ErrNotExist },
	}
	if _, err := res.configDir(); err == nil {
		t.Fatal("expected an error when the home directory cannot be resolved")
	}
}
