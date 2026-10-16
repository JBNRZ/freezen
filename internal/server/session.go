package server

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"sync"
	"time"

	"freezen/internal/opencode"
)

// One made-up session per conversation so its turns keep one session, with an
// id in OpenCode's own shape (provider.mjs).

var sessionHeaders = []string{
	"x-opencode-session",
	"x-opencode-session-id",
	"x-session-affinity",
	"x-session-id",
	"conversation-id",
}

const keepSessions = 1000

// sessions holds one made-up session per conversation, the last keepSessions
// of them.
var sessions = &sessionStore{order: list.New(), seen: map[string]*list.Element{}}

type sessionStore struct {
	mu    sync.Mutex
	order *list.List
	seen  map[string]*list.Element
}

// session answers the session of a conversation, making one the first time
// that conversation is asked about.
func (s *sessionStore) session(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if element, ok := s.seen[key]; ok {
		s.order.MoveToFront(element)
		return element.Value.(string)
	}
	value := opencode.ID("ses", true, time.Now())
	s.seen[key] = s.order.PushFront(value)
	if s.order.Len() > keepSessions {
		oldest := s.order.Back()
		s.order.Remove(oldest)
		delete(s.seen, oldest.Value.(string))
	}
	return value
}

// falsy reports whether a decoded JSON value is one JavaScript would treat as
// falsy, which is what a conversation's first thing has to be to have no
// session of its own.
func falsy(value any) bool {
	switch value := value.(type) {
	case nil:
		return true
	case string:
		return value == ""
	case bool:
		return !value
	case float64:
		return value == 0
	}
	return false
}

// sessionID keeps one session for a conversation: a session id the caller
// gave when it is a real one, otherwise one made up here, keyed by the first
// thing the conversation said.
func sessionID(header http.Header, body map[string]any) string {
	signal := ""
	for _, name := range sessionHeaders {
		if value := header.Get(name); value != "" {
			signal = value
			break
		}
	}
	if signal == "" {
		if metadata, ok := body["metadata"].(map[string]any); ok {
			if value, ok := metadata["session_id"].(string); ok {
				signal = value
			}
		}
	}
	if opencode.RealSessionID(signal) {
		return signal
	}
	if signal == "" {
		// One session per conversation, keyed by what it first said:
		// body.messages ?? body.input, whose first user message is used when it
		// is a list and the value itself when it isn't (a Responses input may
		// be a plain string).
		messages, present := body["messages"]
		if !present || messages == nil {
			messages = body["input"]
		}
		var first any
		if list, isList := messages.([]any); isList {
			for _, entry := range list {
				message, ok := entry.(map[string]any)
				if !ok {
					continue
				}
				if role, _ := message["role"].(string); role == "user" {
					first = message["content"]
					break
				}
			}
		} else {
			first = messages
		}
		if falsy(first) {
			return opencode.ID("ses", true, time.Now())
		}
		encoded, err := json.Marshal(first)
		if err != nil {
			return opencode.ID("ses", true, time.Now())
		}
		signal = string(encoded)
	}
	sum := sha256.Sum256([]byte("ses\x00" + signal))
	return sessions.session(hex.EncodeToString(sum[:]))
}
