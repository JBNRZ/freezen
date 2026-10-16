package proxy

import (
	"testing"
)

func TestParseProxyURLRejectsAnUnknownScheme(t *testing.T) {
	if _, err := parseProxyURL("ftp://127.0.0.1:21"); err == nil {
		t.Fatal("an ftp proxy was accepted")
	}
}

func TestParseProxyURLSchemesAndBareHost(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:8080":      "http",
		"https://proxy.example:8443": "https",
		"socks5://127.0.0.1:1080":    "socks5",
		"socks5h://127.0.0.1:1080":   "socks5h",
		"127.0.0.1:7890":             "http",
		"user:pass@127.0.0.1:1080":   "http",
	}
	for spec, scheme := range cases {
		parsed, err := parseProxyURL(spec)
		if err != nil {
			t.Fatalf("proxy %q: %v", spec, err)
		}
		if parsed.Scheme != scheme {
			t.Fatalf("proxy %q has scheme %q, want %q", spec, parsed.Scheme, scheme)
		}
	}
}

func TestNewBuildsTheProxyAndHidesItsCredentials(t *testing.T) {
	configured, err := New("socks5://user:pass@127.0.0.1:1080")
	if err != nil {
		t.Fatal(err)
	}
	if configured.Through() == nil {
		t.Fatal("a configured proxy has no client to go through")
	}
	if configured.Describe() != "socks5://127.0.0.1:1080" {
		t.Fatalf("the proxy is described as %q", configured.Describe())
	}

	// Naming no proxy is not an error: every request is direct.
	none, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	if none.Through() != nil {
		t.Fatal("an empty proxy has a client to go through")
	}
	if none.Describe() != "direct" {
		t.Fatalf("an empty proxy is described as %q", none.Describe())
	}
	if none.Direct() == nil {
		t.Fatal("an empty proxy has no direct client")
	}

	if _, err := New("ftp://127.0.0.1:21"); err == nil {
		t.Fatal("an invalid proxy was accepted")
	}
}
