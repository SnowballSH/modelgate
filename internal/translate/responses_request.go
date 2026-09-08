package translate

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/SnowballSH/modelgate/internal/oai"
	"github.com/SnowballSH/modelgate/internal/oairesp"
)

var responsesEfforts = map[string]bool{
	"none":    true,
	"minimal": true,
	"low":     true,
	"medium":  true,
	"high":    true,
	"xhigh":   true,
	"max":     true,
}

// ToResponses builds a Responses API request from a Chat Completions one.
// temperature and top_p are normalised away rather than refused: this upstream
// serves reasoning models, which accept only the default value of either.
func ToResponses(req oai.ChatRequest, providerModel string, defaultMaxTokens int) (oairesp.Request, error) {
	if req.N != nil && *req.N != 1 {
		return oairesp.Request{}, errors.New("n other than 1 is not supported on the responses upstream")
	}
	if req.Logprobs != nil && *req.Logprobs {
		return oairesp.Request{}, errors.New("logprobs is not supported on the responses upstream")
	}
	for name, set := range map[string]bool{
		"stop":              len(req.Stop) > 0 && string(req.Stop) != "null",
		"frequency_penalty": req.FrequencyPenalty != nil,
		"presence_penalty":  req.PresencePenalty != nil,
		"seed":              req.Seed != nil,
	} {
		if set {
			return oairesp.Request{}, fmt.Errorf("%s is not supported on the responses upstream", name)
		}
	}

	maxOutputTokens := defaultMaxTokens
	switch {
	case req.MaxCompletionTokens != nil:
		maxOutputTokens = *req.MaxCompletionTokens
	case req.MaxTokens != nil:
		maxOutputTokens = *req.MaxTokens
	}
	out := oairesp.Request{
		Model:             providerModel,
		ParallelToolCalls: req.ParallelToolCalls,
		MaxOutputTokens:   &maxOutputTokens,
		Store:             false,
		Stream:            req.Stream,
	}

	if req.ReasoningEffort != "" {
		level := strings.ToLower(req.ReasoningEffort)
		if !responsesEfforts[level] {
			return oairesp.Request{}, fmt.Errorf("reasoning_effort %q is not one of none, minimal, low, medium, high, xhigh, max", req.ReasoningEffort)
		}
		out.Reasoning = &oairesp.Reasoning{Effort: level}
	}

	instructions, items, err := responsesInput(req.Messages)
	if err != nil {
		return oairesp.Request{}, err
	}
	out.Instructions, out.Input = instructions, items

	tools, err := responsesTools(req.Tools)
	if err != nil {
		return oairesp.Request{}, err
	}
	out.Tools = tools

	choice, err := responsesToolChoice(req.ToolChoice)
	if err != nil {
		return oairesp.Request{}, err
	}
	out.ToolChoice = choice

	if req.ResponseFormat != nil {
		format, err := responsesTextFormat(req.ResponseFormat)
		if err != nil {
			return oairesp.Request{}, err
		}
		out.Text = &oairesp.TextConfig{Format: format}
	}
	return out, nil
}

// responsesInput folds system and developer messages into instructions and
// renders the rest as input items. Assistant tool calls go back without their
// ids: a function_call carrying an id but no paired reasoning item is refused
// upstream, and a Chat Completions history cannot carry reasoning items.
func responsesInput(messages []oai.Message) (string, []oairesp.Item, error) {
	var instructions []string
	var items []oairesp.Item
	for _, msg := range messages {
		switch msg.Role {
		case "system", "developer":
			text, err := contentText(msg.Content)
			if err != nil {
				return "", nil, err
			}
			instructions = append(instructions, text)
		case "user":
			parts, err := responsesContentParts(msg.Content)
			if err != nil {
				return "", nil, err
			}
			items = append(items, oairesp.Item{Type: "message", Role: "user", Content: parts})
		case "assistant":
			assistant, err := responsesAssistantItems(msg)
			if err != nil {
				return "", nil, err
			}
			items = append(items, assistant...)
		case "tool":
			if msg.ToolCallID == "" {
				return "", nil, errors.New("tool message has no tool_call_id")
			}
			text, err := contentText(msg.Content)
			if err != nil {
				return "", nil, err
			}
			items = append(items, oairesp.Item{Type: "function_call_output", CallID: msg.ToolCallID, Output: &text})
		default:
			return "", nil, fmt.Errorf("unsupported message role: %q", msg.Role)
		}
	}
	if len(items) == 0 {
		return "", nil, errors.New("no messages after instruction extraction")
	}
	return strings.Join(instructions, "\n\n"), items, nil
}

