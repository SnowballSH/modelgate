// Package translate converts between the OpenAI Chat Completions wire
// format and the wire formats modelgate speaks upstream: the Anthropic
// Messages API, in both directions and including SSE stream events, and
// the OpenAI Responses API.
package translate

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/SnowballSH/modelgate/internal/anthro"
	"github.com/SnowballSH/modelgate/internal/oai"
)

var defaultInputSchema = json.RawMessage(`{"type":"object"}`)

// effortLevels is the reasoning_effort vocabulary both upstreams accept.
var effortLevels = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

var anthropicEfforts = map[string]string{
	"none":    "low",
	"minimal": "low",
	"low":     "low",
	"medium":  "medium",
	"high":    "high",
	"xhigh":   "xhigh",
	"max":     "max",
}

// KnownEffort canonicalises a client-supplied reasoning_effort, reporting
// false for anything outside the accepted vocabulary, so callers never carry
// an arbitrary request string into an upstream call, a metric or a log line.
func KnownEffort(level string) (string, bool) {
	canonical := strings.ToLower(level)
	if !slices.Contains(effortLevels, canonical) {
		return "", false
	}
	return canonical, true
}

func errUnknownEffort(level string) error {
	return fmt.Errorf("reasoning_effort %q is not one of %s", level, strings.Join(effortLevels, ", "))
}

func EffortForAnthropic(level string) (string, error) {
	if level == "" {
		return "", nil
	}
	canonical, ok := KnownEffort(level)
	if !ok {
		return "", errUnknownEffort(level)
	}
	return anthropicEfforts[canonical], nil
}

func ToAnthropic(req oai.ChatRequest, providerModel string, defaultMaxTokens int) (anthro.MessagesRequest, error) {
	if req.ResponseFormat != nil {
		return anthro.MessagesRequest{}, errors.New("response_format is not supported for anthropic models")
	}
	if req.N != nil && *req.N != 1 {
		return anthro.MessagesRequest{}, errors.New("n other than 1 is not supported for anthropic models")
	}
	for name, set := range map[string]bool{
		"frequency_penalty": req.FrequencyPenalty != nil,
		"presence_penalty":  req.PresencePenalty != nil,
		"seed":              req.Seed != nil,
		"logprobs":          req.Logprobs != nil && *req.Logprobs,
	} {
		if set {
			return anthro.MessagesRequest{}, fmt.Errorf("%s is not supported for anthropic models", name)
		}
	}
	effort, err := EffortForAnthropic(req.ReasoningEffort)
	if err != nil {
		return anthro.MessagesRequest{}, err
	}
	out := anthro.MessagesRequest{
		Model:     providerModel,
		MaxTokens: defaultMaxTokens,
		Stream:    req.Stream,
	}
	// Models that accept output_config.effort reject any temperature other
	// than 1.0 and any top_p below 0.99, so effort displaces both.
	if effort != "" {
		out.OutputConfig = &anthro.OutputConfig{Effort: effort}
	} else {
		out.Temperature = req.Temperature
		out.TopP = req.TopP
	}
	if req.MaxTokens != nil {
		out.MaxTokens = *req.MaxTokens
	}

	stops, err := parseStop(req.Stop)
	if err != nil {
		return anthro.MessagesRequest{}, err
	}
	out.StopSequences = stops

	tools, err := convertTools(req.Tools)
	if err != nil {
		return anthro.MessagesRequest{}, err
	}
	out.Tools = tools

	choice, err := parseToolChoice(req.ToolChoice)
	if err != nil {
		return anthro.MessagesRequest{}, err
	}
	out.ToolChoice = choice

	system, messages, err := convertMessages(req.Messages)
	if err != nil {
		return anthro.MessagesRequest{}, err
	}
	if len(messages) == 0 {
		return anthro.MessagesRequest{}, errors.New("no messages after system extraction")
	}
	out.System = system
	out.Messages = messages
	return out, nil
}

func convertMessages(messages []oai.Message) (string, []anthro.Message, error) {
	var systemParts []string
	var result []anthro.Message
	for i := 0; i < len(messages); i++ {
		msg := messages[i]
		switch msg.Role {
		case "system", "developer":
			text, err := contentText(msg.Content)
			if err != nil {
				return "", nil, err
			}
			systemParts = append(systemParts, text)
		case "user":
			blocks, err := contentBlocks(msg.Content)
			if err != nil {
				return "", nil, err
			}
			result = append(result, anthro.Message{Role: "user", Content: blocks})
		case "assistant":
			m, err := assistantMessage(msg)
			if err != nil {
				return "", nil, err
			}
			result = append(result, m)
		case "tool":
			var blocks []anthro.ContentBlock
			for ; i < len(messages) && messages[i].Role == "tool"; i++ {
				text, err := contentText(messages[i].Content)
				if err != nil {
					return "", nil, err
				}
				blocks = append(blocks, anthro.ContentBlock{
					Type:      "tool_result",
					ToolUseID: messages[i].ToolCallID,
					Content:   text,
				})
			}
			i--
			result = append(result, anthro.Message{Role: "user", Content: blocks})
		default:
			return "", nil, fmt.Errorf("unsupported message role: %q", msg.Role)
		}
	}
	return strings.Join(systemParts, "\n\n"), result, nil
}

