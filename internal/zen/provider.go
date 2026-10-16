package zen

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"maps"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"freezen/internal/config"
	"freezen/internal/logging"
	"freezen/internal/proxy"
)

// The Zen free provider: which models it serves, what it is asked with, and
// how a reply is handed back (provider.mjs of the plugin).

type Provider struct {
	apiKey string
	proxy  *proxy.Proxy
	log    *logging.Logger

	mu        sync.Mutex
	known     map[string]*Model
	metadata  *catalogProvider
	protocols map[string]string
	listed    bool
	listedAt  time.Time
}

func New(cfg *config.Config, log *logging.Logger) (*Provider, error) {
	upstream, err := proxy.New(string(cfg.Proxy))
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(bootstrapProvider.Models))
	for id := range bootstrapProvider.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return &Provider{
		apiKey:    cfg.APIKey,
		proxy:     upstream,
		log:       log,
		metadata:  bootstrapProvider,
		protocols: copyProtocols(bootstrapProtocols),
		known:     buildModels(ids, bootstrapProvider, bootstrapProtocols),
	}, nil
}

// APIKey is what every upstream request is signed with.
func (p *Provider) APIKey() string { return p.apiKey }

// ProxyName names the proxy every request goes through, "direct" for none.
func (p *Provider) ProxyName() string { return p.proxy.Describe() }

// SetTransport points both clients, the proxy's and the direct one, at rt (for
// tests).
func (p *Provider) SetTransport(rt http.RoundTripper) {
	p.proxy.SetThrough(rt)
	p.proxy.SetDirect(rt)
}

// SetProxyTransport points only the proxy client at rt (for tests).
func (p *Provider) SetProxyTransport(rt http.RoundTripper) { p.proxy.SetThrough(rt) }

// SetDirectTransport points only the direct client at rt (for tests).
func (p *Provider) SetDirectTransport(rt http.RoundTripper) { p.proxy.SetDirect(rt) }

// Send makes the request through the configured proxy, or over a direct
// connection when there is none or the proxy fails to carry it.
func (p *Provider) Send(ctx context.Context, method, url string, headers http.Header, body []byte) (*http.Response, error) {
	var proxyErr error
	if client := p.proxy.Through(); client != nil {
		p.log.Debugf("request %s %s through a proxy", method, url)
		response, err := p.attempt(ctx, client, method, url, headers, body)
		if err == nil {
			return response, nil
		}
		if ctx.Err() != nil {
			return nil, err
		}
		p.log.Warnf("proxy %s failed (%v); falling back to a direct connection", p.proxy.Describe(), err)
		proxyErr = err
	}
	p.log.Debugf("request %s %s direct", method, url)
	response, err := p.attempt(ctx, p.proxy.Direct(), method, url, headers, body)
	if err == nil {
		return response, nil
	}
	if proxyErr != nil {
		return nil, proxyErr
	}
	return nil, err
}

func (p *Provider) attempt(ctx context.Context, client *http.Client, method, url string, headers http.Header, body []byte) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	request.Header = headers.Clone()
	return client.Do(request)
}

func copyProtocols(source map[string]string) map[string]string {
	out := make(map[string]string, len(source))
	maps.Copy(out, source)
	return out
}

// ---------------------------------------------------------------- discovery

type zenListing struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

func (p *Provider) readRemote(ctx context.Context, url string, headers http.Header) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	response, err := p.Send(ctx, http.MethodGet, url, headers, nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("Model discovery returned HTTP %d", response.StatusCode)
	}
	return io.ReadAll(io.LimitReader(response.Body, 32<<20))
}

