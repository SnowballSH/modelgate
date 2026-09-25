// Package anthro defines the subset of the Anthropic Messages API wire
// format that modelgate speaks upstream.
package anthro

import "encoding/json"

// MessagesRequest.CacheControl is the request-level automatic breakpoint: the
// API places it on the last cacheable block and moves it forward as the
// conversation grows.
type MessagesRequest struct {
	Model         string        `json:"model"`
	MaxTokens     int           `json:"max_tokens"`
	System        []TextBlock   `json:"system,omitempty"`
	Messages      []Message     `json:"messages"`
	Temperature   *float64      `json:"temperature,omitempty"`
	TopP          *float64      `json:"top_p,omitempty"`
	StopSequences []string      `json:"stop_sequences,omitempty"`
	Stream        bool          `json:"stream,omitempty"`
	Tools         []Tool        `json:"tools,omitempty"`
	ToolChoice    *ToolChoice   `json:"tool_choice,omitempty"`
	OutputConfig  *OutputConfig `json:"output_config,omitempty"`
	Metadata      *Metadata     `json:"metadata,omitempty"`
	CacheControl  *CacheControl `json:"cache_control,omitempty"`
}

// CacheControl.Type is always "ephemeral", the five-minute prompt cache.
type CacheControl struct {
	Type string `json:"type"`
}

func Ephemeral() *CacheControl {
	return &CacheControl{Type: "ephemeral"}
}

type TextBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

type Metadata struct {
	UserID string `json:"user_id,omitempty"`
}

type OutputConfig struct {
	Effort string `json:"effort,omitempty"`
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
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl *CacheControl   `json:"cache_control,omitempty"`
}

// ToolChoice type is one of "auto", "any", "tool" (with Name), or "none";
// DisableParallelToolUse applies to every type but "none".
type ToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
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
// content_block_delta carries Index and Delta; content_block_stop carries
// Index; message_delta carries
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
