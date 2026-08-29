package config

import (
	"time"

	"github.com/kawaiipantsu/boop/internal/permissions"
)

// Version is the schema version written into new config files. Bump it only
// alongside a migration.
const Version = 1

// Defaults that are also referenced elsewhere (flags, help text, the /config
// editor) so the value is stated exactly once.
const (
	// DefaultProvider is Lemonade: Boop is local-first, so the out-of-the-box
	// provider must be one that runs on the user's own machine.
	DefaultProvider = "lemonade"

	// DefaultCommandTimeout bounds a single tool command.
	DefaultCommandTimeout = 300 * time.Second
	// DefaultMaxRetriesPerCommand bounds the error-repair loop for one command.
	DefaultMaxRetriesPerCommand = 3
	// DefaultMaxToolIterations bounds tool calls within a single turn, so a
	// model that loops cannot run indefinitely.
	DefaultMaxToolIterations = 50

	// DefaultMaxAgents bounds scheduler concurrency (§10).
	DefaultMaxAgents = 5

	// DefaultWebListen keeps the WebUI on loopback until the user opts out.
	DefaultWebListen = "127.0.0.1"
	// DefaultWebPort is the documented WebUI port.
	DefaultWebPort = 8585

	// DefaultLogLevel is the level used when none is configured.
	DefaultLogLevel = "info"
)

// Default base URLs for the local providers. These are the vendors' documented
// defaults; users running a server on another port override them in config.
const (
	DefaultLemonadeBaseURL = "http://127.0.0.1:13305"
	DefaultLMStudioBaseURL = "http://127.0.0.1:1234"
	DefaultOllamaBaseURL   = "http://127.0.0.1:11434"
	DefaultOpenAIBaseURL   = "https://api.openai.com/v1"
)

// Default returns the built-in configuration described in PROJECT.md §56.
//
// Every field is populated, so a config file only ever needs to contain the
// values a user actually wants to change. The returned value is freshly
// allocated, including its maps, and is safe for the caller to mutate.
func Default() *Config {
	c := &Config{
		Version:  Version,
		Provider: DefaultProvider,
		Model:    "",
		Execution: ExecutionConfig{
			Mode:                 permissions.ModeConfirm,
			CommandTimeout:       Duration(DefaultCommandTimeout),
			MaxRetriesPerCommand: DefaultMaxRetriesPerCommand,
			MaxToolIterations:    DefaultMaxToolIterations,
		},
		Agents: AgentsConfig{
			Enabled: true,
			Max:     DefaultMaxAgents,
		},
		Web: WebConfig{
			Enabled: false,
			Listen:  DefaultWebListen,
			Port:    DefaultWebPort,
			Auth:    AuthConfig{Enabled: false},
		},
		Network:   DefaultNetwork(),
		Providers: DefaultProviders(),
		Logging: LoggingConfig{
			Level:      DefaultLogLevel,
			Format:     DefaultLogFormat,
			MaxSizeMB:  DefaultLogMaxSizeMB,
			MaxBackups: DefaultLogMaxBackups,
		},
	}
	c.Permissions = DefaultPermissions()
	return c
}

// Network defaults. Outbound web access is off until the user turns it on.
const (
	// DefaultNetworkTimeout bounds a single outbound request.
	DefaultNetworkTimeout = 30 * time.Second
	// DefaultMaxResponseBytes caps a fetched body at 5 MiB so a hostile or
	// endless response cannot exhaust memory.
	DefaultMaxResponseBytes int64 = 5 << 20
	// DefaultMaxRedirects bounds a redirect chain.
	DefaultMaxRedirects = 5
	// DefaultSearchProvider is the only implemented search backend.
	DefaultSearchProvider = "duckduckgo"
	// DefaultSearchMaxResults matches what the DuckDuckGo lite endpoint
	// returns for a single page.
	DefaultSearchMaxResults = 10
	// DefaultSafeSearch is DuckDuckGo's own default.
	DefaultSafeSearch = "moderate"
	// DefaultSearchRegion is DuckDuckGo's no-region code.
	DefaultSearchRegion = "wt-wt"
)

// Logging defaults.
const (
	// DefaultLogFormat is text: the log is read by a person far more often
	// than it is shipped to a parser.
	DefaultLogFormat = "text"
	// DefaultLogMaxSizeMB is the rotation threshold.
	DefaultLogMaxSizeMB = 10
	// DefaultLogMaxBackups is how many rotated files are retained.
	DefaultLogMaxBackups = 5
)

