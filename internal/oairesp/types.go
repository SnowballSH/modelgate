// Package oairesp defines the subset of the OpenAI Responses API wire
// format that modelgate speaks upstream.
package oairesp

import "encoding/json"

// Request carries temperature and top_p only for a model whose table row
// declares sampling: true; reasoning models accept only the default of either.
type Request struct {
	Model             string          `json:"model"`
	Instructions      string          `json:"instructions,omitempty"`
	Input             []Item          `json:"input"`
	Tools             []Tool          `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	MaxToolCalls      *int            `json:"max_tool_calls,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	Reasoning         *Reasoning      `json:"reasoning,omitempty"`
	MaxOutputTokens   *int            `json:"max_output_tokens,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	Text              *TextConfig     `json:"text,omitempty"`
	Store             bool            `json:"store"`
	Stream            bool            `json:"stream,omitempty"`
	User              string          `json:"user,omitempty"`
	PromptCacheKey    string          `json:"prompt_cache_key,omitempty"`
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
	// Action is what a web_search_call did: a search, an open_page or a
	// find_in_page.
	Action json.RawMessage `json:"action,omitempty"`
	Text   string          `json:"-"`
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

// ContentPart.Type is input_text or input_image for user parts, output_text
// for assistant parts, and refusal for a declined one, which carries its
// explanation in Refusal and leaves Text empty. An input_image carries
// ImageURL (an https URL or a data: URI) and no text key at all.
// Annotations, on an output_text part, are the sources the text cites.
type ContentPart struct {
	Type        string       `json:"type"`
	Text        string       `json:"text"`
	Refusal     string       `json:"refusal,omitempty"`
	ImageURL    string       `json:"image_url,omitempty"`
	Detail      string       `json:"detail,omitempty"`
	Annotations []Annotation `json:"annotations,omitempty"`
}

// Annotation is an output_text annotation; a url_citation carries the cited
// page's URL and title and the span of text that cites it.
type Annotation struct {
	Type       string `json:"type"`
	URL        string `json:"url,omitempty"`
	Title      string `json:"title,omitempty"`
	StartIndex int    `json:"start_index,omitempty"`
	EndIndex   int    `json:"end_index,omitempty"`
}

func (p ContentPart) MarshalJSON() ([]byte, error) {
	if p.Type != "input_image" {
		type part ContentPart
		return json.Marshal(part(p))
	}
	return json.Marshal(struct {
		Type     string `json:"type"`
		ImageURL string `json:"image_url"`
		Detail   string `json:"detail,omitempty"`
	}{Type: p.Type, ImageURL: p.ImageURL, Detail: p.Detail})
}

// Tool is a function tool or the built-in web_search tool. Strict carries no
// omitempty on a function tool: the Responses API defaults it to true and
// would refuse a non-strict schema sent without it. The web_search tool
// carries only its own settings.
type Tool struct {
	Type              string          `json:"type"`
	Name              string          `json:"name"`
	Description       string          `json:"description,omitempty"`
	Parameters        json.RawMessage `json:"parameters,omitempty"`
	Strict            bool            `json:"strict"`
	SearchContextSize string          `json:"-"`
	UserLocation      *UserLocation   `json:"-"`
}

const WebSearchToolType = "web_search"

func (t Tool) MarshalJSON() ([]byte, error) {
	if t.Type != WebSearchToolType {
		type tool Tool
		return json.Marshal(tool(t))
	}
	return json.Marshal(struct {
		Type              string        `json:"type"`
		SearchContextSize string        `json:"search_context_size,omitempty"`
		UserLocation      *UserLocation `json:"user_location,omitempty"`
	}{Type: t.Type, SearchContextSize: t.SearchContextSize, UserLocation: t.UserLocation})
}

// UserLocation localises a web search; Type is always "approximate", and a
// location with no other field opts out of the provider's default country.
type UserLocation struct {
	Type     string `json:"type"`
	City     string `json:"city,omitempty"`
	Region   string `json:"region,omitempty"`
	Country  string `json:"country,omitempty"`
	Timezone string `json:"timezone,omitempty"`
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

// InputTokenDetails splits input_tokens: CachedTokens were read from the
// prompt cache and CacheWriteTokens written to it; the rest is plain input.
type InputTokenDetails struct {
	CachedTokens     int64 `json:"cached_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
}

type OutputTokenDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// StreamEvent is one SSE event. Type is the event name (response.created,
// response.output_item.added, response.output_text.delta,
// response.refusal.delta, response.refusal.done,
// response.function_call_arguments.delta,
// response.function_call_arguments.done, response.output_item.done,
// response.completed, response.incomplete, response.failed, error,
// response.output_text.annotation.added, response.output_text.done); the
// populated fields follow the type. Refusal carries the completed text on
// response.refusal.done, whose incremental text arrives in Delta.
type StreamEvent struct {
	Type         string      `json:"type"`
	Response     *Response   `json:"response,omitempty"`
	OutputIndex  int         `json:"output_index,omitempty"`
	ContentIndex int         `json:"content_index,omitempty"`
	Annotation   *Annotation `json:"annotation,omitempty"`
	Item         *Item       `json:"item,omitempty"`
	ItemID       string      `json:"item_id,omitempty"`
	Delta        string      `json:"delta,omitempty"`
	Arguments    string      `json:"arguments,omitempty"`
	Refusal      string      `json:"refusal,omitempty"`
	Code         string      `json:"code,omitempty"`
	Message      string      `json:"message,omitempty"`
}
