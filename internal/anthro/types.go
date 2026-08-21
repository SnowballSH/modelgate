// Package anthro defines the subset of the Anthropic Messages API wire
// format that modelgate speaks upstream.
package anthro

import "encoding/json"

type MessagesRequest struct {
	Model         string      `json:"model"`
	MaxTokens     int         `json:"max_tokens"`
	System        string      `json:"system,omitempty"`
	Messages      []Message   `json:"messages"`
	Temperature   *float64    `json:"temperature,omitempty"`
	TopP          *float64    `json:"top_p,omitempty"`
	StopSequences []string    `json:"stop_sequences,omitempty"`
	Stream        bool        `json:"stream,omitempty"`
	Tools         []Tool      `json:"tools,omitempty"`
	ToolChoice    *ToolChoice `json:"tool_choice,omitempty"`
}

type Message struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

// ContentBlock covers the text, tool_use, and tool_result block shapes;
// which fields are set depends on Type.
type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ToolChoice type is one of "auto", "any", "tool" (with Name), or "none".
type ToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type MessagesResponse struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Role       string         `json:"role"`
	Model      string         `json:"model"`
	Content    []ContentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      Usage          `json:"usage"`
}

type Usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// StreamEvent is one SSE event from a streamed Messages call. Which
// fields are set depends on Type: message_start carries Message;
// content_block_start carries Index and ContentBlock;
// content_block_delta carries Index and Delta; message_delta carries
// Delta (stop_reason) and Usage; error carries Error.
type StreamEvent struct {
	Type         string            `json:"type"`
	Message      *MessagesResponse `json:"message,omitempty"`
	Index        int               `json:"index,omitempty"`
	ContentBlock *ContentBlock     `json:"content_block,omitempty"`
	Delta        *StreamDelta      `json:"delta,omitempty"`
	Usage        *Usage            `json:"usage,omitempty"`
	Error        *APIError         `json:"error,omitempty"`
}

type StreamDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`
}

type APIError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}
