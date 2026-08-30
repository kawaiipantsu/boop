// Package config defines Boop's on-disk configuration.
//
// The struct layout here is the contract; loading, defaulting, validation and
// OS path resolution live alongside it in this package.
package config

import (
	"time"

	"github.com/kawaiipantsu/boop/internal/permissions"
)

// Config is the root configuration document (config.yaml).
type Config struct {
	Version int `yaml:"version" json:"version"`

	// Provider is the name of the active entry in Providers.
	Provider string `yaml:"provider" json:"provider"`
	// Model is the active model ID; empty selects the provider default.
	Model string `yaml:"model" json:"model"`

	Execution   ExecutionConfig           `yaml:"execution" json:"execution"`
	Tools       ToolsConfig               `yaml:"tools,omitempty" json:"tools,omitempty"`
	Agents      AgentsConfig              `yaml:"agents" json:"agents"`
	Web         WebConfig                 `yaml:"web" json:"web"`
	Providers   map[string]ProviderConfig `yaml:"providers" json:"providers"`
	Permissions PermissionsConfig         `yaml:"permissions" json:"permissions"`
	Network     NetworkConfig             `yaml:"network" json:"network"`
	Routing     map[string]RouteTarget    `yaml:"routing,omitempty" json:"routing,omitempty"`
	Fallback    []string                  `yaml:"fallback,omitempty" json:"fallback,omitempty"`
	Logging     LoggingConfig             `yaml:"logging" json:"logging"`
}

// ToolsConfig holds tool system configurations and custom user-declared tools.
type ToolsConfig struct {
	Custom map[string]CustomToolConfig `yaml:"custom,omitempty" json:"custom,omitempty"`
}

