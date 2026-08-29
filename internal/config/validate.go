package config

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"

	"github.com/boop-dev/boop/internal/permissions"
)

// LogLevels lists the accepted logging levels, in increasing severity (§44).
var LogLevels = []string{"trace", "debug", "info", "warn", "error"}

// Validate checks the configuration and returns advisory warnings alongside
// any aggregated errors.
//
// The split matters: an error means Boop cannot safely act on this
// configuration and startup must stop, while a warning means the user has
// chosen something legal but risky (§23) and deserves to be told, once,
// loudly. Errors are joined so a single run reports every problem instead of
// making the user fix them one at a time.
func (c *Config) Validate() (warnings []string, err error) {
	var errs []error

	errs = append(errs, c.validateExecution()...)
	errs = append(errs, c.validatePermissions()...)
	errs = append(errs, c.validateWeb()...)
	errs = append(errs, c.validateProviders()...)
	errs = append(errs, c.validateRouting()...)
	errs = append(errs, c.validateLogging()...)
	errs = append(errs, c.validateNetwork()...)

	warnings = append(warnings, c.webWarnings()...)
	warnings = append(warnings, c.networkWarnings()...)
	return warnings, errors.Join(errs...)
}

// validateNetwork checks the outbound web access settings.
func (c *Config) validateNetwork() []error {
	var errs []error
	n := c.Network

	if n.Timeout <= 0 {
		errs = append(errs, fmt.Errorf("network.timeout: must be positive, got %s", n.Timeout))
	}
	if n.MaxResponseBytes <= 0 {
		errs = append(errs, fmt.Errorf("network.max_response_bytes: must be positive, got %d", n.MaxResponseBytes))
	}
	if n.MaxRedirects < 0 {
		errs = append(errs, fmt.Errorf("network.max_redirects: must not be negative, got %d", n.MaxRedirects))
	}
	if p := strings.ToLower(strings.TrimSpace(n.Search.Provider)); p != "" && p != DefaultSearchProvider {
		errs = append(errs, fmt.Errorf("network.search.provider: %q is not implemented (only %q is)", n.Search.Provider, DefaultSearchProvider))
	}
	if n.Search.MaxResults < 0 {
		errs = append(errs, fmt.Errorf("network.search.max_results: must not be negative, got %d", n.Search.MaxResults))
	}
	switch strings.ToLower(strings.TrimSpace(n.Search.SafeSearch)) {
	case "", "off", "moderate", "strict":
	default:
		errs = append(errs, fmt.Errorf("network.search.safe_search: %q is not valid (want off, moderate or strict)", n.Search.SafeSearch))
	}
	// A custom User-Agent must still identify Boop, so site operators can
	// attribute the traffic and block it if they choose.
	if ua := strings.TrimSpace(n.UserAgent); ua != "" && !strings.Contains(strings.ToLower(ua), "boop") {
		errs = append(errs, fmt.Errorf("network.user_agent: %q must contain %q so outbound requests stay attributable", n.UserAgent, "boop"))
	}
	for i, d := range n.AllowedDomains {
		if strings.TrimSpace(d) == "" {
			errs = append(errs, fmt.Errorf("network.allowed_domains[%d]: empty entry", i))
		}
	}
	for i, d := range n.BlockedDomains {
		if strings.TrimSpace(d) == "" {
			errs = append(errs, fmt.Errorf("network.blocked_domains[%d]: empty entry", i))
		}
	}
	return errs
}

// networkWarnings flags outbound settings that are legal but worth saying out
// loud, since they widen what a model-supplied URL can reach.
func (c *Config) networkWarnings() []string {
	n := c.Network
	if !n.Enabled {
		return nil
	}
	var warnings []string
	if n.AllowPrivateNetworks {
		warnings = append(warnings,
			"network.allow_private_networks is enabled: a URL chosen by the model can reach loopback, "+
				"link-local and private addresses, including cloud metadata endpoints")
	}
	if !n.RespectRobots {
		warnings = append(warnings,
			"network.respect_robots is disabled: Boop will scrape sites that ask not to be scraped")
	}
	return warnings
}

// validateExecution checks the execution mode.
func (c *Config) validateExecution() []error {
	if !c.Execution.Mode.Valid() {
		return []error{fmt.Errorf("execution.mode: %q is not a valid mode (want %q or %q)",
			c.Execution.Mode, permissions.ModeConfirm, permissions.ModeAuto)}
	}
	return nil
}

// validatePermissions checks every category rule.
//
// An unrecognised rule is an error rather than a fallback to "confirm":
// silently reinterpreting a permission setting is exactly the kind of surprise
// the permission engine exists to prevent.
func (c *Config) validatePermissions() []error {
	var errs []error
	rules := c.Permissions.Rules()
	names := make([]string, 0, len(rules))
	for cat := range rules {
		names = append(names, string(cat))
	}
	sort.Strings(names)
	for _, name := range names {
		rule := rules[permissions.Category(name)]
		switch rule {
		case permissions.RuleAllow, permissions.RuleConfirm, permissions.RuleDeny:
		default:
			errs = append(errs, fmt.Errorf("permissions.%s: %q is not a valid rule (want %q, %q or %q)",
				name, rule, permissions.RuleAllow, permissions.RuleConfirm, permissions.RuleDeny))
		}
	}
	return errs
}

// validateWeb checks the WebUI bind address and port.
func (c *Config) validateWeb() []error {
	var errs []error
	if c.Web.Port < 1 || c.Web.Port > 65535 {
		errs = append(errs, fmt.Errorf("web.port: %d is out of range (want 1-65535)", c.Web.Port))
	}
	if net.ParseIP(strings.TrimSpace(c.Web.Listen)) == nil {
		errs = append(errs, fmt.Errorf("web.listen: %q is not an IP address (use 127.0.0.1 for local-only, 0.0.0.0 for all interfaces)", c.Web.Listen))
	}
	return errs
}

