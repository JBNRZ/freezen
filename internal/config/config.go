package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go.yaml.in/yaml/v4"
)

// The server reads one YAML config file (the only command-line argument names
// it); everything else lives in the file, with the defaults below when a field
// or the file itself is absent.

const (
	DefaultAddr          = ":8080"
	DefaultAPIKey        = "public"
	DefaultProxyCooldown = time.Minute
	DefaultLogLevel      = "info"
)

type Config struct {
	// Addr is the address to listen on.
	Addr string `yaml:"addr"`
	// APIKey is what every upstream request is signed with. Zen's free tier
	// answers the public credential, so it defaults to "public".
	APIKey string `yaml:"api_key"`
	// Proxy is the pool of proxies a request may be sent through, empty for
	// none. Each entry is an http://, https:// or socks5:// URL.
	Proxy ProxyList `yaml:"proxy"`
	// ProxyCooldown is how long a proxy that failed is left out of the pool.
	ProxyCooldown Duration `yaml:"proxy_cooldown"`
	// ProxyRequired refuses a request when no proxy is usable rather than
	// falling back to a direct connection.
	ProxyRequired bool `yaml:"proxy_required"`
	// LogLevel is one of debug, info, warn or error.
	LogLevel string `yaml:"log_level"`
}

// ProxyList is a proxy pool written either as one string or as a list.
type ProxyList []string

func (p *ProxyList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var value string
		if err := node.Decode(&value); err != nil {
			return err
		}
		if strings.TrimSpace(value) == "" {
			*p = nil
			return nil
		}
		*p = []string{value}
		return nil
	case yaml.SequenceNode:
		var values []string
		if err := node.Decode(&values); err != nil {
			return err
		}
		*p = values
		return nil
	default:
		return errors.New("proxy must be a string or a list of strings")
	}
}

// Duration is a time.Duration written as a string ("90s", "5m").
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var value string
	if err := node.Decode(&value); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", value, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) Duration() time.Duration { return time.Duration(d) }

func Default() *Config {
	return &Config{
		Addr:          DefaultAddr,
		APIKey:        DefaultAPIKey,
		ProxyCooldown: Duration(DefaultProxyCooldown),
		LogLevel:      DefaultLogLevel,
	}
}

// Load reads the config file, keeping a default for every field it doesn't
// set. A file that isn't there is not an error: the defaults are.
func Load(path string) (*Config, error) {
	cfg := Default()
	if strings.TrimSpace(path) == "" {
		return cfg, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return nil, err
	}
	var parsed Config
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if strings.TrimSpace(parsed.Addr) != "" {
		cfg.Addr = strings.TrimSpace(parsed.Addr)
	}
	if strings.TrimSpace(parsed.APIKey) != "" {
		cfg.APIKey = parsed.APIKey
	}
	if parsed.Proxy != nil {
		cfg.Proxy = parsed.Proxy
	}
	if parsed.ProxyCooldown != 0 {
		cfg.ProxyCooldown = parsed.ProxyCooldown
	}
	if strings.TrimSpace(parsed.LogLevel) != "" {
		cfg.LogLevel = strings.TrimSpace(parsed.LogLevel)
	}
	cfg.ProxyRequired = parsed.ProxyRequired
	return cfg, nil
}
