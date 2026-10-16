package server

import (
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
)

// The request side: what is sent to Zen, and how it is built up from what the
// agent sent (provider.mjs, models.mjs).

const (
	// Zen's free tier only answers a request that carries tools, so one is
	// sent with the five core OpenCode tools the agent didn't declare.
	maxRequestBytes = 128 << 20
)

var protocolPaths = map[string]string{
	"chat":      "/chat/completions",
	"responses": "/responses",
	"anthropic": "/messages",
}

var coreTools = []string{"bash", "edit", "glob", "grep", "read"}

// The parameter each stand-in takes, OpenCode's own first one: Zen's free
// tier answers 403 FreeTierError to bash or read offered with none.
var coreToolParameter = map[string]string{
	"bash": "command",
	"edit": "filePath",
	"glob": "pattern",
	"grep": "pattern",
	"read": "filePath",
}

const notAvailableDescription = "Not available. Never call this tool."

var errNotAnObject = errors.New("expected a JSON request object")

// readRequestBody decodes the request body, keeping every field so nothing an
// agent sends is dropped on the way to Zen.
func readRequestBody(r *http.Request) (map[string]any, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes))
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	body, ok := decoded.(map[string]any)
	if !ok {
		return nil, errNotAnObject
	}
	return body, nil
}

// toolPolicy is the set of tool names the agent declared; a call is matched
// against it, and everything else is left out of the reply.
type toolPolicy struct {
	declared []string
}

// normalizeTools fills in the five core OpenCode tools the agent didn't
// declare, described as not available, and returns what the agent did
// declare. Zen's free tier answers 403 FreeTierError to a request without
// tools, and to bash or read offered with no parameters.
func normalizeTools(body map[string]any, protocol string) (toolPolicy, error) {
	rawTools, hasTools := body["tools"]
	if hasTools {
		if _, ok := rawTools.([]any); !ok {
			return toolPolicy{}, errors.New("tools must be an array")
		}
	} else {
		body["tools"] = []any{}
	}
	tools, _ := body["tools"].([]any)

	present := map[string]bool{}
	var declared []string
	declare := func(tool map[string]any) {
		var name string
		var ok bool
		if protocol == "chat" {
			if function, isMap := tool["function"].(map[string]any); isMap {
				name, ok = function["name"].(string)
			}
		} else {
			name, ok = tool["name"].(string)
		}
		if !ok || present[name] {
			return
		}
		present[name] = true
		declared = append(declared, name)
	}
	for _, entry := range tools {
		if tool, ok := entry.(map[string]any); ok {
			declare(tool)
		}
	}
	if protocol == "chat" {
		if functions, ok := body["functions"].([]any); ok {
			for _, entry := range functions {
				if function, ok := entry.(map[string]any); ok {
					declare(function)
				}
			}
		}
	}

	for _, name := range coreTools {
		if present[name] {
			continue
		}
		parameters := map[string]any{
			"type":                 "object",
			"properties":           map[string]any{coreToolParameter[name]: map[string]any{"type": "string"}},
			"additionalProperties": false,
		}
		var stand any
		switch protocol {
		case "chat":
			stand = map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        name,
					"description": notAvailableDescription,
					"parameters":  parameters,
				},
			}
		case "anthropic":
			stand = map[string]any{
				"name":         name,
				"description":  notAvailableDescription,
				"input_schema": parameters,
			}
		default:
			stand = map[string]any{
				"type":        "function",
				"name":        name,
				"description": notAvailableDescription,
				"parameters":  parameters,
			}
		}
		tools = append(tools, stand)
	}
	body["tools"] = tools
	return toolPolicy{declared: declared}, nil
}

// includeUsage turns on usage in a Chat Completions stream.
func includeUsage(body map[string]any) {
	options, _ := body["stream_options"].(map[string]any)
	if options == nil {
		options = map[string]any{}
	}
	options["include_usage"] = true
	body["stream_options"] = options
}
