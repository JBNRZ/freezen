package server

import (
	"net/http"
	"strings"
)

// Small helpers shared by the handlers.

func isEventStream(header http.Header) bool {
	value := header.Get("content-type")
	if index := strings.IndexByte(value, ';'); index >= 0 {
		value = value[:index]
	}
	return strings.ToLower(strings.TrimSpace(value)) == "text/event-stream"
}

// copyResponseHeaders gives the client the upstream's headers, without the
// ones that describe the body this server is about to write itself.
func copyResponseHeaders(target, source http.Header) {
	for key, values := range source {
		switch strings.ToLower(key) {
		case "content-length", "content-encoding", "content-type", "transfer-encoding":
			continue
		}
		target[key] = append([]string{}, values...)
	}
}

// cmp answers value, or fallback when value is empty.
func cmp(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
