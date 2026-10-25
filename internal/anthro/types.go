// Package anthro defines the subset of the Anthropic Messages API wire
// format that modelgate speaks upstream.
package anthro

import (
	"encoding/json"
	"fmt"
)

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

// CacheControl.Type is always "ephemeral". An empty TTL is the five-minute
// prompt cache; "1h" asks for the one-hour cache, whose writes cost more.
type CacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

func Ephemeral(ttl string) *CacheControl {
	return &CacheControl{Type: "ephemeral", TTL: ttl}
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

// ContentBlock covers the text, image, tool_use, tool_result,
// server_tool_use and web_search_tool_result block shapes; which fields are
// set depends on Type. Content is a tool_result's text; a block whose content
// is not a string, such as a web_search_tool_result's result array or error
// object, decodes it into ResultContent instead. Citations are the sources a
// text block cites.
type ContentBlock struct {
	Type          string          `json:"type"`
	Text          string          `json:"text,omitempty"`
	Source        *ImageSource    `json:"source,omitempty"`
	ID            string          `json:"id,omitempty"`
	Name          string          `json:"name,omitempty"`
	Input         json.RawMessage `json:"input,omitempty"`
	ToolUseID     string          `json:"tool_use_id,omitempty"`
	Content       string          `json:"content,omitempty"`
	IsError       bool            `json:"is_error,omitempty"`
	Citations     []Citation      `json:"citations,omitempty"`
	ResultContent json.RawMessage `json:"-"`
}

func (b *ContentBlock) UnmarshalJSON(data []byte) error {
	type block ContentBlock
	var wire struct {
		block
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*b = ContentBlock(wire.block)
	if len(wire.Content) == 0 || string(wire.Content) == "null" {
		return nil
	}
	if err := json.Unmarshal(wire.Content, &b.Content); err != nil {
		b.ResultContent = wire.Content
	}
	return nil
}

// Citation is one source a text block cites. Web search citations are
// web_search_result_location entries carrying the source's URL and title.
type Citation struct {
	Type           string `json:"type"`
	URL            string `json:"url,omitempty"`
	Title          string `json:"title,omitempty"`
	CitedText      string `json:"cited_text,omitempty"`
	EncryptedIndex string `json:"encrypted_index,omitempty"`
}

// WebSearchResult is a web_search_tool_result's content: the results of a
// search that ran, or the error code of one that did not.
type WebSearchResult struct {
	Results   []WebSearchResultItem
	ErrorCode string
}

type WebSearchResultItem struct {
	Type    string `json:"type"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	PageAge string `json:"page_age,omitempty"`
}

// DecodeWebSearchResult reads a web_search_tool_result's content, which is a
// list of results when the search ran and a single error object when it did
// not.
func DecodeWebSearchResult(content json.RawMessage) (WebSearchResult, error) {
	var items []WebSearchResultItem
	if err := json.Unmarshal(content, &items); err == nil {
		return WebSearchResult{Results: items}, nil
	}
	var failure struct {
		Type      string `json:"type"`
		ErrorCode string `json:"error_code"`
	}
	if err := json.Unmarshal(content, &failure); err != nil || failure.Type != "web_search_tool_result_error" {
		return WebSearchResult{}, fmt.Errorf("web_search_tool_result content is neither a result list nor an error: %.80s", content)
	}
	return WebSearchResult{ErrorCode: failure.ErrorCode}, nil
}

// ImageSource.Type is "base64" (with MediaType and Data) or "url" (with URL).
type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// Tool is a client tool, which carries a name, description and input
// schema, or a server tool, which carries its Type and that tool's settings.
type Tool struct {
	Type         string          `json:"type,omitempty"`
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	CacheControl *CacheControl   `json:"cache_control,omitempty"`
	MaxUses      int             `json:"max_uses,omitempty"`
	UserLocation *UserLocation   `json:"user_location,omitempty"`
}

// UserLocation localises a web search; Type is always "approximate".
type UserLocation struct {
	Type     string `json:"type"`
	City     string `json:"city,omitempty"`
	Region   string `json:"region,omitempty"`
	Country  string `json:"country,omitempty"`
	Timezone string `json:"timezone,omitempty"`
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
	InputTokens              int64          `json:"input_tokens"`
	OutputTokens             int64          `json:"output_tokens"`
	CacheCreationInputTokens int64          `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64          `json:"cache_read_input_tokens"`
	ServerToolUse            *ServerToolUse `json:"server_tool_use,omitempty"`
}

// ServerToolUse counts the server tool calls a response was billed for.
type ServerToolUse struct {
	WebSearchRequests int64 `json:"web_search_requests"`
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

// StreamDelta.Citation carries a citations_delta's one citation, added to
// the text block the event indexes.
type StreamDelta struct {
	Type        string    `json:"type"`
	Text        string    `json:"text,omitempty"`
	PartialJSON string    `json:"partial_json,omitempty"`
	StopReason  string    `json:"stop_reason,omitempty"`
	Citation    *Citation `json:"citation,omitempty"`
}

type APIError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}
