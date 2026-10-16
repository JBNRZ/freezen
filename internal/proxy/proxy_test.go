package proxy

import (
	"errors"
	"testing"
	"time"

	"freezen/internal/logging"
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

func TestNewDropsDuplicatesAndValidatesEveryProxy(t *testing.T) {
	pool, err := New(
		[]string{"http://127.0.0.1:8080", "http://127.0.0.1:8080", "socks5://127.0.0.1:1080"},
		time.Minute, false, logging.New("error"))
	if err != nil {
		t.Fatal(err)
	}
	if pool.Count() != 2 {
		t.Fatalf("the pool holds %d proxies", pool.Count())
	}
	if _, err := New([]string{"ftp://127.0.0.1:21"}, time.Minute, false, logging.New("error")); err == nil {
		t.Fatal("a pool with an invalid proxy was accepted")
	}
}

func TestPoolPicksRandomlyAndCoolsDownAFailedProxy(t *testing.T) {
	pool, err := New(
		[]string{"http://127.0.0.1:8080", "http://127.0.0.1:8081"},
		50*time.Millisecond, false, logging.New("error"))
	if err != nil {
		t.Fatal(err)
	}

	// With nothing excluded, both proxies can be picked.
	seen := map[*Entry]bool{}
	for i := 0; i < 200; i++ {
		entry := pool.Pick(map[*Entry]bool{})
		if entry == nil {
			t.Fatal("a proxy was not offered")
		}
		seen[entry] = true
	}
	if len(seen) != 2 {
		t.Fatalf("only %d proxy/ies were ever picked", len(seen))
	}

	// A failed proxy is set aside, so only the other one is offered.
	failed := pool.entries[0]
	pool.Fail(failed, errors.New("dial tcp: connection refused"))
	for i := 0; i < 20; i++ {
		entry := pool.Pick(map[*Entry]bool{})
		if entry == failed {
			t.Fatal("a proxy that failed was picked again")
		}
	}
	// Excluding the second leaves nothing, so a request falls back to direct.
	if entry := pool.Pick(map[*Entry]bool{pool.entries[1]: true}); entry != nil {
		t.Fatalf("a cooldown proxy was offered: %v", entry)
	}
	// After the cooldown it is offered again.
	time.Sleep(60 * time.Millisecond)
	if entry := pool.Pick(map[*Entry]bool{pool.entries[1]: true}); entry != failed {
		t.Fatalf("the proxy did not come back: %v", entry)
	}
}
