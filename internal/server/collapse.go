package server

import (
	"encoding/json/v2"
	"errors"
	"io"
	"sort"
	"strings"
)

// Collapsing a streamed reply into one ordinary JSON reply, for a client that
// doesn't stream (stream.mjs).

// collapseFailure is the reply shape a protocol answers a collapsed stream's
// failure with.
func collapseFailure(protocol, message string) any {
	detail := map[string]any{"type": "upstream_error", "message": message}
	if protocol == "anthropic" {
		return map[string]any{"type": "error", "error": detail}
	}
	detail["param"] = nil
	detail["code"] = nil
	return map[string]any{"error": detail}
}

func collapseErrorMessage(value any, fallback string) string {
	if text, ok := value.(string); ok && text != "" {
		return text
	}
	if object, ok := value.(map[string]any); ok {
		if text, ok := object["message"].(string); ok && text != "" {
			return text
		}
		if inner, ok := object["error"]; ok {
			if object, ok := inner.(map[string]any); ok {
				if text, ok := object["message"].(string); ok && text != "" {
					return text
				}
			}
		}
	}
	return fallback
}

// ---------------------------------------------------------------- chat

type chatCollapse struct {
	header  chatCompletion
	choices map[int]*chatChoice
	tools   map[int]map[int]chatToolCall
}

func newChatCollapse() *chatCollapse {
	return &chatCollapse{choices: map[int]*chatChoice{}, tools: map[int]map[int]chatToolCall{}}
}

func (c *chatCollapse) apply(chunk *chatCompletion) error {
	if chunk.ID != "" {
		c.header.ID = chunk.ID
	}
	if chunk.Created != 0 {
		c.header.Created = chunk.Created
	}
	if chunk.Model != "" {
		c.header.Model = chunk.Model
	}
	if chunk.SystemFingerprint != "" {
		c.header.SystemFingerprint = chunk.SystemFingerprint
	}
	if chunk.ServiceTier != "" {
		c.header.ServiceTier = chunk.ServiceTier
	}
	if chunk.Usage != nil {
		c.header.Usage = chunk.Usage
	}
	for _, part := range chunk.Choices {
		if part.Index < 0 {
			return errors.New("Invalid stream item index")
		}
		choice, ok := c.choices[part.Index]
		if !ok {
			choice = &chatChoice{Index: part.Index, Message: &chatMessage{Role: "assistant"}}
			c.choices[part.Index] = choice
			c.tools[part.Index] = map[int]chatToolCall{}
		}
		if delta := part.Delta; delta != nil {
			if delta.Role != "" {
				choice.Message.Role = delta.Role
			}
			if delta.Content != nil && *delta.Content != "" {
				existing, _ := choice.Message.Content.(string)
				choice.Message.Content = existing + *delta.Content
			}
			if delta.ReasoningContent != nil {
				choice.Message.ReasoningContent += *delta.ReasoningContent
			}
			if delta.Reasoning != nil {
				choice.Message.Reasoning += *delta.Reasoning
			}
			if delta.Refusal != nil {
				choice.Message.Refusal += *delta.Refusal
			}
			if delta.FunctionCall != nil {
				if choice.Message.FunctionCall == nil {
					choice.Message.FunctionCall = &chatFunctionCall{}
				}
				choice.Message.FunctionCall.Name += delta.FunctionCall.Name
				choice.Message.FunctionCall.Arguments += delta.FunctionCall.Arguments
			}
			calls := c.tools[part.Index]
			for _, fragment := range delta.ToolCalls {
				key, ok := toolIndex(fragment.Index)
				if !ok {
					return errors.New("Invalid stream item index")
				}
				call, seen := calls[key]
				if !seen {
					call = chatToolCall{Type: "function"}
				}
				if fragment.ID != "" {
					call.ID = fragment.ID
				}
				if fragment.Type != "" {
					call.Type = fragment.Type
				}
				call.Function.Name += fragment.Function.Name
				call.Function.Arguments += fragment.Function.Arguments
				calls[key] = call
			}
		}
		if part.FinishReason != nil {
			choice.FinishReason = part.FinishReason
		}
		if len(part.Logprobs) > 0 {
			choice.Logprobs = part.Logprobs
		}
	}
	return nil
}

