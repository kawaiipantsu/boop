package config

import (
	"reflect"
	"testing"
	"time"

	"github.com/kawaiipantsu/boop/internal/permissions"
)

func TestDefaultMatchesSpec(t *testing.T) {
	c := Default()

	tests := []struct {
		name string
		got  any
		want any
	}{
		{"version", c.Version, 1},
		{"provider", c.Provider, "lemonade"},
		{"model", c.Model, ""},
		{"execution.mode", c.Execution.Mode, permissions.ModeConfirm},
		{"execution.command_timeout", c.Execution.CommandTimeout.Std(), 300 * time.Second},
		{"execution.max_retries_per_command", c.Execution.MaxRetriesPerCommand, 3},
		{"execution.max_tool_iterations", c.Execution.MaxToolIterations, 50},
		{"agents.enabled", c.Agents.Enabled, true},
		{"agents.max", c.Agents.Max, 5},
		{"web.enabled", c.Web.Enabled, false},
		{"web.listen", c.Web.Listen, "127.0.0.1"},
		{"web.port", c.Web.Port, 8585},
		{"web.auth.enabled", c.Web.Auth.Enabled, false},
		{"logging.level", c.Logging.Level, "info"},
		{"permissions.filesystem.read", c.Permissions.Filesystem.Read, permissions.RuleAllow},
		{"permissions.filesystem.write", c.Permissions.Filesystem.Write, permissions.RuleConfirm},
		{"permissions.shell.execute", c.Permissions.Shell.Execute, permissions.RuleConfirm},
		{"permissions.git.read", c.Permissions.Git.Read, permissions.RuleAllow},
		{"permissions.git.commit", c.Permissions.Git.Commit, permissions.RuleConfirm},
		{"permissions.git.push", c.Permissions.Git.Push, permissions.RuleConfirm},
		{"permissions.network.http", c.Permissions.Network.HTTP, permissions.RuleConfirm},
		{"permissions.production.change", c.Permissions.Production.Change, permissions.RuleConfirm},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

func TestDefaultProviders(t *testing.T) {
	got := DefaultProviders()

	want := map[string]ProviderConfig{
		"lemonade":  {Type: "lemonade", BaseURL: "http://127.0.0.1:13305"},
		"lmstudio":  {Type: "lmstudio", BaseURL: "http://127.0.0.1:1234"},
		"ollama":    {Type: "ollama", BaseURL: "http://127.0.0.1:11434"},
		"openai":    {Type: "openai", BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY"},
		"anthropic": {Type: "anthropic", APIKeyEnv: "ANTHROPIC_API_KEY"},
		"xai":       {Type: "xai", APIKeyEnv: "XAI_API_KEY"},
	}
	if len(got) != len(want) {
		t.Fatalf("provider count = %d, want %d (%v)", len(got), len(want), providerNames(got))
	}
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("provider %q missing", name)
			continue
		}
		if !reflect.DeepEqual(g, w) {
			t.Errorf("provider %q = %+v, want %+v", name, g, w)
		}
	}
}

func TestDefaultIsIndependentPerCall(t *testing.T) {
	a := Default()
	b := Default()
	a.Providers["openai"] = ProviderConfig{Type: "mutated"}
	delete(a.Providers, "ollama")

	if b.Providers["openai"].Type != "openai" {
		t.Error("mutating one Default() affected another")
	}
	if _, ok := b.Providers["ollama"]; !ok {
		t.Error("deleting from one Default() affected another")
	}
}

func TestDefaultValidatesCleanly(t *testing.T) {
	warnings, err := Default().Validate()
	if err != nil {
		t.Fatalf("Default() must validate: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("Default() must not warn, got %v", warnings)
	}
}

func TestPolicyProjection(t *testing.T) {
	c := Default()
	c.Execution.Mode = permissions.ModeAuto
	p := c.Policy()

	if p.Mode != permissions.ModeAuto {
		t.Errorf("mode = %q, want %q", p.Mode, permissions.ModeAuto)
	}
	if p.Unrestricted {
		t.Error("Unrestricted must never be set from config")
	}
	want := map[permissions.Category]permissions.Rule{
		permissions.CatFilesystemRead:   permissions.RuleAllow,
		permissions.CatFilesystemWrite:  permissions.RuleConfirm,
		permissions.CatShellExecute:     permissions.RuleConfirm,
		permissions.CatGitRead:          permissions.RuleAllow,
		permissions.CatGitCommit:        permissions.RuleConfirm,
		permissions.CatGitPush:          permissions.RuleConfirm,
		permissions.CatNetworkHTTP:      permissions.RuleConfirm,
		permissions.CatNetworkFetch:     permissions.RuleConfirm,
		permissions.CatNetworkSearch:    permissions.RuleAllow,
		permissions.CatProductionChange: permissions.RuleConfirm,
	}
	for cat, rule := range want {
		if p.Rules[cat] != rule {
			t.Errorf("rule %s = %q, want %q", cat, p.Rules[cat], rule)
		}
	}
	// Derive the expected set rather than asserting a count. A hardcoded
	// number fails on every legitimate new category without saying which one
	// is missing, which is noise rather than a signal.
	for cat := range permissions.DefaultRules() {
		if _, ok := p.Rules[cat]; !ok {
			t.Errorf("category %s is missing from the projected policy", cat)
		}
	}
	for cat := range p.Rules {
		if _, ok := want[cat]; !ok {
			t.Errorf("category %s is projected but has no expectation here; add one", cat)
		}
	}
}
