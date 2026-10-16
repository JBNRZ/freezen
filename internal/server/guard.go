package server

import (
	"encoding/json/v2"
	"fmt"
	"strings"
)

// The reply side: Zen's models call OpenCode's own tool names whatever the
// agent declared, and call tools no agent has. A call is renamed onto the
// agent's tool when exactly one folds to it, and is otherwise left out, with
// the reply's text saying so (tool-response.mjs).

type guardError struct{ message string }

func (e *guardError) Error() string { return e.message }

func failf(format string, args ...any) *guardError {
	return &guardError{message: fmt.Sprintf(format, args...)}
}

var callFinishReasons = map[string]bool{"tool_calls": true, "function_call": true}

// nameChecker answers the agent's name for a call, or false for a call of no
// tool the agent has (a missing name included).
type nameChecker func(name string) (string, bool)

var nameSeparators = strings.NewReplacer(" ", "", "_", "", ".", "", "-", "")

func lower(name string) string { return strings.ToLower(name) }

func bare(name string) string { return nameSeparators.Replace(strings.ToLower(name)) }

// foldsTo keys every declared name, keeping "" for a name two of them fold to:
// such a call is never guessed between.
func foldsTo(declared []string, key func(string) string) map[string]string {
	folded := map[string]string{}
	for _, name := range declared {
		k := key(name)
		if k == "" {
			continue
		}
		if _, seen := folded[k]; seen {
			folded[k] = ""
			continue
		}
		folded[k] = name
	}
	return folded
}

func newNameChecker(policy toolPolicy) nameChecker {
	declared := map[string]bool{}
	for _, name := range policy.declared {
		declared[name] = true
	}
	byCase := foldsTo(policy.declared, lower)
	byShape := foldsTo(policy.declared, bare)
	return func(name string) (string, bool) {
		if name == "" {
			return "", false
		}
		if declared[name] {
			return name, true
		}
		if match, ok := byCase[lower(name)]; ok {
			if match == "" {
				return "", false
			}
			return match, true
		}
		if match, ok := byShape[bare(name)]; ok && match != "" {
			return match, true
		}
		return "", false
	}
}

// droppedNote is what a call left out becomes, where the agent's user and
// the model's next turn both read it.
func droppedNote(name string) string {
	if name == "" {
		return "(The model called a tool without a name. The call was left out.)"
	}
	quoted, err := json.Marshal(firstRunes(name, 128))
	if err != nil {
		quoted = []byte(`""`)
	}
	return "(The model called a tool this agent doesn't have: " + string(quoted) + ". The call was left out.)"
}

// cutShortNote ends a Chat reply Zen closed before its [DONE] (#91).
const cutShortNote = "(The reply was cut short: Zen closed the stream before it finished. Ask again to go on.)"

// failure is the reply shape a protocol answers a guard failure with.
func failure(protocol, message string, stream bool) any {
	switch protocol {
	case "anthropic":
		return map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": message}}
	case "responses":
		if stream {
			return map[string]any{"type": "error", "code": "undeclared_tool", "message": message}
		}
		return map[string]any{"error": map[string]any{"type": "upstream_error", "code": "undeclared_tool", "message": message}}
	default:
		return map[string]any{"error": map[string]any{"message": message, "type": "upstream_error"}}
	}
}

func firstRunes(value string, count int) string {
	runes := []rune(value)
	if len(runes) > count {
		runes = runes[:count]
	}
	return string(runes)
}

// ------------------------------------------------------------------ chat

func appendChatNotes(message *chatMessage, notes []string) {
	if len(notes) == 0 {
		return
	}
	text := strings.Join(notes, "\n\n")
	switch content := message.Content.(type) {
	case string:
		if content != "" {
			message.Content = content + "\n\n" + text
			return
		}
		message.Content = text
	case []any:
		message.Content = append(content, map[string]any{"type": "text", "text": text})
	default:
		message.Content = text
	}
}

func hasChatCalls(message *chatMessage) bool {
	return message.FunctionCall != nil || len(message.ToolCalls) > 0
}