func (c *chatCollapse) finish() *chatCompletion {
	body := c.header
	body.Object = "chat.completion"
	body.Error = nil
	body.Choices = nil
	for _, index := range sortedIntKeys(c.choices) {
		choice := c.choices[index]
		if calls := c.tools[index]; len(calls) > 0 {
			for _, key := range sortedIntKeys(calls) {
				choice.Message.ToolCalls = append(choice.Message.ToolCalls, calls[key])
			}
		}
		body.Choices = append(body.Choices, *choice)
	}
	if body.Choices == nil {
		body.Choices = []chatChoice{}
	}
	return &body
}

func sortedIntKeys[V any](values map[int]V) []int {
	keys := make([]int, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	return keys
}

// ------------------------------------------------------------- responses

type responsesCollapse struct {
	body   responseObject
	final  *responseObject
	output map[int]responseItem
}

func newResponsesCollapse() *responsesCollapse {
	return &responsesCollapse{
		body:   responseObject{Object: "response"},
		output: map[int]responseItem{},
	}
}

func (c *responsesCollapse) item(data *responsesEvent, kind string) responseItem {
	position := eventIndex(data.OutputIndex)
	if data.OutputIndex == nil && data.ItemID != "" {
		for index, item := range c.output {
			if item.ID == data.ItemID {
				position = index
				break
			}
		}
	}
	item, ok := c.output[position]
	if !ok {
		item = responseItem{Type: kind}
		if data.ItemID != "" {
			item.ID = data.ItemID
		}
		switch kind {
		case "message":
			item.Role = "assistant"
			item.Content = []responsePart{}
		case "function_call":
			item.Arguments = ""
		case "reasoning":
			item.Summary = []responsePart{}
		}
	}
	return item
}

func (c *responsesCollapse) apply(data *responsesEvent, typ string) error {
	switch typ {
	case "response.created", "response.in_progress":
		if data.Response != nil {
			if data.Response.ID != "" {
				c.body.ID = data.Response.ID
			}
			if data.Response.Status != "" {
				c.body.Status = data.Response.Status
			}
			if data.Response.Model != "" {
				c.body.Model = data.Response.Model
			}
			if data.Response.CreatedAt != 0 {
				c.body.CreatedAt = data.Response.CreatedAt
			}
			if len(data.Response.Usage) > 0 {
				c.body.Usage = data.Response.Usage
			}
			for position, item := range data.Response.Output {
				c.output[position] = item
			}
		}
	case "response.output_item.added", "response.output_item.done":
		if data.Item != nil {
			c.output[eventIndex(data.OutputIndex)] = *data.Item
		}
	case "response.content_part.added", "response.content_part.done":
		item := c.item(data, "message")
		index := eventIndex(data.ContentIndex)
		for len(item.Content) <= index {
			item.Content = append(item.Content, responsePart{})
		}
		if data.Part != nil {
			item.Content[index] = *data.Part
		}
		c.output[eventIndex(data.OutputIndex)] = item
	case "response.output_text.delta", "response.output_text.done", "response.refusal.delta", "response.refusal.done":
		item := c.item(data, "message")
		index := eventIndex(data.ContentIndex)
		for len(item.Content) <= index {
			item.Content = append(item.Content, responsePart{})
		}
		refusal := strings.HasPrefix(typ, "response.refusal.")
		if item.Content[index].Type == "" {
			if refusal {
				item.Content[index] = responsePart{Type: "refusal"}
			} else {
				item.Content[index] = responsePart{Type: "output_text", Annotations: []any{}}
			}
		}
		if strings.HasSuffix(typ, ".delta") {
			if refusal {
				item.Content[index].Refusal += data.Delta
			} else {
				item.Content[index].Text += data.Delta
			}
		} else if refusal {
			if data.Refusal != "" {
				item.Content[index].Refusal = data.Refusal
			}
		} else if data.Text != "" {
			item.Content[index].Text = data.Text
		}
		c.output[eventIndex(data.OutputIndex)] = item
	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
		item := c.item(data, "function_call")
		if strings.HasSuffix(typ, ".delta") {
			item.Arguments += data.Delta
		} else if data.Arguments != "" {
			item.Arguments = data.Arguments
		}
		c.output[eventIndex(data.OutputIndex)] = item
	case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		item := c.item(data, "reasoning")
		index := eventIndex(data.SummaryIndex)
		for len(item.Summary) <= index {
			item.Summary = append(item.Summary, responsePart{})
		}
		if data.Part != nil {
			item.Summary[index] = *data.Part
		}
		c.output[eventIndex(data.OutputIndex)] = item
	case "response.reasoning_text.delta", "response.reasoning_text.done",
		"response.reasoning_summary_text.delta", "response.reasoning_summary_text.done":
		item := c.item(data, "reasoning")
		summary := strings.Contains(typ, "summary")
		index := eventIndex(data.ContentIndex)
		parts := item.Content
		kind := "reasoning_text"
		if summary {
			index = eventIndex(data.SummaryIndex)
			parts = item.Summary
			kind = "summary_text"
		}
		for len(parts) <= index {
			parts = append(parts, responsePart{})
		}
		if parts[index].Type == "" {
			parts[index] = responsePart{Type: kind}
		}
		if strings.HasSuffix(typ, ".delta") {
			parts[index].Text += data.Delta
		} else if data.Text != "" {
			parts[index].Text = data.Text
		}
		if summary {
			item.Summary = parts
		} else {
			item.Content = parts
		}
		c.output[eventIndex(data.OutputIndex)] = item
	case "response.reasoning_content.delta":
		c.body.ReasoningContent += data.Delta
	case "response.completed", "response.incomplete":
		if data.Response != nil {
			c.final = data.Response
		}
		if typ == "response.completed" {
			c.body.Status = "completed"
		} else {
			c.body.Status = "incomplete"
		}
		if len(data.IncompleteDetails) > 0 {
			c.body.IncompleteDetails = data.IncompleteDetails
		}
		if data.FinishReason != "" {
			c.body.FinishReason = data.FinishReason
		}
	}
	if len(data.Usage) > 0 {
		c.body.Usage = data.Usage
	}
	return nil
}

func (c *responsesCollapse) finish() *responseObject {
	if c.final != nil {
		return c.final
	}
	body := c.body
	body.Output = nil
	for _, index := range sortedIntKeys(c.output) {
		body.Output = append(body.Output, c.output[index])
	}
	if body.Output == nil {
		body.Output = []responseItem{}
	}
	return &body
}

// -------------------------------------------------------------- anthropic

type anthropicCollapse struct {
	body      anthropicMessage
	blocks    map[int]anthropicBlock
	arguments map[int]string
}

func newAnthropicCollapse() *anthropicCollapse {
	return &anthropicCollapse{
		body: anthropicMessage{
			Type:    "message",
			Role:    "assistant",
			Content: []anthropicBlock{},
		},
		blocks:    map[int]anthropicBlock{},
		arguments: map[int]string{},
	}
}

func (c *anthropicCollapse) apply(event *anthropicEvent, typ string) error {
	switch typ {
	case "message_start":
		if event.Message != nil {
			c.body = *event.Message
		}
		for position, block := range c.body.Content {
			c.blocks[position] = block
		}
	case "content_block_start":
		if event.ContentBlock != nil {
			c.blocks[eventIndex(event.Index)] = *event.ContentBlock
		}
	case "content_block_delta":
		position := eventIndex(event.Index)
		block, ok := c.blocks[position]
		if !ok {
			return errors.New("Content delta without a content block")
		}
		if event.Delta == nil {
			return nil
		}
		switch event.Delta.Type {
		case "text_delta":
			block.Text += event.Delta.Text
		case "thinking_delta":
			block.Thinking += event.Delta.Thinking
		case "signature_delta":
			block.Signature += event.Delta.Signature
		case "input_json_delta":
			c.arguments[position] += event.Delta.PartialJSON
		}
		c.blocks[position] = block
	case "message_delta":
		if event.Delta != nil {
			if event.Delta.StopReason != nil {
				c.body.StopReason = event.Delta.StopReason
			}
			if event.Delta.StopSequence != nil {
				c.body.StopSequence = event.Delta.StopSequence
			}
		}
		if event.Usage != nil {
			c.body.Usage = event.Usage
		}
	}
	return nil
}

func (c *anthropicCollapse) finish() (*anthropicMessage, error) {
	for _, position := range sortedIntKeys(c.arguments) {
		var input any
		if err := json.Unmarshal([]byte(c.arguments[position]), &input); err != nil {
			return nil, errors.New("Invalid tool input JSON in upstream stream")
		}
		block, ok := c.blocks[position]
		if !ok {
			continue
		}
		block.Input = input
		c.blocks[position] = block
	}
	body := c.body
	body.Content = nil
	for _, index := range sortedIntKeys(c.blocks) {
		body.Content = append(body.Content, c.blocks[index])
	}
	if body.Content == nil {
		body.Content = []anthropicBlock{}
	}
	return &body, nil
}

// -------------------------------------------------------------- the collapse

type collapseState struct {
	protocol  string
	completed bool
	chat      *chatCollapse
	responses *responsesCollapse
	messages  *anthropicCollapse
}

func newCollapseState(protocol string) *collapseState {
	state := &collapseState{protocol: protocol}
	switch protocol {
	case "chat":
		state.chat = newChatCollapse()
	case "responses":
		state.responses = newResponsesCollapse()
	default:
		state.messages = newAnthropicCollapse()
	}
	return state
}

func (s *collapseState) apply(frame *sseFrame) error {
	switch s.protocol {
	case "chat":
		var chunk chatCompletion
		if err := json.Unmarshal([]byte(frame.text()), &chunk); err != nil {
			return errors.New("Invalid JSON in upstream SSE event")
		}
		if chunk.Error != nil {
			return errors.New(collapseErrorMessage(chunk.Error, "Upstream stream failed"))
		}
		return s.chat.apply(&chunk)
	case "responses":
		var event responsesEvent
		if err := json.Unmarshal([]byte(frame.text()), &event); err != nil {
			return errors.New("Invalid JSON in upstream SSE event")
		}
		typ := event.Type
		if typ == "" {
			typ = frame.name
		}
		if event.Error != nil || typ == "error" || typ == "response.failed" ||
			(event.Response != nil && event.Response.Status == "failed") {
			return errors.New(collapseErrorMessage(event.Error, "Upstream stream failed"))
		}
		if s.completed {
			return nil
		}
		if err := s.responses.apply(&event, typ); err != nil {
			return err
		}
		if typ == "response.completed" || typ == "response.incomplete" {
			s.completed = true
		}
		return nil
	default:
		var event anthropicEvent
		if err := json.Unmarshal([]byte(frame.text()), &event); err != nil {
			return errors.New("Invalid JSON in upstream SSE event")
		}
		typ := event.Type
		if typ == "" {
			typ = frame.name
		}
		if event.Error != nil || typ == "error" {
			return errors.New(collapseErrorMessage(event.Error, "Upstream stream failed"))
		}
		if s.completed {
			return nil
		}
		if err := s.messages.apply(&event, typ); err != nil {
			return err
		}
		if typ == "message_stop" {
			s.completed = true
		}
		return nil
	}
}

func (s *collapseState) finish() (any, error) {
	switch s.protocol {
	case "chat":
		return s.chat.finish(), nil
	case "responses":
		return s.responses.finish(), nil
	default:
		return s.messages.finish()
	}
}

// collapse turns a streamed reply into one ordinary JSON reply, or answers
// with a failure naming why it could not.
func collapse(protocol string, source io.Reader) (any, string) {
	reader := newSSEReader(source, false)
	state := newCollapseState(protocol)
	failure := ""
	for failure == "" {
		frame, err := reader.next()
		if err != nil {
			failure = collapseErrorMessage(err, "Upstream stream failed")
			break
		}
		if frame == nil {
			break
		}
		if frame.name == "error" {
			var value any
			if json.Unmarshal([]byte(frame.text()), &value) == nil {
				failure = collapseErrorMessage(value, "Upstream stream failed")
			} else {
				failure = frame.text()
			}
			break
		}
		trimmed := strings.TrimSpace(frame.text())
		if trimmed == "" {
			continue
		}
		if trimmed == "[DONE]" {
			if protocol == "chat" {
				state.completed = true
			}
			continue
		}
		if err := state.apply(frame); err != nil {
			failure = collapseErrorMessage(err, "Invalid upstream SSE event")
			break
		}
	}
	if failure == "" && !state.completed {
		failure = "Upstream SSE ended without a completion marker"
	}
	if failure != "" {
		return nil, failure
	}
	body, err := state.finish()
	if err != nil {
		return nil, collapseErrorMessage(err, "Upstream stream failed")
	}
	return body, ""
}
