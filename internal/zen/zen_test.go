package zen

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"

	"freezen/internal/config"
	"freezen/internal/logging"
)

// roundTripperFunc answers a request without a network, so a test can read
// exactly what the server sends to Zen.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestBuildModelsKeepsOnlyFreeModels(t *testing.T) {
	models := buildModels([]string{"big-pickle", "cat-jam-free", "claude-sonnet-4-5", "old-model-free"},
		bootstrapProvider, bootstrapProtocols)
	if _, ok := models["big-pickle"]; !ok {
		t.Fatal("a zero-cost model Zen lists as free was left out")
	}
	if _, ok := models["claude-sonnet-4-5"]; ok {
		t.Fatal("a model that isn't free was offered")
	}
	free, ok := models["cat-jam-free"]
	if !ok {
		t.Fatal("a free model was left out")
	}
	if free.Npm != SDK["chat"] || free.Limit["context"] != 262144 {
		t.Fatalf("a free model is %+v", free)
	}
}

func TestBuildModelsAsksAnUndocumentedFreeModelAsChat(t *testing.T) {
	models := buildModels([]string{"deepseek-v4.1-flash-free"}, bootstrapProvider, bootstrapProtocols)
	entry, ok := models["deepseek-v4.1-flash-free"]
	if !ok || entry.Npm != SDK["chat"] {
		t.Fatalf("a new free model is %+v", models)
	}
}

func TestBuildModelsListsOnlyLiveFreeModelsWithSupportedProtocols(t *testing.T) {
	paid := 1.0
	paidOut := 3.0
	provider := &catalogProvider{
		Npm: SDK["chat"],
		Models: map[string]catalogModel{
			"coder-free": {
				Name:          "Coder Free",
				Cost:          zero(),
				Limit:         map[string]any{"context": 200000, "input": 160000, "output": 32000},
				Reasoning:     new(true),
				ToolCall:      new(true),
				Modalities:    &catalogModalities{Input: []string{"text", "image"}, Output: []string{"text"}},
				ReasoningOpts: []catalogReasoningOption{{Type: "effort", Values: []string{"low", "high"}}},
			},
			"zero-cost":      {Cost: zero()},
			"paid":           {Cost: &catalogCost{Input: &paid, Output: &paidOut}},
			"old-free":       {Status: "deprecated", Cost: zero()},
			"decision-free":  {Provider: &struct{ Npm string }{Npm: "@vendor/systemone"}},
			"responses-free": {Provider: &struct{ Npm string }{Npm: SDK["responses"]}},
			"messages-free":  {Provider: &struct{ Npm string }{Npm: SDK["anthropic"]}},
		},
	}
	ids := []string{"coder-free", "zero-cost", "paid", "old-free", "decision-free", "responses-free", "messages-free"}
	models := buildModels(ids, provider, map[string]string{})
	names := make([]string, 0, len(models))
	for id := range models {
		names = append(names, id)
	}
	sort.Strings(names)
	want := []string{"coder-free", "messages-free", "responses-free", "zero-cost"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("the free models are %v, want %v", names, want)
	}
	coder := models["coder-free"]
	if coder.Npm != SDK["chat"] || coder.Capabilities.Input["image"] != true ||
		coder.Capabilities.Toolcall != true || coder.Limit["context"] != 200000 {
		t.Fatalf("coder-free is %+v", coder)
	}
	if len(coder.Variants) != 2 {
		t.Fatalf("coder-free's efforts are %v", coder.Variants)
	}
	if models["responses-free"].Npm != SDK["responses"] || models["messages-free"].Npm != SDK["anthropic"] {
		t.Fatalf("protocols are %v and %v", models["responses-free"].Npm, models["messages-free"].Npm)
	}

	// The docs override the inherited SDK, and SystemOne stays out.
	protocols := parseProtocolDocs("| Coder | coder-free | `https://opencode.ai/zen/v1/messages` | sdk |\n" +
		"| Decision | decision-free | `https://opencode.ai/zen/v1/systemone` | - |\n")
	withDocs := buildModels(ids, provider, protocols)
	if withDocs["coder-free"].Npm != SDK["anthropic"] {
		t.Fatalf("the docs did not override coder-free: %v", withDocs["coder-free"].Npm)
	}
	if _, ok := withDocs["decision-free"]; ok {
		t.Fatal("a SystemOne model was offered")
	}
}

