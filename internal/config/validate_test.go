package config

import (
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/permissions"
)

// mutate returns a default config with fn applied, so each table row states
// only the thing it is testing.
func mutate(fn func(*Config)) *Config {
	c := Default()
	fn(c)
	return c
}

func TestValidateErrors(t *testing.T) {
	tests := []struct {
		name   string
		cfg    *Config
		errHas string
	}{
		{
			name:   "invalid execution mode",
			cfg:    mutate(func(c *Config) { c.Execution.Mode = permissions.Mode("yolo") }),
			errHas: "execution.mode",
		},
		{
			name:   "empty execution mode",
			cfg:    mutate(func(c *Config) { c.Execution.Mode = "" }),
			errHas: "execution.mode",
		},
		{
			name:   "invalid permission rule",
			cfg:    mutate(func(c *Config) { c.Permissions.Shell.Execute = permissions.Rule("maybe") }),
			errHas: "permissions.shell.execute",
		},
		{
			name:   "empty permission rule",
			cfg:    mutate(func(c *Config) { c.Permissions.Git.Push = "" }),
			errHas: "permissions.git.push",
		},
		{
			name:   "port zero",
			cfg:    mutate(func(c *Config) { c.Web.Port = 0 }),
			errHas: "web.port",
		},
		{
			name:   "port too high",
			cfg:    mutate(func(c *Config) { c.Web.Port = 70000 }),
			errHas: "web.port",
		},
		{
			name:   "negative port",
			cfg:    mutate(func(c *Config) { c.Web.Port = -1 }),
			errHas: "web.port",
		},
		{
			name:   "listen is a hostname",
			cfg:    mutate(func(c *Config) { c.Web.Listen = "localhost" }),
			errHas: "web.listen",
		},
		{
			name:   "listen is empty",
			cfg:    mutate(func(c *Config) { c.Web.Listen = "" }),
			errHas: "web.listen",
		},
		{
			name:   "listen includes a port",
			cfg:    mutate(func(c *Config) { c.Web.Listen = "127.0.0.1:8585" }),
			errHas: "web.listen",
		},
		{
			name:   "unknown active provider",
			cfg:    mutate(func(c *Config) { c.Provider = "nope" }),
			errHas: `provider: "nope" is not defined`,
		},
		{
			name:   "no active provider",
			cfg:    mutate(func(c *Config) { c.Provider = "" }),
			errHas: "no active provider",
		},
		{
			name: "active provider disabled",
			cfg: mutate(func(c *Config) {
				pc := c.Providers["lemonade"]
				pc.Disabled = true
				c.Providers["lemonade"] = pc
			}),
			errHas: "is disabled",
		},
		{
			name: "routing target unknown",
			cfg: mutate(func(c *Config) {
				c.Routing = map[string]RouteTarget{"reasoning": {Provider: "ghost", Model: "m"}}
			}),
			errHas: "routing.reasoning.provider",
		},
		{
			name:   "fallback entry unknown",
			cfg:    mutate(func(c *Config) { c.Fallback = []string{"lemonade", "ghost"} }),
			errHas: `fallback[1]: "ghost"`,
		},
		{
			name:   "invalid log level",
			cfg:    mutate(func(c *Config) { c.Logging.Level = "verbose" }),
			errHas: "logging.level",
		},
		{
			name:   "empty log level",
			cfg:    mutate(func(c *Config) { c.Logging.Level = "" }),
			errHas: "logging.level",
		},
		{
			name: "literal api key with vendor prefix",
			cfg: mutate(func(c *Config) {
				pc := c.Providers["openai"]
				pc.APIKeyEnv = "sk-" + strings.Repeat("A", 32)
				c.Providers["openai"] = pc
			}),
			errHas: "providers.openai.api_key_env",
		},
		{
			name: "literal api key that is not an identifier",
			cfg: mutate(func(c *Config) {
				pc := c.Providers["anthropic"]
				pc.APIKeyEnv = "abc123!def"
				c.Providers["anthropic"] = pc
			}),
			errHas: "providers.anthropic.api_key_env",
		},
		{
			name: "over-long api_key_env value",
			cfg: mutate(func(c *Config) {
				pc := c.Providers["xai"]
				pc.APIKeyEnv = strings.Repeat("A", 80)
				c.Providers["xai"] = pc
			}),
			errHas: "providers.xai.api_key_env",
		},
		{
			name: "literal credential in a header",
			cfg: mutate(func(c *Config) {
				pc := c.Providers["ollama"]
				pc.Headers = map[string]string{"Authorization": "Bearer " + strings.Repeat("z", 24)}
				c.Providers["ollama"] = pc
			}),
			errHas: "providers.ollama.headers.Authorization",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.cfg.Validate()
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if !strings.Contains(err.Error(), tc.errHas) {
				t.Errorf("error %q does not contain %q", err, tc.errHas)
			}
			if strings.Contains(err.Error(), "api_key_env") &&
				strings.Contains(err.Error(), strings.Repeat("A", 32)) {
				t.Error("validation error leaked the suspected credential")
			}
		})
	}
}

