package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestDefaultNetworkIsOffAndConservative(t *testing.T) {
	n := Default().Network

	if n.Enabled {
		t.Error("outbound web access must be off by default: it sends user text to third-party servers")
	}
	if n.AllowPrivateNetworks {
		t.Error("private-network access must be off by default (SSRF guard)")
	}
	if !n.RespectRobots {
		t.Error("robots.txt must be respected by default")
	}
	if n.UserAgent != "" {
		t.Errorf("UserAgent = %q, want empty so the built-in Boop agent is used", n.UserAgent)
	}
	if got, want := n.Timeout.Std(), 30*time.Second; got != want {
		t.Errorf("Timeout = %s, want %s", got, want)
	}
	if n.MaxResponseBytes != 5<<20 {
		t.Errorf("MaxResponseBytes = %d, want %d", n.MaxResponseBytes, 5<<20)
	}
	if n.Search.Provider != "duckduckgo" {
		t.Errorf("Search.Provider = %q, want duckduckgo", n.Search.Provider)
	}
	if n.Search.MaxResults != 10 {
		t.Errorf("Search.MaxResults = %d, want 10", n.Search.MaxResults)
	}
}

func TestDefaultConfigValidatesWithoutWarnings(t *testing.T) {
	warnings, err := Default().Validate()
	if err != nil {
		t.Fatalf("Default().Validate() = %v, want nil", err)
	}
	for _, w := range warnings {
		if strings.Contains(w, "network") {
			t.Errorf("default config produced a network warning: %s", w)
		}
	}
}

func TestValidateNetworkRejectsBadValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*NetworkConfig)
		want   string
	}{
		{"zero timeout", func(n *NetworkConfig) { n.Timeout = 0 }, "network.timeout"},
		{"zero size cap", func(n *NetworkConfig) { n.MaxResponseBytes = 0 }, "network.max_response_bytes"},
		{"negative redirects", func(n *NetworkConfig) { n.MaxRedirects = -1 }, "network.max_redirects"},
		{"unknown search provider", func(n *NetworkConfig) { n.Search.Provider = "google" }, "not implemented"},
		{"bad safe search", func(n *NetworkConfig) { n.Search.SafeSearch = "sometimes" }, "safe_search"},
		{"negative max results", func(n *NetworkConfig) { n.Search.MaxResults = -1 }, "max_results"},
		{"user agent without boop", func(n *NetworkConfig) { n.UserAgent = "Mozilla/5.0" }, "must contain"},
		{"empty allowed domain", func(n *NetworkConfig) { n.AllowedDomains = []string{""} }, "allowed_domains[0]"},
		{"empty blocked domain", func(n *NetworkConfig) { n.BlockedDomains = []string{" "} }, "blocked_domains[0]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.mutate(&c.Network)
			_, err := c.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate() = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateNetworkAcceptsCustomBoopUserAgent(t *testing.T) {
	c := Default()
	c.Network.UserAgent = "AcmeBoop/2.0 (+https://acme.example/bot)"
	if _, err := c.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil for a user agent containing boop", err)
	}
}

func TestNetworkWarningsOnlyFireWhenEnabled(t *testing.T) {
	// A risky setting on a disabled subsystem is not worth warning about.
	c := Default()
	c.Network.AllowPrivateNetworks = true
	c.Network.RespectRobots = false
	warnings, err := c.Validate()
	if err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	for _, w := range warnings {
		if strings.Contains(w, "allow_private_networks") || strings.Contains(w, "respect_robots") {
			t.Errorf("disabled network should not warn, got: %s", w)
		}
	}

	c.Network.Enabled = true
	warnings, err = c.Validate()
	if err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	var sawPrivate, sawRobots bool
	for _, w := range warnings {
		sawPrivate = sawPrivate || strings.Contains(w, "allow_private_networks")
		sawRobots = sawRobots || strings.Contains(w, "respect_robots")
	}
	if !sawPrivate {
		t.Error("enabling private networks must warn: a model-chosen URL could reach metadata endpoints")
	}
	if !sawRobots {
		t.Error("disabling robots.txt must warn")
	}
}

func TestNetworkConfigRoundTripsThroughYAML(t *testing.T) {
	c := Default()
	c.Network.Enabled = true
	c.Network.UserAgent = "Boop/9.9 (+https://example.test)"
	c.Network.AllowedDomains = []string{"example.com"}
	c.Network.Search.Region = "dk-da"

	path := t.TempDir() + "/config.yaml"
	if err := c.SaveTo(path); err != nil {
		t.Fatalf("SaveTo() = %v", err)
	}
	got, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom() = %v", err)
	}
	if !got.Network.Enabled {
		t.Error("Enabled did not survive the round trip")
	}
	if got.Network.UserAgent != c.Network.UserAgent {
		t.Errorf("UserAgent = %q, want %q", got.Network.UserAgent, c.Network.UserAgent)
	}
	if len(got.Network.AllowedDomains) != 1 || got.Network.AllowedDomains[0] != "example.com" {
		t.Errorf("AllowedDomains = %v, want [example.com]", got.Network.AllowedDomains)
	}
	if got.Network.Search.Region != "dk-da" {
		t.Errorf("Search.Region = %q, want dk-da", got.Network.Search.Region)
	}
	if got.Network.Timeout != c.Network.Timeout {
		t.Errorf("Timeout = %s, want %s", got.Network.Timeout, c.Network.Timeout)
	}
}

func TestPartialNetworkConfigKeepsDefaults(t *testing.T) {
	// A file that sets one network key must not blank the others.
	path := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(path, []byte("version: 1\nnetwork:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom() = %v", err)
	}
	if !got.Network.Enabled {
		t.Error("Enabled = false, want true from the file")
	}
	if got.Network.Timeout.Std() != 30*time.Second {
		t.Errorf("Timeout = %s, want the 30s default to survive", got.Network.Timeout)
	}
	if !got.Network.RespectRobots {
		t.Error("RespectRobots must keep its true default when the file omits it")
	}
	if got.Network.Search.MaxResults != 10 {
		t.Errorf("Search.MaxResults = %d, want the default 10", got.Network.Search.MaxResults)
	}
}