func TestBuildModelsTreatsAnUnknownFreeModelAsTheProviderDoes(t *testing.T) {
	ids := []string{"new-free", "unknown-paid", "freebie", "free-model"}
	plain := buildModels(ids, nil, map[string]string{})
	if _, ok := plain["new-free"]; !ok || plain["new-free"].Npm != SDK["chat"] {
		t.Fatalf("an unknown -free model was asked at %v", plain)
	}
	if len(plain) != 1 {
		t.Fatalf("models the docs never named were offered: %v", plain)
	}
	responses := buildModels([]string{"new-free"}, &catalogProvider{Npm: SDK["responses"]}, map[string]string{})
	if responses["new-free"].Npm != SDK["responses"] {
		t.Fatalf("an unknown -free model took the provider's package: %v", responses["new-free"].Npm)
	}
	systemone := buildModels([]string{"new-free"}, nil, map[string]string{"new-free": "systemone"})
	if len(systemone) != 0 {
		t.Fatalf("a SystemOne free model was offered: %v", systemone)
	}
}

func TestParseProtocolDocs(t *testing.T) {
	// Rows as OpenCode's console docs lay them out: the model id is the second
	// cell, the endpoint the third.
	docs := "| Coder | coder-free | `https://opencode.ai/zen/v1/messages` | sdk |\n" +
		"| Decision | decision-free | `https://opencode.ai/zen/v1/systemone` | - |\n"
	protocols := parseProtocolDocs(docs)
	if protocols["coder-free"] != "anthropic" || protocols["decision-free"] != "systemone" {
		t.Fatalf("the docs gave %v", protocols)
	}
}

func TestSendFallsBackToTheNextProxy(t *testing.T) {
	cfg := config.Default()
	cfg.Proxy = []string{"http://127.0.0.1:8080", "http://127.0.0.1:8081"}
	cfg.LogLevel = "error"
	instance, err := New(cfg, logging.New("error"))
	if err != nil {
		t.Fatal(err)
	}
	var direct, proxied int
	instance.SetProxyTransports(roundTripperFunc(func(*http.Request) (*http.Response, error) {
		proxied++
		return nil, errors.New("dial tcp: connection refused")
	}))
	instance.SetDirectTransport(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		direct++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok")), Request: request}, nil
	}))

	response, err := instance.Send(context.Background(), http.MethodGet, "https://example.test", http.Header{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if proxied != 2 || direct != 1 {
		t.Fatalf("proxies were used %d times, the direct client %d", proxied, direct)
	}

	// Both proxies are now cooling down, so the next request goes straight to
	// the direct client rather than trying either of them again.
	response, err = instance.Send(context.Background(), http.MethodGet, "https://example.test", http.Header{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if proxied != 2 || direct != 2 {
		t.Fatalf("a cooling proxy was retried: proxies %d, direct %d", proxied, direct)
	}
}

func TestSendRefusesWhenProxiesAreRequired(t *testing.T) {
	cfg := config.Default()
	cfg.Proxy = []string{"http://127.0.0.1:8080"}
	cfg.ProxyRequired = true
	instance, err := New(cfg, logging.New("error"))
	if err != nil {
		t.Fatal(err)
	}
	instance.SetProxyTransports(roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: connection refused")
	}))
	instance.SetDirectTransport(roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("a required pool fell back to a direct connection")
		return nil, nil
	}))
	if _, err := instance.Send(context.Background(), http.MethodGet, "https://example.test", http.Header{}, nil); err == nil {
		t.Fatal("a required pool without a usable proxy did not fail")
	}
}

func TestUpstreamHeadersUseTheConfiguredAPIKey(t *testing.T) {
	given := http.Header{}
	chat := UpstreamHeaders(given, "chat", "ses_1", "project", "secret-key")
	if got := chat.Get("authorization"); got != "Bearer secret-key" {
		t.Fatalf("the authorization is %q", got)
	}
	anthropic := UpstreamHeaders(given, "anthropic", "ses_1", "project", "secret-key")
	if got := anthropic.Get("x-api-key"); got != "secret-key" {
		t.Fatalf("the x-api-key is %q", got)
	}
	if anthropic.Get("authorization") != "" {
		t.Fatal("an anthropic request carried a bearer token")
	}
}