// guardChatMessage renames the message's calls to the agent's tools and
// leaves out the rest, answering the notes for those left out.
func guardChatMessage(message *chatMessage, check nameChecker) (bool, []string, error) {
	if message == nil {
		return false, nil, nil
	}
	changed := false
	var notes []string
	if message.FunctionCall != nil {
		name, ok := check(message.FunctionCall.Name)
		if !ok {
			notes = append(notes, droppedNote(message.FunctionCall.Name))
			message.FunctionCall = nil
			changed = true
		} else if name != message.FunctionCall.Name {
			message.FunctionCall.Name = name
			changed = true
		}
	}
	if len(message.ToolCalls) > 0 {
		kept := make([]chatToolCall, 0, len(message.ToolCalls))
		for _, call := range message.ToolCalls {
			if call.Type != "" && call.Type != "function" {
				return changed, notes, failf("Unsupported upstream tool call")
			}
			name, ok := check(call.Function.Name)
			if !ok {
				notes = append(notes, droppedNote(call.Function.Name))
				changed = true
				continue
			}
			if name != call.Function.Name {
				call.Function.Name = name
				changed = true
			}
			kept = append(kept, call)
		}
		message.ToolCalls = kept
	}
	return changed, notes, nil
}

// guardChatCompletion fixes a whole Chat Completions reply.
func guardChatCompletion(completion *chatCompletion, check nameChecker) error {
	for index := range completion.Choices {
		choice := &completion.Choices[index]
		_, notes, err := guardChatMessage(choice.Message, check)
		if err != nil {
			return err
		}
		if len(notes) > 0 {
			appendChatNotes(choice.Message, notes)
			if choice.FinishReason != nil && !hasChatCalls(choice.Message) &&
				callFinishReasons[*choice.FinishReason] {
				choice.FinishReason = completion.finishReason("stop")
			}
		}
	}
	return nil
}

// -------------------------------------------------------------- responses

func isResponseCall(item *responseItem) bool { return item.Type == "function_call" }

// guardResponseOutput renames the output's calls in place, or replaces each
// one left out with text saying so, so no later index moves.
func guardResponseOutput(output []responseItem, check nameChecker) (bool, int, int, error) {
	kept, left := 0, 0
	changed := false
	for index := range output {
		item := &output[index]
		if strings.HasSuffix(item.Type, "_call") && !isResponseCall(item) {
			return changed, kept, left, failf("Unsupported upstream tool call type")
		}
		if !isResponseCall(item) {
			continue
		}
		name, ok := check(item.Name)
		if !ok {
			id := item.ID
			if id == "" {
				id = "msg_" + cmp(item.CallID, "dropped")
			}
			output[index] = responseItem{
				ID:     id,
				Type:   "message",
				Role:   "assistant",
				Status: cmp(item.Status, "completed"),
				Content: []responsePart{{
					Type:        "output_text",
					Text:        droppedNote(item.Name),
					Annotations: []any{},
				}},
			}
			changed = true
			left++
			continue
		}
		if name != item.Name {
			item.Name = name
			changed = true
		}
		kept++
	}
	return changed, kept, left, nil
}

// guardResponseObject fixes a whole Responses reply.
func guardResponseObject(object *responseObject, check nameChecker) error {
	_, _, _, err := guardResponseOutput(object.Output, check)
	return err
}

// -------------------------------------------------------------- anthropic

func isAnthropicCall(block *anthropicBlock) bool {
	return block.Type == "tool_use" || block.Type == "server_tool_use"
}

// guardAnthropicContent renames the content's calls in place, or replaces
// each one left out with text saying so.
func guardAnthropicContent(content []anthropicBlock, check nameChecker) (bool, int, int, error) {
	kept, left := 0, 0
	changed := false
	for index := range content {
		block := &content[index]
		if !isAnthropicCall(block) {
			continue
		}
		name, ok := check(block.Name)
		if !ok {
			content[index] = anthropicBlock{Type: "text", Text: droppedNote(block.Name)}
			changed = true
			left++
			continue
		}
		if name != block.Name {
			block.Name = name
			changed = true
		}
		kept++
	}
	return changed, kept, left, nil
}

// guardAnthropicMessage fixes a whole Messages reply; a message whose every
// call was left out ends as a turn, not as tool use.
func guardAnthropicMessage(message *anthropicMessage, check nameChecker) error {
	_, kept, left, err := guardAnthropicContent(message.Content, check)
	if err != nil {
		return err
	}
	if left > 0 && kept == 0 && message.Type == "message" &&
		message.StopReason != nil && *message.StopReason == "tool_use" {
		stop := "end_turn"
		message.StopReason = &stop
	}
	return nil
}