// CustomToolConfig declares a user-defined tool invoked directly via command.
type CustomToolConfig struct {
	Description string               `yaml:"description" json:"description"`
	Command     []string             `yaml:"command" json:"command"`
	Schema      map[string]any       `yaml:"schema,omitempty" json:"schema,omitempty"`
	Permission  CustomToolPermission `yaml:"permission,omitempty" json:"permission,omitempty"`
	Timeout     Duration             `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// CustomToolPermission specifies the declared permission category and risk level.
type CustomToolPermission struct {
	Category permissions.Category `yaml:"category,omitempty" json:"category,omitempty"`
	Risk     permissions.Risk     `yaml:"risk,omitempty" json:"risk,omitempty"`
}

// ExecutionConfig bounds the tool and repair loops.
type ExecutionConfig struct {
	Mode                 permissions.Mode `yaml:"mode" json:"mode"`
	CommandTimeout       Duration         `yaml:"command_timeout" json:"command_timeout"`
	MaxRetriesPerCommand int              `yaml:"max_retries_per_command" json:"max_retries_per_command"`
	MaxToolIterations    int              `yaml:"max_tool_iterations" json:"max_tool_iterations"`
	// Unrestricted skips confirmation entirely. It is deliberately not a
	// YAML key: this is a per-invocation decision made with an explicitly
	// named flag, not something that should sit in a config file where it
	// would be forgotten (§14). It never bypasses the production gate.
	Unrestricted bool `yaml:"-" json:"unrestricted,omitempty"`
}

// AgentsConfig bounds the agent scheduler.
type AgentsConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	Max     int  `yaml:"max" json:"max"`
}

// WebConfig controls the local WebUI. Defaults are loopback-only by design.
type WebConfig struct {
	Enabled bool       `yaml:"enabled" json:"enabled"`
	Listen  string     `yaml:"listen" json:"listen"`
	Port    int        `yaml:"port" json:"port"`
	Auth    AuthConfig `yaml:"auth" json:"auth"`
	// AllowedOrigins is required when binding beyond loopback.
	AllowedOrigins []string `yaml:"allowed_origins,omitempty" json:"allowed_origins,omitempty"`
	// TrustedProxyHeaders enables proxy-aware client address resolution.
	TrustedProxyHeaders bool `yaml:"trusted_proxy_headers,omitempty" json:"trusted_proxy_headers,omitempty"`
}

// AuthConfig configures WebUI access control.
type AuthConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// TokenEnv names the environment variable holding the access token.
	// The token itself is never stored in the config file.
	TokenEnv string `yaml:"token_env,omitempty" json:"token_env,omitempty"`
}

// ProviderConfig describes one configured backend.
type ProviderConfig struct {
	// Type selects the adapter: lemonade, lmstudio, ollama, openai,
	// anthropic, xai, or openai-compatible.
	Type    string `yaml:"type" json:"type"`
	BaseURL string `yaml:"base_url,omitempty" json:"base_url,omitempty"`
	// APIKeyEnv names the environment variable holding the credential.
	// Literal keys are never accepted here.
	APIKeyEnv string            `yaml:"api_key_env,omitempty" json:"api_key_env,omitempty"`
	Timeout   Duration          `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	Headers   map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	Disabled  bool              `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}

// PermissionsConfig is the category rule table as authored in YAML.
type PermissionsConfig struct {
	Filesystem struct {
		Read  permissions.Rule `yaml:"read" json:"read"`
		Write permissions.Rule `yaml:"write" json:"write"`
	} `yaml:"filesystem" json:"filesystem"`
	Shell struct {
		Execute permissions.Rule `yaml:"execute" json:"execute"`
	} `yaml:"shell" json:"shell"`
	Git struct {
		Read   permissions.Rule `yaml:"read" json:"read"`
		Commit permissions.Rule `yaml:"commit" json:"commit"`
		Push   permissions.Rule `yaml:"push" json:"push"`
	} `yaml:"git" json:"git"`
	Network struct {
		HTTP permissions.Rule `yaml:"http" json:"http"`
		// Fetch covers retrieving an arbitrary external URL, and Search
		// covers a web search, which discloses the query to a third party.
		// They are separate so searching can be allowed while an arbitrary
		// fetch still confirms.
		Fetch  permissions.Rule `yaml:"fetch" json:"fetch"`
		Search permissions.Rule `yaml:"search" json:"search"`
	} `yaml:"network" json:"network"`
	Production struct {
		Change permissions.Rule `yaml:"change" json:"change"`
	} `yaml:"production" json:"production"`
}

// NetworkConfig controls Boop's outbound access to the public internet:
// fetching URLs and running web searches.
//
// This is distinct from WebConfig, which serves Boop's own local WebUI. Reaching
// out to third-party sites is off by default and must be turned on deliberately,
// because it sends the user's query text to servers they did not choose.
type NetworkConfig struct {
	// Enabled is the master toggle for all outbound fetching and searching.
	// With it false, the fetch and websearch tools refuse to run at all.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// UserAgent overrides the request User-Agent. The default identifies Boop
	// so site operators can attribute and, if they wish, block the traffic.
	UserAgent        string   `yaml:"user_agent,omitempty" json:"user_agent,omitempty"`
	Timeout          Duration `yaml:"timeout" json:"timeout"`
	MaxResponseBytes int64    `yaml:"max_response_bytes" json:"max_response_bytes"`
	MaxRedirects     int      `yaml:"max_redirects" json:"max_redirects"`
	// AllowPrivateNetworks permits requests to loopback, link-local and
	// private ranges. Off by default: a model-supplied URL pointing at
	// 169.254.169.254 or an intranet host is a server-side request forgery
	// vector, not a feature.
	AllowPrivateNetworks bool `yaml:"allow_private_networks" json:"allow_private_networks"`
	// AllowedDomains, when non-empty, is an allowlist: nothing else is fetched.
	AllowedDomains []string `yaml:"allowed_domains,omitempty" json:"allowed_domains,omitempty"`
	// BlockedDomains is always denied, and takes precedence over AllowedDomains.
	BlockedDomains []string `yaml:"blocked_domains,omitempty" json:"blocked_domains,omitempty"`
	// RespectRobots honours robots.txt when scraping. Leaving this on is the
	// difference between a well-behaved client and an abusive one.
	RespectRobots bool         `yaml:"respect_robots" json:"respect_robots"`
	Search        SearchConfig `yaml:"search" json:"search"`
}

// SearchConfig selects and bounds the web search backend.
type SearchConfig struct {
	// Provider names the search backend. Only "duckduckgo" is implemented.
	Provider   string `yaml:"provider" json:"provider"`
	MaxResults int    `yaml:"max_results" json:"max_results"`
	// SafeSearch is off, moderate or strict.
	SafeSearch string `yaml:"safe_search" json:"safe_search"`
	// Region is a DuckDuckGo region code such as "wt-wt" (no region).
	Region string `yaml:"region,omitempty" json:"region,omitempty"`
}

// RouteTarget selects a provider/model pair for a routing class.
type RouteTarget struct {
	Provider string `yaml:"provider" json:"provider"`
	Model    string `yaml:"model" json:"model"`
}

// LoggingConfig controls structured logging.
type LoggingConfig struct {
	// Level is one of trace, debug, info, warn, error.
	Level string `yaml:"level" json:"level"`
	// File overrides the platform-default log location when set.
	File string `yaml:"file,omitempty" json:"file,omitempty"`
	// Format is text or json. Text is for humans reading a log file; json
	// is for anything shipping logs somewhere that parses them.
	Format string `yaml:"format" json:"format"`
	// MaxSizeMB is the size at which the log file rotates. A long-running
	// agent runtime must not be able to fill a disk.
	MaxSizeMB int `yaml:"max_size_mb" json:"max_size_mb"`
	// MaxBackups is how many rotated files are kept.
	MaxBackups int `yaml:"max_backups" json:"max_backups"`
}

// Duration is a time.Duration that marshals as a human string ("300s") in YAML
// and JSON, so config files stay readable.
type Duration time.Duration

// Std returns the underlying time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// String renders the duration in Go's canonical form.
func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML accepts either a duration string or an integer of seconds.
func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err == nil {
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		*d = Duration(parsed)
		return nil
	}
	var secs int64
	if err := unmarshal(&secs); err != nil {
		return err
	}
	*d = Duration(time.Duration(secs) * time.Second)
	return nil
}

// MarshalYAML renders the duration as a string.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// UnmarshalJSON accepts a duration string or a number of nanoseconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	if len(b) > 1 && b[0] == '"' {
		parsed, err := time.ParseDuration(string(b[1 : len(b)-1]))
		if err != nil {
			return err
		}
		*d = Duration(parsed)
		return nil
	}
	var ns int64
	if _, err := fmtSscan(string(b), &ns); err != nil {
		return err
	}
	*d = Duration(ns)
	return nil
}

// MarshalJSON renders the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(`"` + d.String() + `"`), nil
}
