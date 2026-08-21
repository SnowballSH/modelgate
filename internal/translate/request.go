// Package translate converts between the OpenAI Chat Completions wire
// format and the Anthropic Messages wire format, in both directions,
// including SSE stream events.
package translate

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/SnowballSH/modelgate/internal/anthro"
	"github.com/SnowballSH/modelgate/internal/oai"
)

var defaultInputSchema = json.RawMessage(`{"type":"object"}`)

func ToAnthropic(req oai.ChatRequest, providerModel string, defaultMaxTokens int) (anthro.MessagesRequest, error) {
	out := anthro.MessagesRequest{
		Model:       providerModel,
		MaxTokens:   defaultMaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      req.Stream,
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
		return anthro.MessagesRequest{}, fmt.Errorf("no messages after system extraction")
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
		case "system":
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
	var blocks []anthro.ContentBlock
	if len(msg.Content) > 0 {
		text, err := contentText(msg.Content)
		if err != nil {
			return anthro.Message{}, err
		}
		if text != "" {
			blocks = append(blocks, anthro.ContentBlock{Type: "text", Text: text})
		}
	}
	for _, call := range msg.ToolCalls {
		if !json.Valid([]byte(call.Function.Arguments)) {
			return anthro.Message{}, fmt.Errorf("tool call %s: invalid arguments JSON", call.ID)
		}
		blocks = append(blocks, anthro.ContentBlock{
			Type:  "tool_use",
			ID:    call.ID,
			Name:  call.Function.Name,
			Input: json.RawMessage(call.Function.Arguments),
		})
	}
	return anthro.Message{Role: "assistant", Content: blocks}, nil
}

func contentBlocks(content json.RawMessage) ([]anthro.ContentBlock, error) {
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return []anthro.ContentBlock{{Type: "text", Text: s}}, nil
	}
	parts, err := textParts(content)
	if err != nil {
		return nil, err
	}
	blocks := make([]anthro.ContentBlock, len(parts))
	for i, p := range parts {
		blocks[i] = anthro.ContentBlock{Type: "text", Text: p}
	}
	return blocks, nil
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

func parseStop(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many, nil
	}
	return nil, fmt.Errorf("stop must be a string or array of strings")
}

func convertTools(tools []oai.Tool) ([]anthro.Tool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	result := make([]anthro.Tool, len(tools))
	for i, t := range tools {
		if t.Type != "function" {
			return nil, fmt.Errorf("unsupported tool type: %q", t.Type)
		}
		schema := t.Function.Parameters
		if schema == nil {
			schema = defaultInputSchema
		}
		result[i] = anthro.Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: schema,
		}
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
