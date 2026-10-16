package zen

import (
	"net/http"
	"time"

	"freezen/internal/opencode"
)

// UpstreamHeaders sends exactly what OpenCode 1.19.0 sends to its gateway, so
// a caller's own headers (an agent's User-Agent, x-stainless-*, x-session-...)
// never mark the request as not OpenCode's.
func UpstreamHeaders(given http.Header, protocol, session, project, apiKey string) http.Header {
	headers := http.Header{}
	headers.Set("content-type", "application/json")
	headers.Set("accept", "*/*")
	headers.Set("user-agent", opencode.UserAgent(protocol))
	headers.Set("x-opencode-project", project)
	headers.Set("x-opencode-session", session)
	headers.Set("x-opencode-request", opencode.ID("msg", false, time.Now()))
	headers.Set("x-opencode-client", "cli")
	headers.Set("x-opencode-session-id", session)
	if protocol == "anthropic" {
		headers.Set("x-api-key", apiKey)
		version := given.Get("anthropic-version")
		if version == "" {
			version = "2023-06-01"
		}
		headers.Set("anthropic-version", version)
		if beta := given.Get("anthropic-beta"); beta != "" {
			headers.Set("anthropic-beta", beta)
		}
	} else {
		headers.Set("authorization", "Bearer "+apiKey)
	}
	return headers
}
