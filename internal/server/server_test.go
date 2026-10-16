package server

import (
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"freezen/internal/config"
	"freezen/internal/logging"
	"freezen/internal/opencode"
	"freezen/internal/zen"
)

// roundTripperFunc answers a request without a network, so a test can read
// exactly what the server sends to Zen.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func decodeInto(data string, target any) error { return json.Unmarshal([]byte(data), target) }

// newTestProvider is a provider on the default config, quiet, with no proxies,
// so its clients are the ones a test can point at a stub transport.
func newTestProvider() *zen.Provider {
	provider, err := zen.New(config.Default(), logging.New("error"))
	if err != nil {
		panic(err)
	}
	return provider
}

func TestSessionKeepsOnePerConversation(t *testing.T) {
	header := http.Header{}
	first := sessionID(header, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	})
	second := sessionID(header, map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"role": "assistant", "content": "hi"},
		},
	})
	if first != second {
		t.Fatalf("one conversation got two sessions: %q and %q", first, second)
	}
	other := sessionID(header, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "goodbye"}},
	})
	if other == first {
		t.Fatal("two conversations shared one session")
	}
}

func TestNormalizeToolsAddsTheFiveCoreTools(t *testing.T) {
	body := map[string]any{
		"model": "big-pickle",
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "todowrite"}}},
	}
	policy, err := normalizeTools(body, "chat")
	if err != nil {
		t.Fatal(err)
	}
	if len(policy.declared) != 1 || policy.declared[0] != "todowrite" {
		t.Fatalf("declared tools are %v", policy.declared)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 6 {
		t.Fatalf("the request carries %d tools", len(tools))
	}
	last, _ := tools[5].(map[string]any)
	function, _ := last["function"].(map[string]any)
	if function["name"] != "read" {
		t.Fatalf("the last tool is %v", function["name"])
	}
	parameters, _ := function["parameters"].(map[string]any)
	if _, ok := parameters["properties"].(map[string]any)["filePath"]; !ok {
		t.Fatalf("the read stand-in has no filePath: %v", parameters)
	}
}

func TestNormalizeToolsRejectsToolsThatAreNotAnArray(t *testing.T) {
	if _, err := normalizeTools(map[string]any{"tools": "none"}, "chat"); err == nil {
		t.Fatal("tools that are not an array were accepted")
	}
}

func TestNameGuardRenamesOnlyOneCandidate(t *testing.T) {
	check := newNameChecker(toolPolicy{declared: []string{"todowrite", "webfetch", "read", "todo_write"}})
	if name, ok := check("Bash"); ok || name != "" {
		t.Fatalf("a call of no declared tool was matched onto %q", name)
	}
	if name, ok := check("Read"); !ok || name != "read" {
		t.Fatalf("Read was not matched onto read (got %q)", name)
	}
	if name, ok := check("read"); !ok || name != "read" {
		t.Fatalf("read was not matched onto itself (got %q)", name)
	}
}

func TestNameGuardFoldsSeparatorsOnlyWhenOneFits(t *testing.T) {
	check := newNameChecker(toolPolicy{declared: []string{"todo_write"}})
	if name, ok := check("todowrite"); !ok || name != "todo_write" {
		t.Fatalf("todowrite was not matched onto todo_write (got %q)", name)
	}
	ambiguous := newNameChecker(toolPolicy{declared: []string{"todo_write", "todo-write"}})
	if name, ok := ambiguous("todowrite"); ok {
		t.Fatalf("a name two tools fold to was guessed between: %q", name)
	}
}

func TestGuardChatCompletionLeavesOutAnUnknownTool(t *testing.T) {
	check := newNameChecker(toolPolicy{declared: []string{"read"}})
	var completion chatCompletion
	if err := decodeInto(`{"choices":[{"index":0,"message":{"role":"assistant","content":"working",
		"tool_calls":[{"type":"function","function":{"name":"bedit","arguments":"{}"}}]},
		"finish_reason":"tool_calls"}]}`, &completion); err != nil {
		t.Fatal(err)
	}
	if err := guardChatCompletion(&completion, check); err != nil {
		t.Fatal(err)
	}
	choice := completion.Choices[0]
	if len(choice.Message.ToolCalls) != 0 {
		t.Fatalf("the call was not left out: %v", choice.Message.ToolCalls)
	}
	if choice.FinishReason == nil || *choice.FinishReason != "stop" {
		t.Fatalf("the reply still finishes as %v", choice.FinishReason)
	}
	text, _ := choice.Message.Content.(string)
	if !strings.Contains(text, "bedit") {
		t.Fatalf("the reply's text does not say which call was left out: %q", text)
	}
}

