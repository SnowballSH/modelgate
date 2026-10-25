package translate

import (
	"strings"

	"github.com/SnowballSH/modelgate/internal/anthro"
	"github.com/SnowballSH/modelgate/internal/oai"
	"github.com/SnowballSH/modelgate/internal/store"
)

func FromAnthropic(resp anthro.MessagesResponse, publicModel string, created int64, id string) oai.ChatResponse {
	var text strings.Builder
	hasText := false
	var toolCalls []oai.ToolCall
	var cited citationIndex
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			hasText = true
			text.WriteString(block.Text)
			markers, _ := cited.cite(anthropicSources(block.Citations))
			text.WriteString(markers)
		case "tool_use":
			toolCalls = append(toolCalls, oai.ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: oai.FunctionCall{
					Name:      block.Name,
					Arguments: normalizedArguments(string(block.Input)),
				},
			})
		}
	}

	var content *string
	if hasText || len(toolCalls) == 0 {
		s := text.String()
		content = &s
	}

	return oai.ChatResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   publicModel,
		Choices: []oai.Choice{{
			Index: 0,
			Message: oai.ResponseMessage{
				Role:        "assistant",
				Content:     content,
				ToolCalls:   toolCalls,
				Annotations: cited.annotations(),
			},
			FinishReason: FinishReason(resp.StopReason),
		}},
		Usage: OAIUsage(resp.Usage),
	}
}

// FinishReason maps a stop reason. pause_turn, a server-side search loop
// stopped before the answer was done, reads as length: the answer is
// unfinished, and modelgate does not continue the turn.
func FinishReason(stopReason string) string {
	switch stopReason {
	case "max_tokens", "model_context_window_exceeded", "pause_turn":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}

func ToStoreUsage(u anthro.Usage) store.Usage {
	usage := store.Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
	if u.ServerToolUse != nil {
		usage.WebSearchRequests = max(u.ServerToolUse.WebSearchRequests, 0)
	}
	return usage
}

func OAIUsage(u anthro.Usage) oai.Usage {
	prompt := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	usage := oai.Usage{
		PromptTokens:     prompt,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      prompt + u.OutputTokens,
	}
	if u.CacheReadInputTokens > 0 {
		usage.PromptTokensDetails = &oai.PromptTokensDetails{CachedTokens: u.CacheReadInputTokens}
	}
	return usage
}
