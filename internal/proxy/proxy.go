package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The proxy a request may be sent through. With one configured every request
// goes through it, and a request it fails to carry is made again over a direct
// connection. Naming no proxy means every request is direct.

// Proxy is the one proxy a config names, and the client a request uses when
// there is none.
type Proxy struct {
	name    string       // the URL without its credentials, "" when none is named
	through *http.Client // the client that goes through it, nil when none is named
	direct  *http.Client
}

// New builds the proxy from the one URL a config may name, empty for none.
func New(spec string) (*Proxy, error) {
	proxy := &Proxy{direct: newClient(newTransport(nil))}
	if strings.TrimSpace(spec) == "" {
		return proxy, nil
	}
	parsed, err := parseProxyURL(spec)
	if err != nil {
		return nil, err
	}
	clean := *parsed
	clean.User = nil
	proxy.name = clean.String()
	proxy.through = newClient(newTransport(parsed))
	return proxy, nil
}

// Through is the client a request goes through, or nil when the config names no
// proxy, in which case the request is direct.
func (p *Proxy) Through() *http.Client { return p.through }

// Direct is the client for a request that doesn't go through the proxy.
func (p *Proxy) Direct() *http.Client { return p.direct }

// Describe names the proxy for a log line, without the credentials it carries,
// or "direct" when none is configured.
func (p *Proxy) Describe() string {
	if p.name == "" {
		return "direct"
	}
	return p.name
}

// SetThrough points the client a request goes through at rt (for tests).
func (p *Proxy) SetThrough(rt http.RoundTripper) {
	if p.through != nil {
		p.through.Transport = rt
	}
}

// SetDirect points the direct client at rt (for tests).
func (p *Proxy) SetDirect(rt http.RoundTripper) { p.direct.Transport = rt }

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
			return errors.New("the reply from Zen was a redirect")
		},
	}
}

func parseProxyURL(spec string) (*url.URL, error) {
	value := strings.TrimSpace(spec)
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
