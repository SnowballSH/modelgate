package translate

import (
	"slices"
	"strings"

	"github.com/SnowballSH/modelgate/internal/oai"
	"github.com/SnowballSH/modelgate/internal/oairesp"
	"github.com/SnowballSH/modelgate/internal/store"
)

func FromResponses(resp oairesp.Response, publicModel string, created int64, id string) oai.ChatResponse {
	var text, refusal strings.Builder
	hasText, hasRefusal := false, false
	var toolCalls []oai.ToolCall
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				switch part.Type {
				case "output_text":
					hasText = true
					text.WriteString(part.Text)
				case "refusal":
					hasRefusal = true
					refusal.WriteString(part.Refusal)
				}
			}
		case "function_call":
			toolCalls = append(toolCalls, oai.ToolCall{
				ID:   item.CallID,
				Type: "function",
				Function: oai.FunctionCall{
					Name:      item.Name,
					Arguments: item.Arguments,
				},
			})
		}
	}

	message := oai.ResponseMessage{Role: "assistant", ToolCalls: toolCalls}
	if hasText || len(toolCalls) == 0 {
		s := text.String()
		message.Content = &s
	}
	if hasRefusal {
		s := refusal.String()
		message.Refusal = &s
	}

	return oai.ChatResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   publicModel,
		Choices: []oai.Choice{{
			Index:        0,
			Message:      message,
			FinishReason: ResponsesFinishReason(resp),
		}},
		Usage: ResponsesOAIUsage(resp.Usage),
	}
}

func ResponsesFinishReason(resp oairesp.Response) string {
	sawToolCall := slices.ContainsFunc(resp.Output, func(item oairesp.Item) bool {
		return item.Type == "function_call"
	})
	return responsesFinishReason(resp.Status, resp.IncompleteDetails, sawToolCall)
}

// responsesFinishReason tests truncation before the tool call so a response cut
// off mid-call reports length: a caller told tool_calls would dispatch a tool
// on arguments the model never finished writing.
func responsesFinishReason(status string, incomplete *oairesp.IncompleteDetails, sawToolCall bool) string {
	reason := ""
	if incomplete != nil {
		reason = incomplete.Reason
	}
	switch {
	case status == "incomplete" && reason == "max_output_tokens":
		return "length"
	case reason == "content_filter":
		return "content_filter"
	case sawToolCall:
		return "tool_calls"
	default:
		return "stop"
	}
}

// ResponsesStoreUsage clamps the cached count into [0, InputTokens], as
// storeUsageFromOAI does: a nonconforming upstream must never produce negative
// input tokens, which would corrupt spend accounting.
func ResponsesStoreUsage(u *oairesp.Usage) store.Usage {
	if u == nil {
		return store.Usage{}
	}
	cached := responsesCachedTokens(u)
	return store.Usage{
		InputTokens:     u.InputTokens - cached,
		OutputTokens:    u.OutputTokens,
		CacheReadTokens: cached,
	}
}

func ResponsesOAIUsage(u *oairesp.Usage) oai.Usage {
	if u == nil {
		return oai.Usage{}
	}
	usage := oai.Usage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.InputTokens + u.OutputTokens,
	}
	if cached := responsesCachedTokens(u); cached > 0 {
		usage.PromptTokensDetails = &oai.PromptTokensDetails{CachedTokens: cached}
	}
	if u.OutputTokensDetails != nil && u.OutputTokensDetails.ReasoningTokens > 0 {
		usage.CompletionTokensDetails = &oai.CompletionTokensDetails{ReasoningTokens: u.OutputTokensDetails.ReasoningTokens}
	}
	return usage
}

func responsesCachedTokens(u *oairesp.Usage) int64 {
	if u.InputTokensDetails == nil {
		return 0
	}
	return min(max(u.InputTokensDetails.CachedTokens, 0), u.InputTokens)
}
