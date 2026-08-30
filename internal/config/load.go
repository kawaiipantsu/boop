package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// fileMode is the permission set for config.yaml.
//
// The file names the environment variables that hold cloud credentials and
// describes exactly what Boop is permitted to do on this machine; neither
// belongs to other users of a shared host.
const fileMode fs.FileMode = 0o600

// Load reads the user's configuration, layered over the built-in defaults.
//
// Keys present in the file win; absent keys keep their default, so a config
// file written by an older version of Boop keeps working when new settings are
// added. When no config file exists yet one is created containing the
// defaults, which gives the user something to edit and makes the effective
// configuration inspectable rather than implicit.
func Load() (*Config, error) {
	path, err := ConfigFile()
	if err != nil {
		return nil, err
	}
	cfg, err := LoadFrom(path)
	if err == nil {
		return cfg, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	cfg = Default()
	if err := EnsureDirs(); err != nil {
		return nil, err
	}
	if err := cfg.SaveTo(path); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadFrom reads a configuration file at an explicit path, layered over the
// built-in defaults.
//
// A missing file is reported as fs.ErrNotExist rather than silently defaulted,
// so a caller that named a path can tell the difference between "not
// configured" and "typo in --config".
func LoadFrom(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, nil
}

// parse decodes YAML over a fresh set of defaults.
func parse(data []byte) (*Config, error) {
	cfg := Default()
	// Snapshot the defaults: the decode below mutates cfg.Providers in place.
	defaults := maps.Clone(cfg.Providers)

	// Decoding into the default Config merges scalars and nested structs, and
	// adds to the providers map without dropping unmentioned entries.
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	// YAML map values are not addressable, so the decode above replaced each
	// mentioned provider entry wholesale and lost the defaults for the fields
	// the user did not restate. Re-decode those entries onto their defaults so
	// that, for example, overriding only openai.base_url keeps its type and
	// api_key_env.
	var overlay struct {
		Providers map[string]yaml.Node `yaml:"providers"`
	}
	if err := yaml.Unmarshal(data, &overlay); err != nil {
		return nil, err
	}
	for name, node := range overlay.Providers {
		if node.IsZero() || node.Tag == "!!null" {
			continue
		}
		entry := defaults[name]
		if err := node.Decode(&entry); err != nil {
			return nil, fmt.Errorf("providers.%s: %w", name, err)
		}
		cfg.Providers[name] = entry
	}
	if cfg.Providers == nil {
		cfg.Providers = map[string]ProviderConfig{}
	}
	return cfg, nil
}

// Save writes the configuration to the platform config file, creating the
// directory tree if needed.
func (c *Config) Save() error {
	path, err := ConfigFile()
	if err != nil {
		return err
	}
	if err := EnsureDirs(); err != nil {
		return err
	}
	return c.SaveTo(path)
}

// SaveTo writes the configuration to an explicit path.
//
// The write is atomic — a temporary file in the destination directory, synced
// and then renamed — so an interrupted or failed write can never leave a
// truncated config behind. The result is 0600; see fileMode.
func (c *Config) SaveTo(path string) error {
	data, err := c.Marshal()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".config-*.yaml.tmp")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup; a successful rename makes this a no-op.
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	// CreateTemp already uses 0600, but be explicit: the mode of this file is
	// a security property, not an accident of the stdlib.
	if err := tmp.Chmod(fileMode); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// Marshal renders the configuration as YAML.
//
// The output is plain, two-space-indented and comment-free: it is written by
// the /config editor as well as by hand, and round-tripping user comments is
// not something yaml.v3 can do for us.
func (c *Config) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	return buf.Bytes(), nil
}

// ErrMissingAPIKey reports that a provider's credential environment variable
// is unset or empty.
var ErrMissingAPIKey = errors.New("api key environment variable is not set")

// ResolveAPIKey reads the credential named by pc.APIKeyEnv from the
// environment.
//
// Keys are never stored in the config file (§45): the file names a variable
// and the value is read at use time. The returned value must not be logged,
// echoed to the WebUI, or written into a transcript or crash report — this
// function deliberately keeps it out of any error it constructs.
func ResolveAPIKey(pc ProviderConfig) (string, error) {
	name := strings.TrimSpace(pc.APIKeyEnv)
	if name == "" {
		return "", fmt.Errorf("provider has no api_key_env: %w", ErrMissingAPIKey)
	}
	value := os.Getenv(name)
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s is empty; export it before using this provider: %w", name, ErrMissingAPIKey)
	}
	return value, nil
}