func assistantMessage(msg oai.Message) (anthro.Message, error) {
	text, calls, err := assistantParts(msg)
	if err != nil {
		return anthro.Message{}, err
	}
	var blocks []anthro.ContentBlock
	if text != "" {
		blocks = append(blocks, anthro.ContentBlock{Type: "text", Text: text})
	}
	for _, call := range calls {
		blocks = append(blocks, anthro.ContentBlock{
			Type:  "tool_use",
			ID:    call.ID,
			Name:  call.Function.Name,
			Input: json.RawMessage(call.Function.Arguments),
		})
	}
	return anthro.Message{Role: "assistant", Content: blocks}, nil
}

// assistantParts reads an assistant message as the text and the tool calls a
// history item should carry, refusing one that would render as neither. Both
// translators build their assistant items from it, so the rules stay one rule.
func assistantParts(msg oai.Message) (string, []oai.ToolCall, error) {
	var text string
	if len(msg.Content) > 0 {
		content, err := contentText(msg.Content)
		if err != nil {
			return "", nil, err
		}
		text = content
	}
	for _, call := range msg.ToolCalls {
		if !json.Valid([]byte(call.Function.Arguments)) {
			return "", nil, fmt.Errorf("tool call %s: invalid arguments JSON", call.ID)
		}
	}
	if text == "" && len(msg.ToolCalls) == 0 {
		return "", nil, errors.New("assistant message has neither content nor tool calls")
	}
	return text, msg.ToolCalls, nil
}

func contentBlocks(content json.RawMessage) ([]anthro.ContentBlock, error) {
	texts, err := nonEmptyTexts(content)
	if err != nil {
		return nil, err
	}
	blocks := make([]anthro.ContentBlock, len(texts))
	for i, text := range texts {
		blocks[i] = anthro.ContentBlock{Type: "text", Text: text}
	}
	return blocks, nil
}

// nonEmptyTexts reads a message content field — a plain string or an array of
// text parts — as the texts a content builder should render, refusing content
// that carries no text at all. Both translators map over it.
func nonEmptyTexts(content json.RawMessage) ([]string, error) {
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		if s == "" {
			return nil, errors.New("message content is empty")
		}
		return []string{s}, nil
	}
	parts, err := textParts(content)
	if err != nil {
		return nil, err
	}
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			texts = append(texts, part)
		}
	}
	if len(texts) == 0 {
		return nil, errors.New("message content is empty")
	}
	return texts, nil
}

func contentText(content json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s, nil
	}
	parts, err := textParts(content)
	if err != nil {
		return "", err
	}
	return strings.Join(parts, ""), nil
}

func textParts(content json.RawMessage) ([]string, error) {
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		return nil, fmt.Errorf("invalid message content: %w", err)
	}
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		if p.Type != "text" {
			return nil, fmt.Errorf("unsupported content part: %s", p.Type)
		}
		texts = append(texts, p.Text)
	}
	return texts, nil
}

// parseStop reads the Chat Completions stop field. A JSON null, an empty
// string and a list that contributes no non-empty sequence all mean unset, so
// no upstream is ever asked to stop on a sequence the client did not name.
func parseStop(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return nonEmptyStops([]string{one}), nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return nonEmptyStops(many), nil
	}
	return nil, errors.New("stop must be a string or array of strings")
}

func nonEmptyStops(stops []string) []string {
	kept := slices.DeleteFunc(stops, func(s string) bool { return s == "" })
	if len(kept) == 0 {
		return nil
	}
	return kept
}

func convertTools(tools []oai.Tool) ([]anthro.Tool, error) {
	return functionTools(tools, func(fn oai.ToolFunction, schema json.RawMessage) anthro.Tool {
		return anthro.Tool{Name: fn.Name, Description: fn.Description, InputSchema: schema}
	})
}

// functionTools refuses any tool type but function and renders the rest
// through build, substituting the empty-object schema both upstreams require
// for a tool that declares no parameters.
func functionTools[T any](tools []oai.Tool, build func(fn oai.ToolFunction, schema json.RawMessage) T) ([]T, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	result := make([]T, len(tools))
	for i, t := range tools {
		if t.Type != "function" {
			return nil, fmt.Errorf("unsupported tool type: %q", t.Type)
		}
		schema := t.Function.Parameters
		if schema == nil {
			schema = defaultInputSchema
		}
		result[i] = build(t.Function, schema)
	}
	return result, nil
}

func parseToolChoice(raw json.RawMessage) (*anthro.ToolChoice, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto":
			return &anthro.ToolChoice{Type: "auto"}, nil
		case "none":
			return &anthro.ToolChoice{Type: "none"}, nil
		case "required":
			return &anthro.ToolChoice{Type: "any"}, nil
		}
		return nil, fmt.Errorf("unsupported tool_choice: %q", s)
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Type == "function" && obj.Function.Name != "" {
		return &anthro.ToolChoice{Type: "tool", Name: obj.Function.Name}, nil
	}
	return nil, fmt.Errorf("unsupported tool_choice: %s", string(raw))
}