// Discover asks Zen which models it serves, the catalog what each of them is,
// and the docs what each is asked at. When Zen's own list can't be read the
// last list is kept.
func (p *Provider) Discover(ctx context.Context) {
	type result struct {
		body []byte
		err  error
	}
	listingCh := make(chan result, 1)
	catalogCh := make(chan result, 1)
	docsCh := make(chan result, 1)

	listingHeaders := http.Header{}
	listingHeaders.Set("authorization", "Bearer "+p.apiKey)
	listingHeaders.Set("x-opencode-client", "cli")
	go func() {
		body, err := p.readRemote(ctx, Base+"/models", listingHeaders)
		listingCh <- result{body, err}
	}()
	go func() {
		body, err := p.readRemote(ctx, catalogURL, nil)
		catalogCh <- result{body, err}
	}()
	go func() {
		body, err := p.readRemote(ctx, docsURL, nil)
		docsCh <- result{body, err}
	}()

	listing, catalog, docs := <-listingCh, <-catalogCh, <-docsCh

	p.mu.Lock()
	defer p.mu.Unlock()

	partial := false
	if catalog.err == nil {
		var decoded modelsDevCatalog
		if err := json.Unmarshal(catalog.body, &decoded); err == nil && decoded.OpenCode != nil &&
			decoded.OpenCode.Models != nil {
			p.metadata = decoded.OpenCode
		} else {
			partial = true
		}
	} else {
		partial = true
	}
	if docs.err == nil {
		if found := parseProtocolDocs(string(docs.body)); len(found) > 0 {
			protocols := copyProtocols(bootstrapProtocols)
			maps.Copy(protocols, found)
			p.protocols = protocols
		} else {
			partial = true
		}
	} else {
		partial = true
	}

	if listing.err != nil {
		p.log.Warnf("model discovery: Zen listing failed (%v); keeping the last list", listing.err)
		return
	}
	var decoded zenListing
	if err := json.Unmarshal(listing.body, &decoded); err != nil {
		p.log.Warnf("model discovery: Zen listing was not valid JSON (%v); keeping the last list", err)
		return
	}
	if decoded.Data == nil {
		// Zen's own list wasn't a list: the last list read in full is kept,
		// as an unreadable listing is.
		p.log.Warnf("model discovery: Zen listing had no models; keeping the last list")
		return
	}
	ids := make([]string, 0, len(decoded.Data))
	for _, item := range decoded.Data {
		ids = append(ids, item.ID)
	}
	discovered := buildModels(ids, p.metadata, p.protocols)
	if partial {
		// Preserve previously verified models only while they remain in the
		// live catalog: what Zen lists is what is offered.
		for _, id := range ids {
			_, offered := discovered[id]
			if offered {
				continue
			}
			if _, described := p.metadata.Models[id]; described {
				continue
			}
			if _, documented := p.protocols[id]; documented {
				continue
			}
			if known, ok := p.known[id]; ok {
				discovered[id] = known
			}
		}
	}
	p.known = discovered
	p.listed = true
	p.listedAt = time.Now()
	p.log.Infof("model discovery: %d free model(s) from Zen (%s)", len(discovered), strings.Join(sortedIDs(discovered), ", "))
}

func sortedIDs(models map[string]*Model) []string {
	ids := make([]string, 0, len(models))
	for id := range models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// ListModels asks Zen what it serves free now and answers its full list. The
// map is replaced, never changed, so a caller may hold on to it.
func (p *Provider) ListModels(ctx context.Context) map[string]*Model {
	p.Discover(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.known
}

// Resolve answers the model a request names, asking Zen's list again for one
// the last list didn't have.
func (p *Provider) Resolve(ctx context.Context, name string) *Model {
	p.mu.Lock()
	found, ok := p.known[name]
	needsList := !ok && (!p.listed || time.Since(p.listedAt) >= relistAfter)
	p.mu.Unlock()
	if needsList {
		p.Discover(ctx)
		p.mu.Lock()
		found, ok = p.known[name]
		p.mu.Unlock()
	}
	if !ok {
		return nil
	}
	return found
}

// FreeModelNames answers the ids of every free model this server offers.
func (p *Provider) FreeModelNames() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	names := make([]string, 0, len(p.known))
	for id := range p.known {
		names = append(names, id)
	}
	sort.Strings(names)
	return names
}