func TestGuardAnthropicMessageTurnsEndWhenEveryCallIsLeftOut(t *testing.T) {
	check := newNameChecker(toolPolicy{declared: []string{"read"}})
	var message anthropicMessage
	if err := decodeInto(`{"type":"message","stop_reason":"tool_use","content":[
		{"type":"tool_use","name":"ash","input":{}}]}`, &message); err != nil {
		t.Fatal(err)
	}
	if err := guardAnthropicMessage(&message, check); err != nil {
		t.Fatal(err)
	}
	if message.StopReason == nil || *message.StopReason != "end_turn" {
		t.Fatalf("the message still stops as %v", message.StopReason)
	}
	if message.Content[0].Type != "text" {
		t.Fatalf("the call was not left out: %v", message.Content[0])
	}
}

func TestGuardStreamRenamesACallAndKeepsTheStreamWhole(t *testing.T) {
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"Read\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	var out strings.Builder
	check := newNameChecker(toolPolicy{declared: []string{"read"}})
	err := guardStream(strings.NewReader(stream), "chat", check, true, func(chunk []byte) error {
		out.Write(chunk)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	guarded := out.String()
	if !strings.Contains(guarded, `"name":"read"`) {
		t.Fatalf("the call was not renamed:\n%s", guarded)
	}
	if strings.Contains(guarded, `"name":"Read"`) {
		t.Fatalf("the call was left as it came:\n%s", guarded)
	}
	if strings.Contains(guarded, "without a completion marker") {
		t.Fatalf("a whole reply was failed:\n%s", guarded)
	}
}

func TestGuardStreamEndsACutShortReplyAsATurn(t *testing.T) {
	stream := "data: {\"id\":\"chatcmpl_1\",\"model\":\"space-bunny-free\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"half an ans\"}}]}\n\n"
	var out strings.Builder
	check := newNameChecker(toolPolicy{})
	if err := guardStream(strings.NewReader(stream), "chat", check, true, func(chunk []byte) error {
		out.Write(chunk)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	guarded := out.String()
	if !strings.Contains(guarded, "cut short") || !strings.Contains(guarded, "[DONE]") {
		t.Fatalf("a reply cut short was not ended as a turn:\n%s", guarded)
	}
}

func TestGuardStreamFailsAReplyCutShortWithNothingSaid(t *testing.T) {
	var out strings.Builder
	check := newNameChecker(toolPolicy{})
	if err := guardStream(strings.NewReader("data: {\"id\":\"chatcmpl_1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n"),
		"chat", check, true, func(chunk []byte) error {
			out.Write(chunk)
			return nil
		}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "without a completion marker") {
		t.Fatalf("an empty reply cut short was not failed:\n%s", out.String())
	}
}

func TestGuardStreamReplacesALeftOutResponsesCallWithText(t *testing.T) {
	stream := "event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"fc_1","type":"function_call","name":"bedit","call_id":"call_1","arguments":""}}` + "\n\n" +
		"event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","sequence_number":2,"output_index":0,"item_id":"fc_1","delta":"{}"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","sequence_number":3,"response":{"id":"resp_1","status":"completed","output":[]}}` + "\n\n"
	check := newNameChecker(toolPolicy{declared: []string{"read"}})
	var out strings.Builder
	if err := guardStream(strings.NewReader(stream), "responses", check, true, func(chunk []byte) error {
		out.Write(chunk)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	guarded := out.String()
	if strings.Contains(guarded, "function_call") {
		t.Fatalf("a left-out call was still streamed:\n%s", guarded)
	}
	if !strings.Contains(guarded, `"type":"message"`) || !strings.Contains(guarded, "bedit") {
		t.Fatalf("the reply's text does not say which call was left out:\n%s", guarded)
	}
	if strings.Contains(guarded, `"sequence_number":0`) {
		t.Fatalf("a synthesized event carried a zero sequence number:\n%s", guarded)
	}
}

func TestGuardStreamReplacesALeftOutAnthropicCallWithText(t *testing.T) {
	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"stop_reason":null,"stop_sequence":null}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"ash","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	check := newNameChecker(toolPolicy{declared: []string{"read"}})
	var out strings.Builder
	if err := guardStream(strings.NewReader(stream), "anthropic", check, true, func(chunk []byte) error {
		out.Write(chunk)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	guarded := out.String()
	if strings.Contains(guarded, "tool_use") || strings.Contains(guarded, "partial_json") {
		t.Fatalf("a left-out call or its arguments was still streamed:\n%s", guarded)
	}
	if !strings.Contains(guarded, "end_turn") || !strings.Contains(guarded, "ash") {
		t.Fatalf("the message was not ended as text saying so:\n%s", guarded)
	}
}

func TestSessionKeysAResponsesStringInput(t *testing.T) {
	header := http.Header{}
	first := sessionID(header, map[string]any{"input": "hello"})
	second := sessionID(header, map[string]any{"input": "hello"})
	if first != second {
		t.Fatalf("one string conversation got two sessions: %q and %q", first, second)
	}
	if !opencode.RealSessionID(first) {
		t.Fatalf("the session %q is not one OpenCode could have made", first)
	}
	other := sessionID(header, map[string]any{"input": "goodbye"})
	if other == first {
		t.Fatal("two string conversations shared one session")
	}
}

func TestCollapseCollectsAChatStreamIntoOneReply(t *testing.T) {
	stream := "data: {\"id\":\"chatcmpl_2\",\"object\":\"chat.completion.chunk\",\"model\":\"big-pickle\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello \"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"there\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	body, collapsedFailure := collapse("chat", strings.NewReader(stream))
	if collapsedFailure != "" {
		t.Fatalf("the reply was not collapsed: %s", collapsedFailure)
	}
	completion, ok := body.(*chatCompletion)
	if !ok || completion.Choices[0].Message.Content != "hello there" {
		t.Fatalf("the collapsed reply is %+v", body)
	}
}

func TestInferenceSendsOpenCodesRequestAndGuardsTheReply(t *testing.T) {
	var sent *http.Request
	var sentBody []byte
	provider := newTestProvider()
	provider.SetTransport(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		sent = request
		sentBody, _ = io.ReadAll(request.Body)
		stream := "data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"model\":\"big-pickle\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"Read\",\"arguments\":\"\"}}]}}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
			"data: [DONE]\n\n"
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(stream)),
			Request:    request,
		}, nil
	}))
	instance := New(provider, logging.New("error"))

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"big-pickle","stream":true,"messages":[{"role":"user","content":"hi"}],`+
			`"tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object"}}}]}`))
	request.Header.Set("x-session-id", "conversation-a")
	request.Header.Set("authorization", "Bearer sk-user")
	request.Header.Set("user-agent", "some-agent/9.9")
	recorder := httptest.NewRecorder()
	instance.handleInference("chat").ServeHTTP(recorder, request)

	if got := sent.URL.String(); got != "https://opencode.ai/inference/v1/chat/completions" {
		t.Fatalf("the request went to %s", got)
	}
	// Only OpenCode's own headers, none of the caller's.
	if got := sent.Header.Get("user-agent"); got != opencode.UserAgent("chat") {
		t.Fatalf("the user-agent is %q", got)
	}
	if got := sent.Header.Get("authorization"); got != "Bearer public" {
		t.Fatalf("the authorization is %q", got)
	}
	if got := sent.Header.Get("x-opencode-client"); got != "cli" {
		t.Fatalf("x-opencode-client is %q", got)
	}
	if got := sent.Header.Get("x-opencode-project"); got != "global" {
		t.Fatalf("x-opencode-project is %q", got)
	}
	if got := sent.Header.Get("x-opencode-session"); !opencode.RealSessionID(got) {
		t.Fatalf("x-opencode-session is %q, not one OpenCode could have made", got)
	}
	if got := sent.Header.Get("x-opencode-request"); !strings.HasPrefix(got, "msg_") {
		t.Fatalf("x-opencode-request is %q", got)
	}
	var body map[string]any
	if err := json.Unmarshal(sentBody, &body); err != nil {
		t.Fatal(err)
	}
	if body["stream"] != true {
		t.Fatalf("the request is not streamed: %v", body["stream"])
	}
	options, _ := body["stream_options"].(map[string]any)
	if options["include_usage"] != true {
		t.Fatalf("usage was not asked for: %v", body["stream_options"])
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 5 {
		t.Fatalf("the request carries %d tools: %v", len(tools), tools)
	}
	if _, ok := body["messages"].([]any); !ok {
		t.Fatalf("the messages were dropped: %v", body["messages"])
	}

	guarded := recorder.Body.String()
	if !strings.Contains(guarded, `"name":"read"`) {
		t.Fatalf("the model's call was not renamed onto the agent's tool:\n%s", guarded)
	}
	if strings.Contains(guarded, `"name":"Read"`) {
		t.Fatalf("the model's own name reached the client:\n%s", guarded)
	}
}

func TestInferenceAnswers404ForAModelThatIsNotFree(t *testing.T) {
	provider := newTestProvider()
	// Nothing is reachable: a model that isn't in the bootstrap list can't be
	// discovered either, so the request is answered as not free.
	provider.SetTransport(roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("offline")
	}))
	instance := New(provider, logging.New("error"))
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`))
	recorder := httptest.NewRecorder()
	instance.handleInference("chat").ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("a model that isn't free got HTTP %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "not available as an OpenCode Zen free model") ||
		!strings.Contains(recorder.Body.String(), "big-pickle") {
		t.Fatalf("the 404 does not name the free models: %s", recorder.Body.String())
	}
}