func responsesAssistantItems(msg oai.Message) ([]oairesp.Item, error) {
	var items []oairesp.Item
	if len(msg.Content) > 0 {
		text, err := contentText(msg.Content)
		if err != nil {
			return nil, err
		}
		if text != "" {
			items = append(items, oairesp.Item{Role: "assistant", Text: text})
		}
	}
	for _, call := range msg.ToolCalls {
		if !json.Valid([]byte(call.Function.Arguments)) {
			return nil, fmt.Errorf("tool call %s: invalid arguments JSON", call.ID)
		}
		items = append(items, oairesp.Item{
			Type:      "function_call",
			CallID:    call.ID,
			Name:      call.Function.Name,
			Arguments: call.Function.Arguments,
		})
	}
	if len(items) == 0 {
		return nil, errors.New("assistant message has neither content nor tool calls")
	}
	return items, nil
}

func responsesContentParts(content json.RawMessage) ([]oairesp.ContentPart, error) {
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		if s == "" {
			return nil, errors.New("message content is empty")
		}
		return []oairesp.ContentPart{{Type: "input_text", Text: s}}, nil
	}
	texts, err := textParts(content)
	if err != nil {
		return nil, err
	}
	var parts []oairesp.ContentPart
	for _, text := range texts {
		if text == "" {
			continue
		}
		parts = append(parts, oairesp.ContentPart{Type: "input_text", Text: text})
	}
	if len(parts) == 0 {
		return nil, errors.New("message content is empty")
	}
	return parts, nil
}

func responsesTools(tools []oai.Tool) ([]oairesp.Tool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	result := make([]oairesp.Tool, len(tools))
	for i, t := range tools {
		if t.Type != "function" {
			return nil, fmt.Errorf("unsupported tool type: %q", t.Type)
		}
		schema := t.Function.Parameters
		if schema == nil {
			schema = defaultInputSchema
		}
		result[i] = oairesp.Tool{
			Type:        "function",
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  schema,
			Strict:      false,
		}
	}
	return result, nil
}

func responsesToolChoice(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto", "none", "required":
			return json.Marshal(s)
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
		return json.Marshal(struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}{Type: "function", Name: obj.Function.Name})
	}
	return nil, fmt.Errorf("unsupported tool_choice: %s", string(raw))
}

func responsesTextFormat(raw json.RawMessage) (json.RawMessage, error) {
	var obj struct {
		Type       string `json:"type"`
		JSONSchema *struct {
			Name   string          `json:"name"`
			Schema json.RawMessage `json:"schema"`
			Strict bool            `json:"strict"`
		} `json:"json_schema"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("unsupported response_format: %s", string(raw))
	}
	switch obj.Type {
	case "text", "json_object":
		return json.Marshal(struct {
			Type string `json:"type"`
		}{Type: obj.Type})
	case "json_schema":
		if obj.JSONSchema == nil || obj.JSONSchema.Name == "" || obj.JSONSchema.Schema == nil {
			return nil, errors.New("response_format json_schema requires a name and a schema")
		}
		return json.Marshal(struct {
			Type   string          `json:"type"`
			Name   string          `json:"name"`
			Schema json.RawMessage `json:"schema"`
			Strict bool            `json:"strict"`
		}{Type: "json_schema", Name: obj.JSONSchema.Name, Schema: obj.JSONSchema.Schema, Strict: obj.JSONSchema.Strict})
	}
	return nil, fmt.Errorf("unsupported response_format: %s", string(raw))
}
