package proxy

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"freezen/internal/logging"
)

// A pool of proxies: every upstream request picks one at random from those
// that are usable, and one that fails to carry a request is set aside for a
// cooldown before it can be picked again. An empty pool means a direct
// connection.

const defaultCooldown = time.Minute

type Entry struct {
	url    *url.URL
	client *http.Client
	until  time.Time // unusable until then
}

type Pool struct {
	mu       sync.Mutex
	entries  []*Entry
	direct   *http.Client
	cooldown time.Duration
	required bool
	log      *logging.Logger
}

// newTransport is the transport a client is built on: HTTP/2 attempted, a
// redirect refused (as the plugin's fetch asks for one), and the given proxy
// (nil for a direct connection).
func newTransport(proxyURL *url.URL) *http.Transport {
	transport := &http.Transport{
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 10 * time.Minute,
	}
	if proxyURL != nil {
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return transport
}

func newClient(transport *http.Transport) *http.Client {
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("Zen answered with a redirect")
		},
	}
}

func parseProxyURL(spec string) (*url.URL, error) {
	value := strings.TrimSpace(spec)
	if value == "" {
		return nil, errors.New("empty proxy URL")
	}
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q (use http, https or socks5)", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("proxy %q names no host", spec)
	}
	return parsed, nil
}

// redact hides any credentials a proxy URL carries, for a log line.
func redact(parsed *url.URL) string {
	if parsed == nil {
		return "direct"
	}
	clean := *parsed
	clean.User = nil
	return clean.String()
}

func New(specs []string, cooldown time.Duration, required bool, log *logging.Logger) (*Pool, error) {
	pool := &Pool{
		direct:   newClient(newTransport(nil)),
		cooldown: cooldown,
		required: required,
		log:      log,
	}
	if cooldown <= 0 {
		pool.cooldown = defaultCooldown
	}
	seen := map[string]bool{}
	for _, spec := range specs {
		parsed, err := parseProxyURL(spec)
		if err != nil {
			return nil, err
		}
		key := parsed.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		pool.entries = append(pool.entries, &Entry{
			url:    parsed,
			client: newClient(newTransport(parsed)),
		})
	}
	return pool, nil
}

func (p *Pool) Count() int {
	if p == nil {
		return 0
	}
	return len(p.entries)
}

// Required reports whether a request must go through a proxy rather than
// falling back to a direct connection.
func (p *Pool) Required() bool {
	if p == nil {
		return false
	}
	return p.required
}

// Pick answers a proxy at random from those not cooling down and not already
// tried by this request, or nil when none is left (a direct connection).
func (p *Pool) Pick(exclude map[*Entry]bool) *Entry {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	available := make([]*Entry, 0, len(p.entries))
	for _, entry := range p.entries {
		if exclude[entry] {
			continue
		}
		if now.Before(entry.until) {
			continue
		}
		available = append(available, entry)
	}
	if len(available) == 0 {
		return nil
	}
	return available[rand.IntN(len(available))]
}

// Fail sets a proxy aside for the cooldown, so a request picks another one
// until it is offered again.
func (p *Pool) Fail(entry *Entry, err error) {
	if p == nil || entry == nil {
		return
	}
	p.mu.Lock()
	entry.until = time.Now().Add(p.cooldown)
	p.mu.Unlock()
	p.log.Warnf("proxy %s failed (%v); out of the pool for %s", redact(entry.url), err, p.cooldown)
}

func (p *Pool) ClientFor(entry *Entry) *http.Client {
	if entry == nil {
		return p.direct
	}
	return entry.client
}

// SetTransport points every client the pool may use (each proxy and the direct
// one) at rt. It exists so a test can answer requests without a network.
func (p *Pool) SetTransport(rt http.RoundTripper) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.direct.Transport = rt
	for _, entry := range p.entries {
		entry.client.Transport = rt
	}
}

// SetDirectTransport points only the direct client at rt (for tests).
func (p *Pool) SetDirectTransport(rt http.RoundTripper) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.direct.Transport = rt
}

// SetProxyTransports points every proxy client at rt (for tests).
func (p *Pool) SetProxyTransports(rt http.RoundTripper) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, entry := range p.entries {
		entry.client.Transport = rt
	}
}
