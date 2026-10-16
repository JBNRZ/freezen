package server

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"freezen/internal/logging"
	"freezen/internal/zen"
)

// Server serves OpenCode Zen's free models, in the three request formats the
// models are asked at: OpenAI Chat Completions, OpenAI Responses and Anthropic
// Messages. See README.md for what each turn of a reply goes through.

type Server struct {
	provider *zen.Provider
	log      *logging.Logger
}

func New(provider *zen.Provider, log *logging.Logger) *Server {
	return &Server{provider: provider, log: log}
}

// protocolOfPath answers which of the three formats a path asks for, so an
// agent may use /v1/... or the bare path.
func protocolOfPath(path string) string {
	trimmed := strings.TrimSuffix(path, "/")
	for protocol, suffix := range protocolPaths {
		if trimmed == suffix || trimmed == "/v1"+suffix {
			return protocol
		}
	}
	return ""
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if protocol := protocolOfPath(r.URL.Path); protocol != "" {
		s.handleInference(protocol).ServeHTTP(w, r)
		return
	}
	switch strings.TrimSuffix(r.URL.Path, "/") {
	case "/v1/models", "/models":
		s.handleModels(w, r)
	case "/healthz", "/health":
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	case "":
		s.handleIndex(w, r)
	default:
		writeError(w, http.StatusNotFound, "", "Not found. Use /v1/chat/completions, /v1/responses, /v1/messages or /v1/models.")
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	models := s.provider.FreeModelNames()
	writeJSON(w, http.StatusOK, map[string]any{
		"name":     "freezen",
		"provider": zen.ProviderID,
		"base":     zen.Base,
		"endpoints": []string{
			"POST /v1/chat/completions",
			"POST /v1/responses",
			"POST /v1/messages",
			"GET  /v1/models",
		},
		"models": models,
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "", "Method not allowed")
		return
	}
	models := s.provider.ListModels(r.Context())
	ids := make([]string, 0, len(models))
	for id := range models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	data := make([]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, models[id].ListView())
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) handleInference(protocol string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, protocol, "Unsupported Zen endpoint or method")
			return
		}
		body, err := readRequestBody(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, protocol, err.Error())
			return
		}
		name, _ := body["model"].(string)
		entry := s.provider.Resolve(r.Context(), name)
		if entry == nil {
			// Said as a model not served, so the caller sets this model aside
			// and not the account, whose other free models are still served.
			free := strings.Join(s.provider.FreeModelNames(), ", ")
			if free == "" {
				free = "none"
			}
			s.log.Warnf("%s request for model %s is not a free model", protocol, name)
			writeJSON(w, http.StatusNotFound, map[string]any{
				"type": "error",
				"error": map[string]any{
					"type":    "not_found_error",
					"message": fmt.Sprintf("Model %s is not available as an OpenCode Zen free model now. Free models: %s.", name, free),
				},
			})
			return
		}
		if entry.Npm != zen.SDK[protocol] {
			writeError(w, http.StatusBadRequest, protocol,
				"Model is not an available free model for this protocol. Refresh the model list.")
			return
		}

		// A streamed reply is what Zen answers with either way: a client that
		// doesn't stream gets the reply collapsed afterwards.
		streaming := body["stream"] == true
		body["stream"] = true
		policy, err := normalizeTools(body, protocol)
		if err != nil {
			writeError(w, http.StatusBadRequest, protocol, err.Error())
			return
		}
		if protocol == "chat" {
			includeUsage(body)
		}
		session := sessionID(r.Header, body)
		project := r.Header.Get("x-opencode-project")
		if project == "" {
			project = "global"
		}
		s.log.Infof("%s request model=%s stream=%t session=%s", protocol, name, streaming, session)
		encoded, err := json.Marshal(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, protocol, err.Error())
			return
		}
		requestHeaders := zen.UpstreamHeaders(r.Header, protocol, session, project, s.provider.APIKey())
		response, err := s.provider.Send(r.Context(), http.MethodPost,
			zen.Base+protocolPaths[protocol], requestHeaders, encoded)
		if err != nil {
			s.log.Errorf("%s request model=%s failed: %v", protocol, name, err)
			writeError(w, http.StatusBadGateway, protocol, "Zen request failed: "+err.Error())
			return
		}
		defer response.Body.Close()
		s.log.Debugf("%s request model=%s answered %d", protocol, name, response.StatusCode)
		s.relay(w, response, protocol, policy, streaming)
	}
}

