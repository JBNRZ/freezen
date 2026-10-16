package zen

import (
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Which free models Zen serves, what each is asked at, and how much of it
// the agent is told about (models.go of the plugin).

const ProviderID = "opencode-zen-free"

// SDK is the ai-sdk package each protocol's models are asked with.
var SDK = map[string]string{
	"chat":      "@ai-sdk/openai-compatible",
	"responses": "@ai-sdk/openai",
	"anthropic": "@ai-sdk/anthropic",
}

func sdkProtocol(npm string) string {
	switch {
	case strings.Contains(npm, "anthropic"):
		return "anthropic"
	case npm == SDK["responses"]:
		return "responses"
	case strings.Contains(npm, "openai-compatible"):
		return "chat"
	}
	return ""
}

// ------------------------------------------------------------- models.dev catalog

type catalogCost struct {
	Input  *float64 `json:"input"`
	Output *float64 `json:"output"`
}

type catalogModalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

type catalogReasoningOption struct {
	Type   string   `json:"type"`
	Values []string `json:"values"`
}

type catalogModel struct {
	Name           string                   `json:"name"`
	Cost           *catalogCost             `json:"cost"`
	Limit          map[string]any           `json:"limit"`
	Reasoning      *bool                    `json:"reasoning"`
	ToolCall       *bool                    `json:"tool_call"`
	Attachment     *bool                    `json:"attachment"`
	Modalities     *catalogModalities       `json:"modalities"`
	ReasoningOpts  []catalogReasoningOption `json:"reasoning_options"`
	Provider       *struct{ Npm string }    `json:"provider"`
	Status         string                   `json:"status"`
	Lifecycle      string                   `json:"lifecycle"`
	Deprecated     bool                     `json:"deprecated"`
	DeprecatedAt   any                      `json:"deprecated_at"`
	RetirementDate any                      `json:"retirement_date"`
	ReleaseDate    string                   `json:"release_date"`
}

type catalogProvider struct {
	Npm    string                  `json:"npm"`
	Models map[string]catalogModel `json:"models"`
}

type modelsDevCatalog struct {
	OpenCode *catalogProvider `json:"opencode"`
}

// ------------------------------------------------------------------ our models

// Model is one free model as this server offers it: what it is asked at, how
// much of it the agent is told about, and what it costs (nothing).
type Model struct {
	ID           string
	Name         string
	Npm          string
	Limit        map[string]any
	Capabilities modelCapabilities
	Variants     map[string]any
	ReleaseDate  string
}

type modelCapabilities struct {
	Reasoning  bool
	Attachment bool
	Toolcall   bool
	Input      map[string]bool
	Output     map[string]bool
}

// ListView is how a free model is offered on GET /v1/models.
func (m *Model) ListView() map[string]any {
	var input, output []string
	for _, name := range modalityNames {
		if m.Capabilities.Input[name] {
			input = append(input, name)
		}
		if m.Capabilities.Output[name] {
			output = append(output, name)
		}
	}
	return map[string]any{
		"id":             m.ID,
		"object":         "model",
		"created":        0,
		"owned_by":       ProviderID,
		"model":          m.ID,
		"name":           m.Name,
		"free":           true,
		"protocol":       protocolOf(m.Npm),
		"release_date":   m.ReleaseDate,
		"limit":          m.Limit,
		"context_window": m.Limit["context"],
		"modalities":     map[string]any{"input": input, "output": output},
		"reasoning":      m.Capabilities.Reasoning,
		"tool_call":      m.Capabilities.Toolcall,
		"variants":       m.Variants,
	}
}

func protocolOf(npm string) string {
	if protocol := sdkProtocol(npm); protocol != "" {
		return protocol
	}
	return "chat"
}

// --------------------------------------------------------------- model filters

// retiredStatuses are the catalog's ways of saying a model is not served.
var retiredStatuses = []string{"deprecated", "retired", "disabled"}

func retired(entry *catalogModel) bool {
	// The catalog names a model retired either by its status or, when it has
	// no status, by its lifecycle (`model.status ?? model.lifecycle`).
	status := entry.Status
	if status == "" {
		status = entry.Lifecycle
	}
	return entry.Deprecated || entry.DeprecatedAt != nil || entry.RetirementDate != nil ||
		slices.Contains(retiredStatuses, status)
}

func zeroCost(cost *catalogCost) bool {
	return cost != nil && cost.Input != nil && cost.Output != nil && *cost.Input == 0 && *cost.Output == 0
}

var freePattern = regexp.MustCompile(`(?i)free`)

// free reports whether Zen serves this model for free: a model the catalog
// doesn't describe (entry nil, which the id's own "-free" then decides) is not
// retired, and costs nothing only when the catalog says so.
func free(id string, entry *catalogModel) bool {
	if entry != nil && retired(entry) {
		return false
	}
	if freePattern.MatchString(id) {
		return true
	}
	return entry != nil && zeroCost(entry.Cost)
}

// listedFree is a model Zen lists whose id says it is free (Zen names them
// "-free") and that the catalog does not describe yet.
func listedFree(id string) bool { return strings.HasSuffix(id, "-free") }

var modalityNames = []string{"text", "image", "audio", "video", "pdf"}

func modalityFlags(values []string) map[string]bool {
	flags := map[string]bool{}
	for _, name := range modalityNames {
		flags[name] = slices.Contains(values, name)
	}
	return flags
}

func buildModels(ids []string, provider *catalogProvider, protocols map[string]string) map[string]*Model {
	result := map[string]*Model{}
	providerNpm := ""
	if provider != nil {
		providerNpm = provider.Npm
	}
	for _, id := range ids {
		if id == "" || id == "__proto__" {
			continue
		}
		entry, known := catalogEntry(provider, id)
		if entry == nil {
			// A model the catalog doesn't describe yet is held against an empty
			// entry, as the plugin holds it against `{}`: its id alone decides
			// whether it is free, and every field it doesn't set stays unset.
			entry = &catalogModel{}
		}
		protocol, hasProtocol := protocols[id]
		if !hasProtocol {
			switch {
			case known && entry.Provider != nil && entry.Provider.Npm != "":
				protocol = sdkProtocol(entry.Provider.Npm)
			case known:
				protocol = sdkProtocol(providerNpm)
			case listedFree(id):
				// Asked as OpenCode asks a model with no package of its own,
				// so a free model Zen listed today is served today.
				protocol = sdkProtocol(providerNpm)
				if protocol == "" {
					protocol = "chat"
				}
			}
		}
		npm, ok := SDK[protocol]
		if !ok || !free(id, entry) {
			continue
		}
		input, output := []string{"text"}, []string{"text"}
		if entry.Modalities != nil {
			// A modality list that is present, empty included, is what the
			// catalog says; only an absent one falls back to text.
			if entry.Modalities.Input != nil {
				input = entry.Modalities.Input
			}
			if entry.Modalities.Output != nil {
				output = entry.Modalities.Output
			}
		}
		attachment := false
		for _, name := range input {
			if name != "text" {
				attachment = true
				break
			}
		}
		limits := map[string]any{"context": 32768, "output": 4096}
		maps.Copy(limits, entry.Limit)
		variants := map[string]any{}
		for _, option := range entry.ReasoningOpts {
			if option.Type != "effort" {
				continue
			}
			for _, effort := range option.Values {
				variants[effort] = map[string]any{"reasoningEffort": effort}
			}
		}
		result[id] = &Model{
			ID:    id,
			Name:  cmp(entry.Name, id),
			Npm:   npm,
			Limit: limits,
			Capabilities: modelCapabilities{
				Reasoning:  deref(entry.Reasoning, false),
				Attachment: deref(entry.Attachment, attachment),
				Toolcall:   deref(entry.ToolCall, true),
				Input:      modalityFlags(input),
				Output:     modalityFlags(output),
			},
			Variants:    variants,
			ReleaseDate: entry.ReleaseDate,
		}
	}
	return result
}

func catalogEntry(provider *catalogProvider, id string) (*catalogModel, bool) {
	if provider == nil {
		return nil, false
	}
	entry, ok := provider.Models[id]
	if !ok {
		return nil, false
	}
	return &entry, true
}

func deref(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func cmp(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// --------------------------------------------------------------- protocol docs

// parseProtocolDocs reads OpenCode's console table of what each model is
// asked at, out of models.mdx.
func parseProtocolDocs(text string) map[string]string {
	pattern := regexp.MustCompile("\\|[^|]+\\|\\s*`?([^|`\\s]+)`?\\s*\\|\\s*`[^`]+/v1/(chat/completions|responses|messages|systemone)`")
	paths := map[string]string{
		"chat/completions": "chat",
		"responses":        "responses",
		"messages":         "anthropic",
		"systemone":        "systemone",
	}
	protocols := map[string]string{}
	for _, match := range pattern.FindAllStringSubmatch(text, -1) {
		protocols[match[1]] = paths[match[2]]
	}
	return protocols
}

// ------------------------------------------------------------ offline bootstrap

// What each free model is asked at, from OpenCode's console docs on
// 2026-10-15: used for a listed model when the docs can't be read, so the
// catalog or the docs being out of reach never leaves a listed free model
// out. Jev is a SystemOne decision model, not one to chat with.
var bootstrapProtocols = map[string]string{
	"big-pickle":                      "chat",
	"space-bunny-free":                "chat",
	"cat-jam-free":                    "chat",
	"longcat-2.5-preview-free":        "chat",
	"step-5-preview-free":             "chat",
	"mimo-v2.6-flash-free":            "chat",
	"nemotron-3.5-lightning-free":     "chat",
	"muse-spark-1.3-contributor-free": "responses",
	"jev-1.13-free":                   "systemone",
}

func zero() *catalogCost {
	amount := 0.0
	return &catalogCost{Input: &amount, Output: &amount}
}

// Offline bootstrap metadata from the OpenCode catalog, 2026-10-06; the free
// models listed since were added on 2026-10-15.
var bootstrapProvider = &catalogProvider{
	Npm: SDK["chat"],
	Models: map[string]catalogModel{
		"cat-jam-free": {
			Name:          "Cat Jam Free",
			Cost:          zero(),
			Limit:         map[string]any{"context": 262144, "output": 131072},
			Reasoning:     new(true),
			ToolCall:      new(true),
			ReasoningOpts: []catalogReasoningOption{{Type: "effort", Values: []string{"low", "medium", "high", "xhigh", "max"}}},
		},
		"step-5-preview-free": {
			Name:          "Step 5 Preview Free",
			Cost:          zero(),
			Limit:         map[string]any{"context": 1000000, "input": 1000000, "output": 65536},
			Reasoning:     new(true),
			ToolCall:      new(true),
			Modalities:    &catalogModalities{Input: []string{"text", "image", "video"}, Output: []string{"text"}},
			ReasoningOpts: []catalogReasoningOption{{Type: "effort", Values: []string{"low", "medium", "high"}}},
		},
		"longcat-2.5-preview-free": {
			Name:       "LongCat 2.5 Preview Free",
			Cost:       zero(),
			Limit:      map[string]any{"context": 1000000, "output": 131072},
			Reasoning:  new(true),
			ToolCall:   new(true),
			Modalities: &catalogModalities{Input: []string{"text", "image"}, Output: []string{"text"}},
		},
		"mimo-v2.6-flash-free": {
			Name:       "MiMo-V2.6-Flash Free",
			Cost:       zero(),
			Limit:      map[string]any{"context": 200000, "output": 32000},
			Reasoning:  new(true),
			ToolCall:   new(true),
			Modalities: &catalogModalities{Input: []string{"text", "image", "audio", "video"}, Output: []string{"text"}},
		},
		"nemotron-3.5-lightning-free": {
			Name:      "Nemotron 3.5 Lightning Free",
			Cost:      zero(),
			Limit:     map[string]any{"context": 262144, "output": 262144},
			Reasoning: new(true),
			ToolCall:  new(true),
		},
		"big-pickle": {
			Name:      "Big Pickle",
			Cost:      zero(),
			Limit:     map[string]any{"context": 200000, "input": 160000, "output": 32000},
			Reasoning: new(true),
			ToolCall:  new(true),
		},
		"space-bunny-free": {
			Name:      "Space Bunny Free",
			Cost:      zero(),
			Limit:     map[string]any{"context": 1048576, "input": 524288, "output": 524288},
			Reasoning: new(true),
			ToolCall:  new(true),
		},
		"muse-spark-1.3-contributor-free": {
			Name:          "Muse Spark 1.3 Free",
			Cost:          zero(),
			Limit:         map[string]any{"context": 1048576, "output": 131072},
			Reasoning:     new(true),
			ToolCall:      new(true),
			Modalities:    &catalogModalities{Input: []string{"text", "image", "video", "pdf", "audio"}, Output: []string{"text"}},
			ReasoningOpts: []catalogReasoningOption{{Type: "effort", Values: []string{"minimal", "low", "medium", "high", "xhigh"}}},
			Provider:      &struct{ Npm string }{Npm: SDK["responses"]},
		},
	},
}
