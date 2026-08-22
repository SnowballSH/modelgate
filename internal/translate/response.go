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
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			hasText = true
			text.WriteString(block.Text)
		case "tool_use":
			toolCalls = append(toolCalls, oai.ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: oai.FunctionCall{
					Name:      block.Name,
					Arguments: string(block.Input),
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
				Role:      "assistant",
				Content:   content,
				ToolCalls: toolCalls,
			},
			FinishReason: FinishReason(resp.StopReason),
		}},
		Usage: OAIUsage(resp.Usage),
	}
}

func FinishReason(stopReason string) string {
	switch stopReason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

func ToStoreUsage(u anthro.Usage) store.Usage {
	return store.Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
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