// validateProviders checks the active provider and the provider table.
func (c *Config) validateProviders() []error {
	var errs []error

	switch entry, ok := c.Providers[c.Provider]; {
	case strings.TrimSpace(c.Provider) == "":
		errs = append(errs, errors.New("provider: no active provider is configured"))
	case !ok:
		errs = append(errs, fmt.Errorf("provider: %q is not defined in providers (defined: %s)",
			c.Provider, strings.Join(providerNames(c.Providers), ", ")))
	case entry.Disabled:
		errs = append(errs, fmt.Errorf("provider: %q is disabled; enable it or select another provider", c.Provider))
	}

	for _, name := range providerNames(c.Providers) {
		pc := c.Providers[name]
		if looksLikeLiteralKey(pc.APIKeyEnv) {
			errs = append(errs, fmt.Errorf("providers.%s.api_key_env: looks like a literal API key; it must name an environment variable instead (for example api_key_env: %s_API_KEY)",
				name, strings.ToUpper(sanitizeEnvName(name))))
		}
		for _, header := range sortedKeys(pc.Headers) {
			if looksLikeLiteralHeaderSecret(pc.Headers[header]) {
				errs = append(errs, fmt.Errorf("providers.%s.headers.%s: looks like a literal credential; use api_key_env so the secret stays out of the config file",
					name, header))
			}
		}
	}
	return errs
}

// validateRouting checks that routing and fallback only name known providers.
func (c *Config) validateRouting() []error {
	var errs []error
	for _, class := range sortedKeys(c.Routing) {
		target := c.Routing[class]
		if _, ok := c.Providers[target.Provider]; !ok {
			errs = append(errs, fmt.Errorf("routing.%s.provider: %q is not defined in providers", class, target.Provider))
		}
	}
	for i, name := range c.Fallback {
		if _, ok := c.Providers[name]; !ok {
			errs = append(errs, fmt.Errorf("fallback[%d]: %q is not defined in providers", i, name))
		}
	}
	return errs
}

// validateLogging checks the log level.
func (c *Config) validateLogging() []error {
	for _, lvl := range LogLevels {
		if c.Logging.Level == lvl {
			return nil
		}
	}
	return []error{fmt.Errorf("logging.level: %q is not a valid level (want one of %s)",
		c.Logging.Level, strings.Join(LogLevels, ", "))}
}

// webWarnings reports legal but risky WebUI exposure.
//
// PROJECT.md §23: never assume a LAN is automatically trustworthy. Binding
// beyond loopback puts an interface that can run arbitrary commands on the
// network, so doing it without authentication or origin validation is worth
// saying out loud even though the user asked for it.
func (c *Config) webWarnings() []string {
	ip := net.ParseIP(strings.TrimSpace(c.Web.Listen))
	var warnings []string
	if ip != nil && !ip.IsLoopback() {
		if !c.Web.Auth.Enabled {
			warnings = append(warnings, fmt.Sprintf(
				"web.listen %s is not loopback and web.auth.enabled is false: the WebUI will be reachable on the network without authentication", c.Web.Listen))
		}
		if len(c.Web.AllowedOrigins) == 0 {
			warnings = append(warnings, fmt.Sprintf(
				"web.listen %s is not loopback and web.allowed_origins is empty: browser and WebSocket origins will not be validated", c.Web.Listen))
		}
	}
	if c.Web.Auth.Enabled && strings.TrimSpace(c.Web.Auth.TokenEnv) == "" {
		warnings = append(warnings,
			"web.auth.enabled is true but web.auth.token_env is empty: no access token can be resolved")
	}
	return warnings
}

// envVarNamePattern matches a plausible environment variable name.
var envVarNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// credentialPrefixes are prefixes used by the vendors Boop supports (and by
// common tooling) for literal secrets. They are checked in addition to the
// shape test because some of them are valid identifiers on their own.
var credentialPrefixes = []string{
	"sk-", "sk_", "pk-", "pk_", "xai-", "xai_", "ghp_", "gho_", "ghs_", "github_pat_", "Bearer ", "bearer ",
}

// looksLikeLiteralKey reports whether an api_key_env value is a secret that
// was pasted where a variable name belongs.
//
// The test is deliberately shape-based rather than value-based, and callers
// must never echo the offending string: an over-eager heuristic that leaks the
// key into an error message would be worse than the mistake it detects.
func looksLikeLiteralKey(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if hasCredentialPrefix(s) {
		return true
	}
	if !envVarNamePattern.MatchString(s) {
		return true
	}
	// No realistic environment variable name is this long; API keys routinely
	// are.
	return len(s) > 64
}

// looksLikeLiteralHeaderSecret reports whether a custom header value carries a
// credential inline.
func looksLikeLiteralHeaderSecret(v string) bool {
	return hasCredentialPrefix(strings.TrimSpace(v))
}

// hasCredentialPrefix reports whether s starts with a known secret prefix.
func hasCredentialPrefix(s string) bool {
	for _, p := range credentialPrefixes {
		if strings.HasPrefix(s, p) && len(s) > len(p) {
			return true
		}
	}
	return false
}

// sanitizeEnvName turns a provider name into an environment-variable-safe stem
// for use in guidance messages.
func sanitizeEnvName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "PROVIDER"
	}
	return b.String()
}

// providerNames returns the provider keys in a stable order, so error output
// does not depend on map iteration.
func providerNames(m map[string]ProviderConfig) []string { return sortedKeys(m) }

// sortedKeys returns the keys of m in ascending order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