func TestValidateAggregatesEveryError(t *testing.T) {
	c := mutate(func(c *Config) {
		c.Execution.Mode = "nope"
		c.Logging.Level = "nope"
		c.Web.Port = 0
		c.Provider = "ghost"
	})
	_, err := c.Validate()
	if err == nil {
		t.Fatal("expected validation errors")
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("error %T does not aggregate", err)
	}
	if n := len(joined.Unwrap()); n != 4 {
		t.Errorf("aggregated %d errors, want 4: %v", n, err)
	}
}

func TestValidateAcceptsValidVariants(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Config
	}{
		{"auto mode", mutate(func(c *Config) { c.Execution.Mode = permissions.ModeAuto })},
		{"deny rule", mutate(func(c *Config) { c.Permissions.Network.HTTP = permissions.RuleDeny })},
		{"lowest port", mutate(func(c *Config) { c.Web.Port = 1 })},
		{"highest port", mutate(func(c *Config) { c.Web.Port = 65535 })},
		{"ipv6 loopback", mutate(func(c *Config) { c.Web.Listen = "::1" })},
		{"known routing", mutate(func(c *Config) {
			c.Routing = map[string]RouteTarget{"cheap": {Provider: "lmstudio", Model: "m"}}
			c.Fallback = []string{"ollama"}
		})},
		{"disabled non-active provider", mutate(func(c *Config) {
			pc := c.Providers["xai"]
			pc.Disabled = true
			c.Providers["xai"] = pc
		})},
		{"lowercase env var name", mutate(func(c *Config) {
			pc := c.Providers["openai"]
			pc.APIKeyEnv = "my_openai_key"
			c.Providers["openai"] = pc
		})},
		{"benign custom header", mutate(func(c *Config) {
			pc := c.Providers["ollama"]
			pc.Headers = map[string]string{"X-Tenant": "engineering"}
			c.Providers["ollama"] = pc
		})},
	}
	for _, level := range LogLevels {
		lvl := level
		tests = append(tests, struct {
			name string
			cfg  *Config
		}{"log level " + lvl, mutate(func(c *Config) { c.Logging.Level = lvl })})
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.cfg.Validate(); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateWarnings(t *testing.T) {
	tests := []struct {
		name     string
		cfg      *Config
		wantHas  []string
		wantNone bool
	}{
		{
			name: "loopback is quiet",
			cfg: mutate(func(c *Config) {
				c.Web.Enabled = true
			}),
			wantNone: true,
		},
		{
			name: "non-loopback without auth or origins",
			cfg: mutate(func(c *Config) {
				c.Web.Enabled = true
				c.Web.Listen = "0.0.0.0"
			}),
			wantHas: []string{"web.auth.enabled is false", "web.allowed_origins is empty"},
		},
		{
			name: "non-loopback with auth but no origins",
			cfg: mutate(func(c *Config) {
				c.Web.Enabled = true
				c.Web.Listen = "192.168.1.10"
				c.Web.Auth = AuthConfig{Enabled: true, TokenEnv: "BOOP_WEB_TOKEN"}
			}),
			wantHas: []string{"web.allowed_origins is empty"},
		},
		{
			name: "non-loopback fully configured",
			cfg: mutate(func(c *Config) {
				c.Web.Enabled = true
				c.Web.Listen = "192.168.1.10"
				c.Web.Auth = AuthConfig{Enabled: true, TokenEnv: "BOOP_WEB_TOKEN"}
				c.Web.AllowedOrigins = []string{"http://192.168.1.10:8585"}
			}),
			wantNone: true,
		},
		{
			name: "auth enabled without a token variable",
			cfg: mutate(func(c *Config) {
				c.Web.Auth = AuthConfig{Enabled: true}
			}),
			wantHas: []string{"token_env is empty"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			warnings, err := tc.cfg.Validate()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantNone {
				if len(warnings) != 0 {
					t.Fatalf("got warnings %v, want none", warnings)
				}
				return
			}
			if len(warnings) != len(tc.wantHas) {
				t.Fatalf("got %d warnings %v, want %d", len(warnings), warnings, len(tc.wantHas))
			}
			all := strings.Join(warnings, "\n")
			for _, want := range tc.wantHas {
				if !strings.Contains(all, want) {
					t.Errorf("warnings %v do not mention %q", warnings, want)
				}
			}
		})
	}
}

func TestLooksLikeLiteralKey(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"OPENAI_API_KEY", false},
		{"_UNDERSCORE_START", false},
		{"my_key_2", false},
		{"sk-abcdefghijklmnop", true},
		{"sk_live_abcdefghij", true},
		{"xai-abcdefghijklmnop", true},
		{"ghp_abcdefghijklmnop", true},
		{"Bearer abcdefghijklmnop", true},
		{"has spaces", true},
		{"2LEADING_DIGIT", true},
		{"has-hyphen", true},
		{strings.Repeat("A", 65), true},
		{strings.Repeat("A", 64), false},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := looksLikeLiteralKey(tc.in); got != tc.want {
				t.Errorf("looksLikeLiteralKey(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidateAfterRoundTrip(t *testing.T) {
	// A config that survives save/load must still validate; this guards the
	// defaulting path from producing something the validator rejects.
	path := t.TempDir() + "/config.yaml"
	if err := Default().SaveTo(path); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	warnings, err := cfg.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
}
