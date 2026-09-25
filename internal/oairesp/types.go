// Package oairesp defines the subset of the OpenAI Responses API wire
// format that modelgate speaks upstream.
package oairesp

import "encoding/json"

// Request carries no temperature or top_p: this upstream serves reasoning
// models, which accept only the default value of either.
type Request struct {
	Model             string          `json:"model"`
	Instructions      string          `json:"instructions,omitempty"`
	Input             []Item          `json:"input"`
	Tools             []Tool          `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	Reasoning         *Reasoning      `json:"reasoning,omitempty"`
	MaxOutputTokens   *int            `json:"max_output_tokens,omitempty"`
	Text              *TextConfig     `json:"text,omitempty"`
	Store             bool            `json:"store"`
	Stream            bool            `json:"stream,omitempty"`
	User              string          `json:"user,omitempty"`
}

type Reasoning struct {
	Effort string `json:"effort"`
}

type TextConfig struct {
	Format json.RawMessage `json:"format"`
}

// Item is one input or output item. Type is one of message, function_call,
// function_call_output, reasoning; the populated fields follow the type.
// Output is a pointer because a function_call_output must carry the key even
// when the tool returned the empty string. Text holds the assistant
// string-shorthand content, which MarshalJSON emits in place of Content.
type Item struct {
	Type      string        `json:"type,omitempty"`
	ID        string        `json:"id,omitempty"`
	Role      string        `json:"role,omitempty"`
	Content   []ContentPart `json:"content,omitempty"`
	CallID    string        `json:"call_id,omitempty"`
	Name      string        `json:"name,omitempty"`
	Arguments string        `json:"arguments,omitempty"`
	Output    *string       `json:"output,omitempty"`
	Status    string        `json:"status,omitempty"`
	Text      string        `json:"-"`
}

func (i Item) MarshalJSON() ([]byte, error) {
	type item Item
	if i.Text == "" {
		return json.Marshal(item(i))
	}
	return json.Marshal(struct {
		item
		Content string `json:"content"`
	}{item: item(i), Content: i.Text})
}

// ContentPart.Type is input_text for user parts, output_text for assistant
// parts, and refusal for a declined one, which carries its explanation in
// Refusal and leaves Text empty.
type ContentPart struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal,omitempty"`
}

// Tool.Strict carries no omitempty: the Responses API defaults it to true and
// would refuse a non-strict schema sent without it.
type Tool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict"`
}

type Response struct {
	ID                string             `json:"id"`
	Object            string             `json:"object"`
	CreatedAt         int64              `json:"created_at"`
	Status            string             `json:"status"`
	IncompleteDetails *IncompleteDetails `json:"incomplete_details,omitempty"`
	Error             *APIError          `json:"error,omitempty"`
	Model             string             `json:"model"`
	Output            []Item             `json:"output"`
	Usage             *Usage             `json:"usage,omitempty"`
}

type IncompleteDetails struct {
	Reason string `json:"reason"`
}

type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Usage struct {
	InputTokens         int64               `json:"input_tokens"`
	InputTokensDetails  *InputTokenDetails  `json:"input_tokens_details,omitempty"`
	OutputTokens        int64               `json:"output_tokens"`
	OutputTokensDetails *OutputTokenDetails `json:"output_tokens_details,omitempty"`
	TotalTokens         int64               `json:"total_tokens"`
}

// InputTokenDetails reports cached input tokens; the Responses usage object
// has no cache-write count to carry.
type InputTokenDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

type OutputTokenDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// StreamEvent is one SSE event. Type is the event name (response.created,
// response.output_item.added, response.output_text.delta,
// response.refusal.delta, response.refusal.done,
// response.function_call_arguments.delta,
// response.function_call_arguments.done, response.output_item.done,
// response.completed, response.incomplete, response.failed, error); the
// populated fields follow the type. Refusal carries the completed text on
// response.refusal.done, whose incremental text arrives in Delta.
type StreamEvent struct {
	Type        string    `json:"type"`
	Response    *Response `json:"response,omitempty"`
	OutputIndex int       `json:"output_index,omitempty"`
	Item        *Item     `json:"item,omitempty"`
	ItemID      string    `json:"item_id,omitempty"`
	Delta       string    `json:"delta,omitempty"`
	Arguments   string    `json:"arguments,omitempty"`
	Refusal     string    `json:"refusal,omitempty"`
	Code        string    `json:"code,omitempty"`
	Message     string    `json:"message,omitempty"`
}
