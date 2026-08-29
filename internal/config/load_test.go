package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/boop-dev/boop/internal/permissions"
)

func TestLoadCreatesDefaultsWhenMissing(t *testing.T) {
	root := filepath.Join(t.TempDir(), "boop")
	t.Setenv(EnvConfigDir, root)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Errorf("Load returned %+v, want defaults", cfg)
	}

	path := filepath.Join(root, configFileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("config file was not created: %v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("config file mode = %o, want 600", perm)
		}
	}

	// A second Load must read the file back rather than rewrite it.
	again, err := Load()
	if err != nil {
		t.Fatalf("Load (second): %v", err)
	}
	if !reflect.DeepEqual(again, cfg) {
		t.Errorf("second Load = %+v, want %+v", again, cfg)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")

	want := Default()
	want.Provider = "ollama"
	want.Model = "qwen3:8b"
	want.Execution.Mode = permissions.ModeAuto
	want.Execution.CommandTimeout = Duration(90 * time.Second)
	want.Web.Enabled = true
	want.Web.AllowedOrigins = []string{"http://boop.local"}
	want.Web.Auth = AuthConfig{Enabled: true, TokenEnv: "BOOP_WEB_TOKEN"}
	want.Routing = map[string]RouteTarget{"fast": {Provider: "lmstudio", Model: "small"}}
	want.Fallback = []string{"lemonade", "ollama"}
	want.Logging.Level = "debug"
	want.Permissions.Shell.Execute = permissions.RuleDeny

	if err := want.SaveTo(path); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	got, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestSaveToIsAtomicAndPrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, configFileName)

	if err := Default().SaveTo(path); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	// Overwriting an existing file must also work, and must not leave
	// temporary files behind.
	c := Default()
	c.Model = "second-write"
	if err := c.SaveTo(path); err != nil {
		t.Fatalf("SaveTo (overwrite): %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != configFileName {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory contains %v, want only %s", names, configFileName)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("mode = %o, want 600", perm)
		}
	}
}

func TestSaveUsesPlatformPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "boop")
	t.Setenv(EnvConfigDir, root)

	c := Default()
	c.Model = "saved"
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := LoadFrom(filepath.Join(root, configFileName))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got.Model != "saved" {
		t.Errorf("model = %q, want %q", got.Model, "saved")
	}
}

func TestLoadFromPartialFileKeepsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), configFileName)
	body := `
provider: ollama
execution:
  mode: auto
providers:
  openai:
    base_url: http://proxy.internal/v1
  local-dev:
    type: openai-compatible
    base_url: http://127.0.0.1:9999
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}

	tests := []struct {
		name string
		got  any
		want any
	}{
		{"overridden provider", got.Provider, "ollama"},
		{"overridden mode", got.Execution.Mode, permissions.ModeAuto},
		{"default timeout survives", got.Execution.CommandTimeout.Std(), 300 * time.Second},
		{"default retries survive", got.Execution.MaxRetriesPerCommand, 3},
		{"default agents survive", got.Agents.Max, 5},
		{"default web port survives", got.Web.Port, 8585},
		{"default permission survives", got.Permissions.Git.Push, permissions.RuleConfirm},
		{"default log level survives", got.Logging.Level, "info"},
		{"unmentioned provider survives", got.Providers["ollama"].BaseURL, "http://127.0.0.1:11434"},
		{"partial provider keeps type", got.Providers["openai"].Type, "openai"},
		{"partial provider keeps api_key_env", got.Providers["openai"].APIKeyEnv, "OPENAI_API_KEY"},
		{"partial provider takes new base url", got.Providers["openai"].BaseURL, "http://proxy.internal/v1"},
		{"new provider is added", got.Providers["local-dev"].Type, "openai-compatible"},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
	if len(got.Providers) != len(DefaultProviders())+1 {
		t.Errorf("provider count = %d, want %d", len(got.Providers), len(DefaultProviders())+1)
	}
}

func TestLoadFromEmptyFileYieldsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), configFileName)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if !reflect.DeepEqual(got, Default()) {
		t.Errorf("empty file = %+v, want defaults", got)
	}
}

func TestLoadFromErrors(t *testing.T) {
	dir := t.TempDir()

	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("providers: [this is not a map]\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tests := []struct {
		name   string
		path   string
		notExt bool
	}{
		{name: "missing file", path: filepath.Join(dir, "absent.yaml"), notExt: true},
		{name: "malformed yaml", path: bad},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadFrom(tc.path)
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := errors.Is(err, fs.ErrNotExist); got != tc.notExt {
				t.Errorf("errors.Is(err, fs.ErrNotExist) = %v, want %v (err: %v)", got, tc.notExt, err)
			}
		})
	}
}

func TestMarshalIsReadableAndCommentFree(t *testing.T) {
	data, err := Default().Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out := string(data)

	for _, want := range []string{
		"provider: lemonade",
		"command_timeout: 5m0s",
		"  mode: confirm",
		"listen: 127.0.0.1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("marshalled config missing %q:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			t.Errorf("marshalled config contains a comment: %q", line)
		}
	}
}

func TestResolveAPIKey(t *testing.T) {
	const varName = "BOOP_TEST_API_KEY"
	const secret = "test-value-not-a-real-key"

	tests := []struct {
		name    string
		pc      ProviderConfig
		set     string
		want    string
		wantErr bool
		errHas  string
	}{
		{
			name: "variable set",
			pc:   ProviderConfig{Type: "openai", APIKeyEnv: varName},
			set:  secret,
			want: secret,
		},
		{
			name:    "variable unset",
			pc:      ProviderConfig{Type: "openai", APIKeyEnv: varName},
			wantErr: true,
			errHas:  varName,
		},
		{
			name:    "variable blank",
			pc:      ProviderConfig{Type: "openai", APIKeyEnv: varName},
			set:     "   ",
			wantErr: true,
			errHas:  varName,
		},
		{
			name:    "no api_key_env configured",
			pc:      ProviderConfig{Type: "lemonade"},
			wantErr: true,
			errHas:  "api_key_env",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(varName, tc.set)
			got, err := ResolveAPIKey(tc.pc)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				if !errors.Is(err, ErrMissingAPIKey) {
					t.Errorf("error %v does not wrap ErrMissingAPIKey", err)
				}
				if !strings.Contains(err.Error(), tc.errHas) {
					t.Errorf("error %q does not mention %q", err, tc.errHas)
				}
				if strings.Contains(err.Error(), secret) {
					t.Error("error message leaked the credential value")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("= %q, want %q", got, tc.want)
			}
		})
	}
}