// relay hands the reply back: guarded, and collapsed for a client that asked
// for one ordinary JSON reply.
func (s *Server) relay(w http.ResponseWriter, response *http.Response, protocol string, policy toolPolicy, streaming bool) {
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		copyResponseHeaders(w.Header(), response.Header)
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
		return
	}
	check := newNameChecker(policy)

	if isEventStream(response.Header) {
		if streaming {
			copyResponseHeaders(w.Header(), response.Header)
			w.Header().Set("content-type", "text/event-stream")
			w.Header().Set("cache-control", "no-cache")
			w.Header().Set("x-accel-buffering", "no")
			w.WriteHeader(response.StatusCode)
			flusher, _ := w.(http.Flusher)
			err := guardStream(response.Body, protocol, check, true, func(chunk []byte) error {
				if _, err := w.Write(chunk); err != nil {
					return err
				}
				if flusher != nil {
					flusher.Flush()
				}
				return nil
			})
			if err != nil {
				// The client was cut off mid-stream: it must not read a
				// reply that ends as if it were whole.
				panic(http.ErrAbortHandler)
			}
			return
		}
		var guarded bytes.Buffer
		hold := func(chunk []byte) error {
			_, err := guarded.Write(chunk)
			return err
		}
		if err := guardStream(response.Body, protocol, check, false, hold); err != nil {
			panic(http.ErrAbortHandler)
		}
		payload, collapsedFailure := collapse(protocol, bytes.NewReader(guarded.Bytes()))
		status := response.StatusCode
		if collapsedFailure != "" {
			status = http.StatusBadGateway
			payload = collapseFailure(protocol, collapsedFailure)
		}
		copyResponseHeaders(w.Header(), response.Header)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		encoded, err := json.Marshal(payload)
		if err == nil {
			_, _ = w.Write(encoded)
		}
		return
	}

	raw, err := io.ReadAll(io.LimitReader(response.Body, maxEventBytes))
	if err != nil {
		writeError(w, http.StatusBadGateway, protocol, "Zen reply failed: "+err.Error())
		return
	}
	var payload any
	var failureMessage string
	switch protocol {
	case "chat":
		var completion chatCompletion
		if err := json.Unmarshal(raw, &completion); err != nil {
			failureMessage = "Invalid JSON in upstream response"
			break
		}
		if err := guardChatCompletion(&completion, check); err != nil {
			failureMessage = err.Error()
			break
		}
		payload = &completion
	case "responses":
		var object responseObject
		if err := json.Unmarshal(raw, &object); err != nil {
			failureMessage = "Invalid JSON in upstream response"
			break
		}
		if err := guardResponseObject(&object, check); err != nil {
			failureMessage = err.Error()
			break
		}
		payload = &object
	default:
		var message anthropicMessage
		if err := json.Unmarshal(raw, &message); err != nil {
			failureMessage = "Invalid JSON in upstream response"
			break
		}
		if err := guardAnthropicMessage(&message, check); err != nil {
			failureMessage = err.Error()
			break
		}
		payload = &message
	}
	status := response.StatusCode
	if failureMessage != "" {
		status = http.StatusBadGateway
		payload = failure(protocol, failureMessage, false)
	}
	copyResponseHeaders(w.Header(), response.Header)
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	encoded, err := json.Marshal(payload)
	if err == nil {
		_, _ = w.Write(encoded)
	}
}

// writeJSON writes one JSON reply.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = w.Write(encoded)
}

// writeError answers a request with the protocol's own error shape.
func writeError(w http.ResponseWriter, status int, protocol, message string) {
	writeJSON(w, status, failure(protocol, message, false))
}
