package app

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/kawaiipantsu/boop/internal/config"
	"github.com/kawaiipantsu/boop/internal/logging"
	"github.com/kawaiipantsu/boop/internal/provider"
	"github.com/kawaiipantsu/boop/internal/provider/anthropic"
	"github.com/kawaiipantsu/boop/internal/provider/lemonade"
	"github.com/kawaiipantsu/boop/internal/provider/lmstudio"
	"github.com/kawaiipantsu/boop/internal/provider/ollama"
	"github.com/kawaiipantsu/boop/internal/provider/openai"
	"github.com/kawaiipantsu/boop/internal/provider/openaicompat"
	"github.com/kawaiipantsu/boop/internal/provider/xai"
	"github.com/kawaiipantsu/boop/internal/webclient"
)

// BuildProvider constructs the adapter named by a config entry.
//
// This is the only place in Boop that maps a provider type string onto a
// concrete adapter. Everything downstream sees provider.Provider, which is what
// keeps the rest of the codebase vendor-neutral (§2.2).
func BuildProvider(name string, pc config.ProviderConfig, httpClient *http.Client) (provider.Provider, error) {
	timeout := pc.Timeout.Std()

	apiKey, err := config.ResolveAPIKey(pc)
	if err != nil && requiresKey(pc.Type) {
		return nil, fmt.Errorf("provider %q: %w", name, err)
	}
	// Tell the log redactor the exact credential value. Shape-based detection
	// catches sk- and similar, but a self-hosted gateway key can be an
	// ordinary-looking word that no pattern would ever match (§45).
	if apiKey != "" {
		logging.RegisterSecret(apiKey)
	}

	switch strings.ToLower(strings.TrimSpace(pc.Type)) {
	case "ollama":
		return ollama.New(pc.BaseURL, httpClient, ollama.WithName(name), ollama.WithTimeout(timeout)), nil
	case "lmstudio":
		return lmstudio.New(pc.BaseURL, httpClient, lmstudio.WithName(name), lmstudio.WithTimeout(timeout)), nil
	case "lemonade":
		return lemonade.New(pc.BaseURL, httpClient, lemonade.WithName(name), lemonade.WithTimeout(timeout)), nil
	case "openai":
		return openai.New(openai.Options{
			Name: name, BaseURL: pc.BaseURL, APIKey: apiKey,
			Headers: pc.Headers, Timeout: timeout, HTTPClient: httpClient,
		}), nil
	case "anthropic":
		return anthropic.New(anthropic.Options{
			Name: name, BaseURL: pc.BaseURL, APIKey: apiKey,
			Headers: pc.Headers, Timeout: timeout, HTTPClient: httpClient,
		}), nil
	case "xai":
		return xai.New(xai.Options{
			Name: name, BaseURL: pc.BaseURL, APIKey: apiKey,
			Headers: pc.Headers, Timeout: timeout, HTTPClient: httpClient,
		}), nil
	case "openai-compatible", "openaicompat", "generic", "":
		if pc.BaseURL == "" {
			return nil, fmt.Errorf("provider %q: an OpenAI-compatible provider needs a base_url", name)
		}
		return openaicompat.New(openaicompat.Options{
			Name: name, BaseURL: pc.BaseURL, APIKey: apiKey,
			Headers: pc.Headers, Timeout: timeout, HTTPClient: httpClient,
		}), nil
	default:
		return nil, fmt.Errorf("provider %q: unknown type %q (want ollama, lmstudio, lemonade, openai, anthropic, xai or openai-compatible)", name, pc.Type)
	}
}

// requiresKey reports whether a provider type cannot work without a credential.
//
// Local servers need none, which is the point of local-first: a missing
// OPENAI_API_KEY must not stop Boop starting when the user runs Ollama.
func requiresKey(providerType string) bool {
	switch strings.ToLower(strings.TrimSpace(providerType)) {
	case "openai", "anthropic", "xai":
		return true
	default:
		return false
	}
}

// BuildRouter assembles every usable provider in cfg into a router.
//
// A provider that cannot be constructed is skipped rather than fatal, and the
// reason is returned in warnings. A missing cloud credential must not stop a
// local-only user from starting; they would be told to fix something they do
// not use.
func BuildRouter(cfg *config.Config, httpClient *http.Client) (*provider.Router, []string, error) {
	registry := provider.NewRegistry()
	var warnings []string

	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		pc := cfg.Providers[name]
		if pc.Disabled {
			continue
		}
		p, err := BuildProvider(name, pc, httpClient)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("provider %q unavailable: %v", name, err))
			continue
		}
		if err := registry.Register(p); err != nil {
			warnings = append(warnings, fmt.Sprintf("provider %q not registered: %v", name, err))
		}
	}

	if registry.Len() == 0 {
		return nil, warnings, fmt.Errorf("no usable providers configured; check the providers section of your config")
	}
	if _, ok := registry.Get(cfg.Provider); !ok {
		return nil, warnings, fmt.Errorf("active provider %q is not available (configured: %s)",
			cfg.Provider, strings.Join(registry.Names(), ", "))
	}

	rc := provider.RouterConfig{
		Classes:  map[provider.RouteClass]provider.Target{},
		Fallback: cfg.Fallback,
	}
	rc.Classes[provider.ClassDefault] = provider.Target{Provider: cfg.Provider, Model: cfg.Model}
	for class, target := range cfg.Routing {
		rc.Classes[provider.RouteClass(class)] = provider.Target{Provider: target.Provider, Model: target.Model}
	}
	return provider.NewRouter(registry, rc), warnings, nil
}

// defaultHTTPClient is the shared client for provider traffic.
//
// No Timeout is set: it would cut off a long generation mid-stream. Adapters
// bound individual requests with a context deadline instead.
func defaultHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:          32,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		},
	}
}

// newWebClient builds the outbound web client from configuration.
func newWebClient(cfg *config.Config) (*webclient.Client, error) {
	return webclient.New(cfg.Network)
}
