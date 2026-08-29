// Package config defines Boop's on-disk configuration.
//
// The struct layout here is the contract; loading, defaulting, validation and
// OS path resolution live alongside it in this package.
package config

import (
	"time"

	"github.com/boop-dev/boop/internal/permissions"
)

// Config is the root configuration document (config.yaml).
type Config struct {
	Version int `yaml:"version" json:"version"`

	// Provider is the name of the active entry in Providers.
	Provider string `yaml:"provider" json:"provider"`
	// Model is the active model ID; empty selects the provider default.
	Model string `yaml:"model" json:"model"`

	Execution   ExecutionConfig           `yaml:"execution" json:"execution"`
	Agents      AgentsConfig              `yaml:"agents" json:"agents"`
	Web         WebConfig                 `yaml:"web" json:"web"`
	Providers   map[string]ProviderConfig `yaml:"providers" json:"providers"`
	Permissions PermissionsConfig         `yaml:"permissions" json:"permissions"`
	Routing     map[string]RouteTarget    `yaml:"routing,omitempty" json:"routing,omitempty"`
	Fallback    []string                  `yaml:"fallback,omitempty" json:"fallback,omitempty"`
	Logging     LoggingConfig             `yaml:"logging" json:"logging"`
}

// ExecutionConfig bounds the tool and repair loops.
type ExecutionConfig struct {
	Mode                 permissions.Mode `yaml:"mode" json:"mode"`
	CommandTimeout       Duration         `yaml:"command_timeout" json:"command_timeout"`
	MaxRetriesPerCommand int              `yaml:"max_retries_per_command" json:"max_retries_per_command"`
	MaxToolIterations    int              `yaml:"max_tool_iterations" json:"max_tool_iterations"`
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
	} `yaml:"network" json:"network"`
	Production struct {
		Change permissions.Rule `yaml:"change" json:"change"`
	} `yaml:"production" json:"production"`
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
