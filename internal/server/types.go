package server

import "encoding/json/jsontext"

// Wire types for the three request/response formats the server speaks.
//
// Requests are forwarded as decoded JSON (see request.go) so nothing an agent
// sends is lost; replies and stream events are handled with the typed shapes
// below, which is what the reply side of the guard and the collapser need.

type apiError struct {
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// ---------------------------------------------------------------- OpenAI chat

type chatToolCall struct {
	Index    *int           `json:"index,omitempty"`
	ID       string         `json:"id,omitempty"`
	Type     string         `json:"type,omitempty"`
	Function chatToolCallFn `json:"function"`
}

type chatToolCallFn struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type chatFunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// content is a string, a list of parts, or null; the guard appends a note to
// whichever it is, exactly as the original does.
type chatMessage struct {
	Role             string            `json:"role,omitempty"`
	Content          any               `json:"content,omitempty"`
	Refusal          string            `json:"refusal,omitempty"`
	Reasoning        string            `json:"reasoning,omitempty"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	ToolCalls        []chatToolCall    `json:"tool_calls,omitempty"`
	FunctionCall     *chatFunctionCall `json:"function_call,omitempty"`
}

type chatDelta struct {
	Role             string            `json:"role,omitempty"`
	Content          *string           `json:"content,omitempty"`
	Refusal          *string           `json:"refusal,omitempty"`
	Reasoning        *string           `json:"reasoning,omitempty"`
	ReasoningContent *string           `json:"reasoning_content,omitempty"`
	ToolCalls        []chatToolCall    `json:"tool_calls,omitempty"`
	FunctionCall     *chatFunctionCall `json:"function_call,omitempty"`
}

func (d *chatDelta) empty() bool {
	if d == nil {
		return true
	}
	return d.Role == "" && d.Content == nil && d.Refusal == nil && d.Reasoning == nil &&
		d.ReasoningContent == nil && len(d.ToolCalls) == 0 && d.FunctionCall == nil
}

type chatChoice struct {
	Index        int            `json:"index"`
	Message      *chatMessage   `json:"message,omitempty"`
	Delta        *chatDelta     `json:"delta,omitempty"`
	FinishReason *string        `json:"finish_reason,omitempty"`
	Logprobs     jsontext.Value `json:"logprobs,omitempty"`
}

func (c *chatChoice) kept() bool {
	return c.FinishReason != nil || !c.Delta.empty() || c.Message != nil || len(c.Logprobs) > 0
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chatCompletion struct {
	ID                string       `json:"id,omitempty"`
	Object            string       `json:"object,omitempty"`
	Created           int64        `json:"created,omitzero"`
	Model             string       `json:"model,omitempty"`
	SystemFingerprint string       `json:"system_fingerprint,omitempty"`
	ServiceTier       string       `json:"service_tier,omitempty"`
	Choices           []chatChoice `json:"choices,omitzero"`
	Usage             *chatUsage   `json:"usage,omitempty"`
	Error             *apiError    `json:"error,omitempty"`
}

func (c *chatCompletion) finishReason(value string) *string {
	reason := value
	return &reason
}

// ------------------------------------------------------------ OpenAI responses

type responsePart struct {
	Type        string `json:"type,omitempty"`
	Text        string `json:"text,omitempty"`
	Refusal     string `json:"refusal,omitempty"`
	Annotations []any  `json:"annotations,omitempty"`
}

type responseItem struct {
	ID        string         `json:"id,omitempty"`
	Type      string         `json:"type,omitempty"`
	Role      string         `json:"role,omitempty"`
	Status    string         `json:"status,omitempty"`
	Content   []responsePart `json:"content,omitempty"`
	Arguments string         `json:"arguments,omitempty"`
	Name      string         `json:"name,omitempty"`
	CallID    string         `json:"call_id,omitempty"`
	Summary   []responsePart `json:"summary,omitempty"`
}

type responseObject struct {
	ID                string         `json:"id,omitempty"`
	Object            string         `json:"object,omitempty"`
	CreatedAt         float64        `json:"created_at,omitzero"`
	Status            string         `json:"status,omitempty"`
	Model             string         `json:"model,omitempty"`
	Output            []responseItem `json:"output,omitzero"`
	Usage             jsontext.Value `json:"usage,omitempty"`
	IncompleteDetails jsontext.Value `json:"incomplete_details,omitempty"`
	FinishReason      string         `json:"finish_reason,omitempty"`
	Error             *apiError      `json:"error,omitempty"`
	ReasoningContent  string         `json:"reasoning_content,omitempty"`
}

type responsesEvent struct {
	Type              string          `json:"type,omitempty"`
	SequenceNumber    int             `json:"sequence_number,omitzero"`
	Response          *responseObject `json:"response,omitempty"`
	Item              *responseItem   `json:"item,omitempty"`
	OutputIndex       *int            `json:"output_index,omitempty"`
	ContentIndex      *int            `json:"content_index,omitempty"`
	SummaryIndex      *int            `json:"summary_index,omitempty"`
	ItemID            string          `json:"item_id,omitempty"`
	Part              *responsePart   `json:"part,omitempty"`
	Delta             string          `json:"delta,omitempty"`
	Text              string          `json:"text,omitempty"`
	Refusal           string          `json:"refusal,omitempty"`
	Arguments         string          `json:"arguments,omitempty"`
	Status            string          `json:"status,omitempty"`
	FinishReason      string          `json:"finish_reason,omitempty"`
	IncompleteDetails jsontext.Value  `json:"incomplete_details,omitempty"`
	Usage             jsontext.Value  `json:"usage,omitempty"`
	Error             *apiError       `json:"error,omitempty"`
	ID                string          `json:"id,omitempty"`
}

// ------------------------------------------------------------ Anthropic messages

type anthropicBlock struct {
	Type      string `json:"type,omitempty"`
	Text      string `json:"text,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Input     any    `json:"input,omitempty"`
}

type anthropicUsage struct {
	InputTokens              int  `json:"input_tokens"`
	OutputTokens             int  `json:"output_tokens"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens,omitempty"`
}

type anthropicMessage struct {
	Type         string           `json:"type,omitempty"`
	ID           string           `json:"id,omitempty"`
	Role         string           `json:"role,omitempty"`
	Model        string           `json:"model,omitempty"`
	Content      []anthropicBlock `json:"content"`
	StopReason   *string          `json:"stop_reason,omitempty"`
	StopSequence *string          `json:"stop_sequence,omitempty"`
	Usage        *anthropicUsage  `json:"usage,omitempty"`
}

type anthropicDelta struct {
	Type         string  `json:"type,omitempty"`
	Text         string  `json:"text,omitempty"`
	Thinking     string  `json:"thinking,omitempty"`
	Signature    string  `json:"signature,omitempty"`
	PartialJSON  string  `json:"partial_json,omitempty"`
	StopReason   *string `json:"stop_reason,omitempty"`
	StopSequence *string `json:"stop_sequence,omitempty"`
}

type anthropicEvent struct {
	Type         string            `json:"type,omitempty"`
	Message      *anthropicMessage `json:"message,omitempty"`
	Index        *int              `json:"index,omitempty"`
	ContentBlock *anthropicBlock   `json:"content_block,omitempty"`
	Delta        *anthropicDelta   `json:"delta,omitempty"`
	Usage        *anthropicUsage   `json:"usage,omitempty"`
	Error        *apiError         `json:"error,omitempty"`
}
