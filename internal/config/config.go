package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"go.yaml.in/yaml/v4"
)

// The server reads one YAML config file (the only command-line argument names
// it); everything else lives in the file, with the defaults below when a field
// or the file itself is absent.

const (
	DefaultAddr     = ":8080"
	DefaultAPIKey   = "public"
	DefaultLogLevel = "info"
)

type Config struct {
	// Addr is the address to listen on.
	Addr string `yaml:"addr"`
	// APIKey is what every upstream request is signed with. Zen's free tier
	// answers the public credential, so it defaults to "public".
	APIKey string `yaml:"api_key"`
	// Proxy is the one proxy a request may be sent through, empty for a
	// direct connection. It is an http://, https:// or socks5:// URL.
	Proxy ProxyURL `yaml:"proxy"`
	// LogLevel is one of debug, info, warn or error.
	LogLevel string `yaml:"log_level"`
}

// ProxyURL is the one proxy a request may be sent through, empty for a direct
// connection. Only one may be named: a list is refused.
type ProxyURL string

func (p *ProxyURL) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.SequenceNode {
		return errors.New("proxy must be one URL, not a list")
	}
	var value string
	if err := node.Decode(&value); err != nil {
		return err
	}
	*p = ProxyURL(strings.TrimSpace(value))
	return nil
}

func Default() *Config {
	return &Config{
		Addr:     DefaultAddr,
		APIKey:   DefaultAPIKey,
		LogLevel: DefaultLogLevel,
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
	if parsed.Proxy != "" {
		cfg.Proxy = parsed.Proxy
	}
	if strings.TrimSpace(parsed.LogLevel) != "" {
		cfg.LogLevel = strings.TrimSpace(parsed.LogLevel)
	}
	return cfg, nil
}
