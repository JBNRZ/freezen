package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaultsWhenThereIsNoFile(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != DefaultAddr || cfg.APIKey != DefaultAPIKey ||
		cfg.ProxyCooldown.Duration() != DefaultProxyCooldown || cfg.LogLevel != DefaultLogLevel {
		t.Fatalf("the defaults are %+v", cfg)
	}
	if cfg.Proxy != nil || cfg.ProxyRequired {
		t.Fatalf("the defaults name a proxy: %+v", cfg)
	}
	// A missing file is not an error either.
	missing, err := Load(filepath.Join(t.TempDir(), "nothing.yaml"))
	if err != nil || missing.APIKey != DefaultAPIKey {
		t.Fatalf("a missing config file gave %+v, %v", missing, err)
	}
}

func TestLoadReadsTheYAMLFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := "" +
		"addr: \"127.0.0.1:9000\"\n" +
		"api_key: \"secret-key\"\n" +
		"proxy:\n" +
		"  - \"http://127.0.0.1:8080\"\n" +
		"  - \"socks5://user:pass@127.0.0.1:1080\"\n" +
		"proxy_cooldown: 90s\n" +
		"proxy_required: true\n" +
		"log_level: debug\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "127.0.0.1:9000" || cfg.APIKey != "secret-key" {
		t.Fatalf("the config is %+v", cfg)
	}
	if len(cfg.Proxy) != 2 || cfg.Proxy[0] != "http://127.0.0.1:8080" ||
		cfg.Proxy[1] != "socks5://user:pass@127.0.0.1:1080" {
		t.Fatalf("the proxies are %v", cfg.Proxy)
	}
	if cfg.ProxyCooldown.Duration() != 90*time.Second || !cfg.ProxyRequired || cfg.LogLevel != "debug" {
		t.Fatalf("the config is %+v", cfg)
	}
}

func TestLoadTakesOneProxyWrittenAsAString(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("proxy: http://127.0.0.1:8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Proxy) != 1 || cfg.Proxy[0] != "http://127.0.0.1:8080" {
		t.Fatalf("the proxy is %v", cfg.Proxy)
	}
	if cfg.Addr != DefaultAddr || cfg.APIKey != DefaultAPIKey {
		t.Fatalf("the untouched fields are %+v", cfg)
	}
}

func TestTheExampleConfigLoads(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != DefaultAddr || cfg.APIKey != DefaultAPIKey ||
		cfg.ProxyCooldown.Duration() != DefaultProxyCooldown || cfg.LogLevel != DefaultLogLevel {
		t.Fatalf("the example config is %+v", cfg)
	}
	if len(cfg.Proxy) != 0 {
		t.Fatalf("the example config names a proxy: %v", cfg.Proxy)
	}
}
