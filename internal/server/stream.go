package server

import (
	"bufio"
	"encoding/json/v2"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Reading, guarding and collapsing an upstream SSE reply (stream.mjs and
// tool-response.mjs of the plugin).

const maxEventBytes = 32 << 20

// The reader's structural failures are guard failures: the plugin throws them
// as GuardErrors, which a streaming reply answers with a protocol error event
// rather than by tearing the connection down.
var errEventTooLarge = failf("Upstream SSE event exceeds the 32 MiB buffer limit")
var errInsideEvent = failf("Upstream SSE ended inside an event")

// --------------------------------------------------------------- SSE reading

type sseFrame struct {
	name string
	data []string
	raw  []byte
	keep []byte // the frame's lines that are not data lines
}

func (f *sseFrame) text() string { return strings.Join(f.data, "\n") }

func sseField(line string) (string, string) {
	before, after, ok := strings.Cut(line, ":")
	if !ok {
		return line, ""
	}
	content := after
	if strings.HasPrefix(content, " ") {
		content = content[1:]
	}
	return before, content
}

type sseReader struct {
	reader *bufio.Reader
	strict bool
	raw    []byte
	keep   []byte
	data   []string
	name   string
	first  bool
	size   int
	bytes  int
}

func newSSEReader(source io.Reader, strict bool) *sseReader {
	return &sseReader{reader: bufio.NewReaderSize(source, 32<<10), strict: strict, first: true}
}

// next returns the next complete event, or nil at the end of the stream. A
// strict reader (the one that streams a reply to the client) fails on a
// stream that ends inside an event; a lenient one (the collapser) drops it.
func (s *sseReader) next() (*sseFrame, error) {
	for {
		line, terminator, err := readSSELine(s.reader)
		if err != nil {
			if s.strict && (s.size > 0 || len(s.data) > 0) {
				return nil, errInsideEvent
			}
			return nil, nil
		}
		s.raw = append(s.raw, line...)
		s.raw = append(s.raw, terminator...)
		s.size += len(line) + len(terminator)
		s.bytes += len(line) + len(terminator)
		if s.size > maxEventBytes {
			return nil, errEventTooLarge
		}
		text := string(line)
		if s.first {
			text = strings.TrimPrefix(text, "\uFEFF")
			s.first = false
		}
		if text == "" {
			frame := &sseFrame{name: s.name, data: s.data, raw: s.raw, keep: s.keep}
			s.raw, s.keep, s.data, s.name, s.size = nil, nil, nil, "", 0
			return frame, nil
		}
		field, content := sseField(text)
		switch field {
		case "event":
			s.name = content
		case "data":
			s.data = append(s.data, content)
		default:
			if !strings.HasPrefix(text, ":") {
				s.keep = append(s.keep, line...)
				s.keep = append(s.keep, terminator...)
			}
		}
	}
}

func readSSELine(reader *bufio.Reader) ([]byte, string, error) {
	var line []byte
	for {
		value, err := reader.ReadByte()
		if err != nil {
			return line, "", err
		}
		switch value {
		case '\n':
			return line, "\n", nil
		case '\r':
			if next, err := reader.ReadByte(); err == nil {
				if next == '\n' {
					return line, "\r\n", nil
				}
				_ = reader.UnreadByte()
			}
			return line, "\r", nil
		}
		line = append(line, value)
	}
}

// ------------------------------------------------------------------- writing

func encodeEvent(payload any, name string) string {
	var builder strings.Builder
	if name != "" {
		builder.WriteString("event: " + name + "\n")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		encoded = []byte("null")
	}
	builder.WriteString("data: ")
	builder.Write(encoded)
	builder.WriteString("\n\n")
	return builder.String()
}

func encodeEvents(events []streamEvent) []byte {
	var out []byte
	for _, event := range events {
		out = append(out, encodeEvent(event.payload, event.name)...)
	}
	return out
}

// rewrite keeps the frame's own non-data lines and gives it new data.
func rewrite(frame *sseFrame, payload any) []byte {
	encoded, err := json.Marshal(payload)
	if err != nil {
		encoded = []byte("null")
	}
	return append(append([]byte{}, frame.keep...), append([]byte("data: "), append(encoded, '\n', '\n')...)...)
}

// ------------------------------------------------------------ chat buffering

// A Chat reply's tool calls are held until the choice finishes, so a name can
// be renamed across every fragment and a call left out takes its place in the
// stream.
type pendingTools struct {
	header chatCompletion
	calls  map[int]chatToolCall
	order  []int
	legacy *chatFunctionCall
	bytes  int
}

type chatToolBuffer struct {
	pending map[int]*pendingTools
	wrote   map[int]bool
	ended   map[int]bool
	size    int
}

func newChatToolBuffer() *chatToolBuffer {
	return &chatToolBuffer{
		pending: map[int]*pendingTools{},
		wrote:   map[int]bool{},
		ended:   map[int]bool{},
	}
}

func (b *chatToolBuffer) pendingCount() int { return len(b.pending) }

func (b *chatToolBuffer) charge(state *pendingTools, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return failf("Unsupported upstream tool delta")
	}
	b.size += len(encoded)
	state.bytes += len(encoded)
	if b.size > maxEventBytes {
		return failf("Pending upstream tools exceed the 32 MiB buffer limit")
	}
	return nil
}

func (b *chatToolBuffer) stateFor(index int, chunk *chatCompletion) *pendingTools {
	if state, ok := b.pending[index]; ok {
		return state
	}
	state := &pendingTools{
		header: chatCompletion{
			ID:                chunk.ID,
			Object:            chunk.Object,
			Created:           chunk.Created,
			Model:             chunk.Model,
			SystemFingerprint: chunk.SystemFingerprint,
			ServiceTier:       chunk.ServiceTier,
		},
		calls: map[int]chatToolCall{},
	}
	b.pending[index] = state
	_ = b.charge(state, state.header)
	return state
}

func (b *chatToolBuffer) remove(index int) {
	if state, ok := b.pending[index]; ok {
		b.size -= state.bytes
	}
	delete(b.pending, index)
}

// complete turns the calls held for a choice into the delta that publishes
// them, with the calls of no tool the agent has left out.
func (b *chatToolBuffer) complete(state *pendingTools, index int, check nameChecker) (*chatDelta, error) {
	delta := &chatDelta{}
	for _, key := range state.order {
		delta.ToolCalls = append(delta.ToolCalls, state.calls[key])
	}
	if state.legacy != nil {
		delta.FunctionCall = state.legacy
	}
	message := &chatMessage{ToolCalls: delta.ToolCalls, FunctionCall: delta.FunctionCall}
	_, notes, err := guardChatMessage(message, check)
	if err != nil {
		return nil, err
	}
	delta.ToolCalls = message.ToolCalls
	delta.FunctionCall = message.FunctionCall
	if len(notes) > 0 {
		for position := range delta.ToolCalls {
			offset := position
			delta.ToolCalls[position].Index = &offset
		}
		content := strings.Join(notes, "\n\n")
		if b.wrote[index] {
			content = "\n\n" + content
		}
		delta.Content = &content
		if !hasChatCalls(message) {
			b.ended[index] = true
		}
	}
	return delta, nil
}

// apply holds back a chunk's tool deltas, publishing each choice's calls when
// it finishes. It returns nil for a chunk that says nothing at all.
func (b *chatToolBuffer) apply(chunk *chatCompletion, check nameChecker) (*chatCompletion, bool, error) {
	changed := false
	if err := guardChatCompletion(chunk, check); err != nil {
		return nil, false, err
	}
	for index := range chunk.Choices {
		choice := &chunk.Choices[index]
		delta := choice.Delta
		if delta == nil {
			continue
		}
		if delta.Content != nil && *delta.Content != "" {
			b.wrote[choice.Index] = true
		}
		if len(delta.ToolCalls) == 0 && delta.FunctionCall == nil {
			continue
		}
		changed = true
		state := b.stateFor(choice.Index, chunk)
		for _, fragment := range delta.ToolCalls {
			if fragment.Type != "" && fragment.Type != "function" {
				return nil, false, failf("Unsupported upstream tool delta")
			}
			if err := b.charge(state, fragment); err != nil {
				return nil, false, err
			}
			key, ok := toolIndex(fragment.Index)
			if !ok {
				return nil, false, failf("Invalid upstream tool index")
			}
			previous, seen := state.calls[key]
			if !seen {
				previous = chatToolCall{}
				state.order = append(state.order, key)
			}
			offset := key
			previous.Index = &offset
			if fragment.ID != "" {
				previous.ID = fragment.ID
			}
			if fragment.Type != "" {
				previous.Type = fragment.Type
			}
			previous.Function.Name += fragment.Function.Name
			previous.Function.Arguments += fragment.Function.Arguments
			state.calls[key] = previous
		}
		if delta.FunctionCall != nil {
			if err := b.charge(state, delta.FunctionCall); err != nil {
				return nil, false, err
			}
			if state.legacy == nil {
				state.legacy = &chatFunctionCall{}
			}
			state.legacy.Name += delta.FunctionCall.Name
			state.legacy.Arguments += delta.FunctionCall.Arguments
		}
		choice.Delta.ToolCalls = nil
		choice.Delta.FunctionCall = nil
	}
	// Validate every finishing choice before publishing any part of this chunk.
	for index := range chunk.Choices {
		choice := &chunk.Choices[index]
		if choice.FinishReason == nil {
			continue
		}
		state, ok := b.pending[choice.Index]
		if !ok {
			continue
		}
		delta, err := b.complete(state, choice.Index, check)
		if err != nil {
			return nil, false, err
		}
		// The chunk's own delta kept whatever it said; the calls published
		// now go in its place, with any note before what it said.
		own := choice.Delta
		merged := &chatDelta{}
		if own != nil {
			merged.Role = own.Role
			merged.Content = own.Content
			merged.Refusal = own.Refusal
			merged.Reasoning = own.Reasoning
			merged.ReasoningContent = own.ReasoningContent
		}
		if delta.Content != nil {
			if own != nil && own.Content != nil {
				text := *own.Content + *delta.Content
				merged.Content = &text
			} else {
				merged.Content = delta.Content
			}
		}
		merged.ToolCalls = delta.ToolCalls
		merged.FunctionCall = delta.FunctionCall
		choice.Delta = merged
		b.remove(choice.Index)
		changed = true
	}
	for index := range chunk.Choices {
		choice := &chunk.Choices[index]
		if choice.FinishReason != nil && callFinishReasons[*choice.FinishReason] && b.ended[choice.Index] {
			choice.FinishReason = chunk.finishReason("stop")
			changed = true
		}
	}
	if !changed {
		return chunk, false, nil
	}
	kept := make([]chatChoice, 0, len(chunk.Choices))
	for _, choice := range chunk.Choices {
		if choice.kept() {
			kept = append(kept, choice)
		}
	}
	chunk.Choices = kept
	if len(chunk.Choices) == 0 && chunk.Usage == nil {
		return nil, true, nil
	}
	return chunk, true, nil
}

// finish publishes the calls of every choice still open, which a stream that
// ended without a [DONE] leaves behind.
func (b *chatToolBuffer) finish(check nameChecker) (*chatCompletion, error) {
	if len(b.pending) == 0 {
		return nil, nil
	}
	indices := make([]int, 0, len(b.pending))
	for index := range b.pending {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	var final *chatCompletion
	for _, index := range indices {
		state := b.pending[index]
		if final == nil {
			final = &chatCompletion{
				ID:                state.header.ID,
				Object:            state.header.Object,
				Created:           state.header.Created,
				Model:             state.header.Model,
				SystemFingerprint: state.header.SystemFingerprint,
				ServiceTier:       state.header.ServiceTier,
			}
			if final.Object == "" {
				final.Object = "chat.completion.chunk"
			}
		}
		delta, err := b.complete(state, index, check)
		if err != nil {
			return nil, err
		}
		final.Choices = append(final.Choices, chatChoice{Index: index, Delta: delta})
	}
	b.pending = map[int]*pendingTools{}
	b.size = 0
	return final, nil
}

func toolIndex(value *int) (int, bool) {
	if value == nil || *value < 0 {
		return 0, false
	}
	return *value, true
}

// ------------------------------------------------------ responses streaming

type responsesStreamGuard struct {
	check   nameChecker
	left    map[int]bool
	leftIDs map[string]bool
	kept    int
	dropped int
}

func newResponsesStreamGuard(check nameChecker) *responsesStreamGuard {
	return &responsesStreamGuard{
		check:   check,
		left:    map[int]bool{},
		leftIDs: map[string]bool{},
	}
}

type streamEvent struct {
	name    string
	payload any
}

func eventIndex(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

// apply checks a Responses event; a call left out is streamed as text in its
// place, so its arguments are never sent. It answers the events to add, and
// whether the event itself changed or is to be dropped.
func (g *responsesStreamGuard) apply(event *responsesEvent) ([]streamEvent, bool, bool, error) {
	if event.Type == "response.output_item.added" && event.Item != nil &&
		event.Item.Type == "function_call" {
		if _, ok := g.check(event.Item.Name); !ok {
			note := droppedNote(event.Item.Name)
			item := event.Item
			id := item.ID
			if id == "" {
				id = "msg_" + cmp(item.CallID, "dropped")
			}
			outputIndex := eventIndex(event.OutputIndex)
			contentIndex := eventIndex(event.ContentIndex)
			g.left[outputIndex] = true
			g.leftIDs[id] = true
			g.leftIDs[event.ItemID] = true
			g.dropped++
			// The call's item becomes a message saying it was left out, so no
			// later index moves; what it said is streamed as text in its place.
			event.Item = &responseItem{
				ID:     id,
				Type:   "message",
				Role:   "assistant",
				Status: "in_progress",
			}
			where := func(name string) *responsesEvent {
				return &responsesEvent{
					Type:         name,
					OutputIndex:  event.OutputIndex,
					ContentIndex: &contentIndex,
					ItemID:       id,
				}
			}
			added := where("response.content_part.added")
			added.Part = &responsePart{Type: "output_text", Annotations: []any{}}
			delta := where("response.output_text.delta")
			delta.Delta = note
			done := where("response.output_text.done")
			done.Text = note
			finished := where("response.content_part.done")
			finished.Part = &responsePart{Type: "output_text", Text: note, Annotations: []any{}}
			return []streamEvent{
				{name: "response.content_part.added", payload: added},
				{name: "response.output_text.delta", payload: delta},
				{name: "response.output_text.done", payload: done},
				{name: "response.content_part.done", payload: finished},
			}, true, false, nil
		}
	}
	if strings.HasPrefix(event.Type, "response.function_call_arguments.") {
		if event.OutputIndex != nil && g.left[*event.OutputIndex] {
			return nil, true, true, nil
		}
		if g.leftIDs[event.ItemID] {
			return nil, true, true, nil
		}
	}
	changed := false
	if event.Item != nil {
		item := []responseItem{*event.Item}
		itemChanged, kept, left, err := guardResponseOutput(item, g.check)
		if err != nil {
			return nil, false, false, err
		}
		changed = itemChanged
		g.kept += kept
		g.dropped += left
		event.Item = &item[0]
	}
	if event.Response != nil {
		if err := guardResponseObject(event.Response, g.check); err != nil {
			return nil, false, false, err
		}
	}
	return nil, changed, false, nil
}

// ------------------------------------------------------ anthropic streaming

type anthropicStreamGuard struct {
	check   nameChecker
	left    map[int]bool
	kept    int
	dropped int
}

func newAnthropicStreamGuard(check nameChecker) *anthropicStreamGuard {
	return &anthropicStreamGuard{check: check, left: map[int]bool{}}
}

func (g *anthropicStreamGuard) apply(event *anthropicEvent) ([]streamEvent, bool, bool, error) {
	if event.Type == "message_start" {
		g.left = map[int]bool{}
		g.kept = 0
		g.dropped = 0
	}
	if event.Type == "content_block_start" && event.ContentBlock != nil &&
		isAnthropicCall(event.ContentBlock) {
		if _, ok := g.check(event.ContentBlock.Name); !ok {
			note := droppedNote(event.ContentBlock.Name)
			g.left[eventIndex(event.Index)] = true
			g.dropped++
			// The block becomes text saying the call was left out; its
			// arguments are never sent.
			event.ContentBlock = &anthropicBlock{Type: "text"}
			delta := &anthropicEvent{
				Type:  "content_block_delta",
				Index: event.Index,
				Delta: &anthropicDelta{Type: "text_delta", Text: note},
			}
			return []streamEvent{
				{name: "content_block_delta", payload: delta},
			}, true, false, nil
		}
	}
	if event.Type == "content_block_delta" && event.Index != nil && g.left[*event.Index] {
		return nil, true, true, nil
	}
	if event.Type == "message_delta" && event.Delta != nil && event.Delta.StopReason != nil &&
		*event.Delta.StopReason == "tool_use" && g.dropped > 0 && g.kept == 0 {
		stop := "end_turn"
		event.Delta.StopReason = &stop
		return nil, true, false, nil
	}
	changed := false
	if event.ContentBlock != nil {
		block := []anthropicBlock{*event.ContentBlock}
		blockChanged, kept, left, err := guardAnthropicContent(block, g.check)
		if err != nil {
			return nil, false, false, err
		}
		changed = blockChanged
		g.kept += kept
		g.dropped += left
		event.ContentBlock = &block[0]
	}
	if event.Message != nil {
		if err := guardAnthropicMessage(event.Message, g.check); err != nil {
			return nil, false, false, err
		}
	}
	return nil, changed, false, nil
}

// --------------------------------------------------------- chat cut short

// A Chat stream Zen closed before its [DONE] is ended as a turn cut short
// when it has said something and no tool call of it is pending (#91). A
// pending call's arguments may be half written and a reply that said nothing
// is an empty one, so both still fail.
type chatTracker struct {
	open     map[int]bool
	finished map[int]bool
	wrote    map[int]bool
	said     bool
	header   chatCompletion
}

func newChatTracker() *chatTracker {
	return &chatTracker{
		open:     map[int]bool{},
		finished: map[int]bool{},
		wrote:    map[int]bool{},
	}
}

func (t *chatTracker) see(chunk *chatCompletion) {
	if chunk.ID != "" {
		t.header.ID = chunk.ID
	}
	if chunk.Created != 0 {
		t.header.Created = chunk.Created
	}
	if chunk.Model != "" {
		t.header.Model = chunk.Model
	}
	for _, choice := range chunk.Choices {
		delta := choice.Delta
		if delta != nil {
			for _, value := range []*string{delta.Content, delta.ReasoningContent, delta.Reasoning} {
				if value != nil && *value != "" {
					t.said = true
				}
			}
			if delta.Content != nil && *delta.Content != "" {
				t.wrote[choice.Index] = true
			}
		}
		if choice.FinishReason != nil {
			t.finished[choice.Index] = true
			delete(t.open, choice.Index)
		} else if !t.finished[choice.Index] {
			t.open[choice.Index] = true
		}
	}
}

// ending answers the frames that end a reply cut short, or "" when it can't.
func (t *chatTracker) ending() string {
	if !t.said {
		return ""
	}
	indices := make([]int, 0, len(t.open))
	for index := range t.open {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	header := t.header
	if header.ID == "" {
		header.ID = "chatcmpl-cut-short"
	}
	if header.Object == "" {
		header.Object = "chat.completion.chunk"
	}
	if header.Created == 0 {
		header.Created = time.Now().Unix()
	}
	out := ""
	for _, index := range indices {
		content := cutShortNote
		if t.wrote[index] {
			content = "\n\n" + content
		}
		header.Choices = append(header.Choices, chatChoice{
			Index:        index,
			Delta:        &chatDelta{Content: &content},
			FinishReason: header.finishReason("stop"),
		})
	}
	if len(header.Choices) > 0 {
		out = encodeEvent(&header, "")
	}
	return out + "data: [DONE]\n\n"
}

// ------------------------------------------------------------ the guard loop

type guardState struct {
	protocol  string
	check     nameChecker
	buffer    *chatToolBuffer
	tracker   *chatTracker
	responses *responsesStreamGuard
	messages  *anthropicStreamGuard
	completed bool
	replyID   string
}

func newGuardState(protocol string, check nameChecker, endCutShort bool) *guardState {
	state := &guardState{protocol: protocol, check: check}
	if protocol == "chat" {
		state.buffer = newChatToolBuffer()
		if endCutShort {
			state.tracker = newChatTracker()
		}
		return state
	}
	if protocol == "responses" {
		state.responses = newResponsesStreamGuard(check)
	} else {
		state.messages = newAnthropicStreamGuard(check)
	}
	return state
}

// guardEvent decodes one event, applies the guard, and answers the bytes the
// client should read (nil for an event that is dropped). failed says the
// upstream reported an error in the event itself.
func (s *guardState) guardEvent(frame *sseFrame) (out []byte, failed bool, err error) {
	switch s.protocol {
	case "chat":
		var chunk chatCompletion
		if err := json.Unmarshal([]byte(frame.text()), &chunk); err != nil {
			return nil, false, failf("Invalid JSON in upstream response")
		}
		if chunk.ID != "" {
			s.replyID = chunk.ID
		}
		if chunk.Error != nil {
			return frame.raw, true, nil
		}
		guarded, changed, err := s.buffer.apply(&chunk, s.check)
		if err != nil {
			return nil, false, err
		}
		if s.tracker != nil && guarded != nil {
			s.tracker.see(guarded)
		}
		if guarded == nil {
			return nil, false, nil
		}
		if changed {
			return rewrite(frame, guarded), false, nil
		}
		return frame.raw, false, nil

	case "responses":
		var event responsesEvent
		if err := json.Unmarshal([]byte(frame.text()), &event); err != nil {
			return nil, false, failf("Invalid JSON in upstream response")
		}
		typ := event.Type
		if typ == "" {
			typ = frame.name
		}
		if event.Response != nil && event.Response.ID != "" {
			s.replyID = event.Response.ID
		}
		if event.ID != "" {
			s.replyID = event.ID
		}
		if event.Error != nil || typ == "error" || typ == "response.failed" ||
			(event.Response != nil && event.Response.Status == "failed") {
			return frame.raw, true, nil
		}
		extra, changed, dropped, err := s.responses.apply(&event)
		if err != nil {
			return nil, false, err
		}
		if typ == "response.completed" || typ == "response.incomplete" {
			s.completed = true
		}
		if dropped {
			return encodeEvents(extra), false, nil
		}
		if changed {
			return append(rewrite(frame, &event), encodeEvents(extra)...), false, nil
		}
		return append(frame.raw, encodeEvents(extra)...), false, nil

	default:
		var event anthropicEvent
		if err := json.Unmarshal([]byte(frame.text()), &event); err != nil {
			return nil, false, failf("Invalid JSON in upstream response")
		}
		typ := event.Type
		if typ == "" {
			typ = frame.name
		}
		if event.Message != nil && event.Message.ID != "" {
			s.replyID = event.Message.ID
		}
		if event.Error != nil || typ == "error" {
			return frame.raw, true, nil
		}
		extra, changed, dropped, err := s.messages.apply(&event)
		if err != nil {
			return nil, false, err
		}
		if typ == "message_stop" {
			s.completed = true
		}
		if dropped {
			return encodeEvents(extra), false, nil
		}
		if changed {
			return append(rewrite(frame, &event), encodeEvents(extra)...), false, nil
		}
		return append(frame.raw, encodeEvents(extra)...), false, nil
	}
}

// guardStream reads an upstream SSE reply, checks every event, and hands what
// the client should read to emit.
func guardStream(source io.Reader, protocol string, check nameChecker, endCutShort bool, emit func([]byte) error) error {
	reader := newSSEReader(source, true)
	state := newGuardState(protocol, check, endCutShort)
	var bytesRead, events int

	fail := func(err error) error {
		guard, ok := err.(*guardError)
		if !ok {
			return err
		}
		name := "error"
		if protocol == "chat" {
			name = ""
		}
		return emit([]byte(encodeEvent(failure(protocol, guard.message, true), name)))
	}

	for {
		frame, err := reader.next()
		if err != nil {
			return fail(err)
		}
		bytesRead = reader.bytes
		if frame == nil {
			unfinished := state.buffer != nil && state.buffer.pendingCount() > 0
			if !state.completed && !unfinished && state.tracker != nil {
				if ending := state.tracker.ending(); ending != "" {
					return emit([]byte(ending))
				}
			}
			if !state.completed || unfinished {
				came := []string{}
				if state.replyID != "" {
					came = append(came, "reply "+firstRunes(state.replyID, 128))
				}
				plural := "s"
				if events == 1 {
					plural = ""
				}
				came = append(came, fmt.Sprintf("%d event%s", events, plural),
					fmt.Sprintf("%d bytes received", bytesRead))
				if unfinished {
					came = append(came, "a tool call unfinished")
				}
				return fail(&guardError{message: "Upstream SSE ended without a completion marker (" +
					strings.Join(came, ", ") + ")"})
			}
			return nil
		}
		trimmed := strings.TrimSpace(frame.text())
		if frame.name != "error" && trimmed == "" {
			if err := emit(frame.raw); err != nil {
				return err
			}
			continue
		}
		if frame.name != "error" && trimmed == "[DONE]" {
			if protocol != "chat" {
				return fail(failf("Unexpected upstream completion marker"))
			}
			final, err := state.buffer.finish(check)
			if err != nil {
				return fail(err)
			}
			state.completed = true
			out := []byte{}
			if final != nil {
				out = append(out, encodeEvent(final, "")...)
			}
			out = append(out, frame.raw...)
			if err := emit(out); err != nil {
				return err
			}
			continue
		}
		events++
		if frame.name == "error" {
			return emit(frame.raw)
		}
		out, failed, err := state.guardEvent(frame)
		if err != nil {
			return fail(err)
		}
		if len(out) > 0 {
			if err := emit(out); err != nil {
				return err
			}
		}
		if failed {
			return nil
		}
	}
}