// DefaultNetwork returns the outbound web access defaults.
//
// Enabled is false: reaching third-party sites sends the user's text to servers
// they did not choose, so it is opt-in rather than something Boop starts doing
// on its own. Private-network access is likewise off, because a URL suggested
// by a model must not be able to reach the loopback interface or a cloud
// metadata endpoint.
func DefaultNetwork() NetworkConfig {
	return NetworkConfig{
		Enabled:              false,
		UserAgent:            "",
		Timeout:              Duration(DefaultNetworkTimeout),
		MaxResponseBytes:     DefaultMaxResponseBytes,
		MaxRedirects:         DefaultMaxRedirects,
		AllowPrivateNetworks: false,
		RespectRobots:        true,
		Search: SearchConfig{
			Provider:   DefaultSearchProvider,
			MaxResults: DefaultSearchMaxResults,
			SafeSearch: DefaultSafeSearch,
			Region:     DefaultSearchRegion,
		},
	}
}

// DefaultProviders returns the provider table from §56.
//
// The three local providers are configured with their default base URLs and no
// credentials. The cloud providers name an environment variable instead of
// carrying a key (§45); Anthropic and xAI omit a base URL because their
// adapters know their own endpoint.
func DefaultProviders() map[string]ProviderConfig {
	return map[string]ProviderConfig{
		"lemonade": {
			Type:    "lemonade",
			BaseURL: DefaultLemonadeBaseURL,
		},
		"lmstudio": {
			Type:    "lmstudio",
			BaseURL: DefaultLMStudioBaseURL,
		},
		"ollama": {
			Type:    "ollama",
			BaseURL: DefaultOllamaBaseURL,
		},
		"openai": {
			Type:      "openai",
			BaseURL:   DefaultOpenAIBaseURL,
			APIKeyEnv: "OPENAI_API_KEY",
		},
		"anthropic": {
			Type:      "anthropic",
			APIKeyEnv: "ANTHROPIC_API_KEY",
		},
		"xai": {
			Type:      "xai",
			APIKeyEnv: "XAI_API_KEY",
		},
	}
}

// DefaultPermissions returns the permission table from §14.
//
// Reads are allowed because they are recoverable and constant; everything that
// mutates the machine, the repository, the network or production requires
// confirmation. Nothing is denied by default: denial is a user decision, and a
// silently denied category is harder to debug than a prompt.
func DefaultPermissions() PermissionsConfig {
	var p PermissionsConfig
	p.Filesystem.Read = permissions.RuleAllow
	p.Filesystem.Write = permissions.RuleConfirm
	p.Shell.Execute = permissions.RuleConfirm
	p.Git.Read = permissions.RuleAllow
	p.Git.Commit = permissions.RuleConfirm
	p.Git.Push = permissions.RuleConfirm
	p.Network.HTTP = permissions.RuleConfirm
	// Fetching a URL confirms by default. Searching only discloses the query
	// text to the configured engine and returns no local side effect, so it
	// is allowed once the user has already opted into outbound access at all
	// via network.enabled.
	p.Network.Fetch = permissions.RuleConfirm
	p.Network.Search = permissions.RuleAllow
	p.Production.Change = permissions.RuleConfirm
	return p
}

// Policy projects the configured permission table onto the runtime policy used
// by the permission engine.
//
// It exists so the evaluator never has to know the YAML shape, and so the
// category names live in exactly one place: internal/permissions.
func (c *Config) Policy() permissions.Policy {
	return permissions.Policy{
		Mode:         c.Execution.Mode,
		Rules:        c.Permissions.Rules(),
		Unrestricted: c.Execution.Unrestricted,
	}
}

// Rules flattens the authored category table into the map form the permission
// evaluator consumes.
func (p PermissionsConfig) Rules() map[permissions.Category]permissions.Rule {
	return map[permissions.Category]permissions.Rule{
		permissions.CatFilesystemRead:   p.Filesystem.Read,
		permissions.CatFilesystemWrite:  p.Filesystem.Write,
		permissions.CatShellExecute:     p.Shell.Execute,
		permissions.CatGitRead:          p.Git.Read,
		permissions.CatGitCommit:        p.Git.Commit,
		permissions.CatGitPush:          p.Git.Push,
		permissions.CatNetworkHTTP:      p.Network.HTTP,
		permissions.CatNetworkFetch:     p.Network.Fetch,
		permissions.CatNetworkSearch:    p.Network.Search,
		permissions.CatProductionChange: p.Production.Change,
	}
}
