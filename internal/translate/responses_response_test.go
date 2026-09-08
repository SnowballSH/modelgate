package translate

import (
	"testing"

	"github.com/SnowballSH/modelgate/internal/oairesp"
	"github.com/SnowballSH/modelgate/internal/store"
)

func responsesUsageFixture() *oairesp.Usage {
	return &oairesp.Usage{
		InputTokens:         100,
		InputTokensDetails:  &oairesp.InputTokenDetails{CachedTokens: 40},
		OutputTokens:        50,
		OutputTokensDetails: &oairesp.OutputTokenDetails{ReasoningTokens: 30},
		TotalTokens:         150,
	}
}

func TestFromResponsesToolCall(t *testing.T) {
	resp := oairesp.Response{
		ID:     "resp_1",
		Object: "response",
		Status: "completed",
		Model:  "gpt-real",
		Output: []oairesp.Item{
			{Type: "reasoning", ID: "rs_1"},
			{Type: "message", ID: "msg_1", Role: "assistant", Content: []oairesp.ContentPart{{Type: "output_text", Text: "Hello"}}},
			{Type: "function_call", ID: "fc_1", CallID: "call_1", Name: "get_weather", Arguments: `{"city":"Paris"}`},
		},
		Usage: responsesUsageFixture(),
	}
	got := FromResponses(resp, "gpt-proxy", 1700000000, "chatcmpl-abc")

	if got.ID != "chatcmpl-abc" || got.Object != "chat.completion" || got.Created != 1700000000 || got.Model != "gpt-proxy" {
		t.Errorf("envelope mismatch: %+v", got)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(got.Choices))
	}
	c := got.Choices[0]
	if c.Index != 0 || c.FinishReason != "tool_calls" {
		t.Errorf("choice = %+v", c)
	}
	if c.Message.Role != "assistant" || c.Message.Content == nil || *c.Message.Content != "Hello" {
		t.Errorf("message = %+v", c.Message)
	}
	if c.Message.Refusal != nil {
		t.Errorf("refusal = %q, want nil", *c.Message.Refusal)
	}
	if len(c.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(c.Message.ToolCalls))
	}
	tc := c.Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Type != "function" || tc.Function.Name != "get_weather" || tc.Function.Arguments != `{"city":"Paris"}` {
		t.Errorf("tool call = %+v", tc)
	}

	u := got.Usage
	if u.PromptTokens != 100 || u.CompletionTokens != 50 || u.TotalTokens != 150 {
		t.Errorf("usage = %+v", u)
	}
	if u.PromptTokensDetails == nil || u.PromptTokensDetails.CachedTokens != 40 {
		t.Errorf("prompt_tokens_details = %+v", u.PromptTokensDetails)
	}
	if u.CompletionTokensDetails == nil || u.CompletionTokensDetails.ReasoningTokens != 30 {
		t.Errorf("completion_tokens_details = %+v", u.CompletionTokensDetails)
	}
}

func TestFromResponsesToolCallWithoutText(t *testing.T) {
	resp := oairesp.Response{
		Status: "completed",
		Output: []oairesp.Item{
			{Type: "function_call", CallID: "call_1", Name: "get_weather", Arguments: "{}"},
		},
	}
	got := FromResponses(resp, "gpt-proxy", 1, "id").Choices[0].Message
	if got.Content != nil {
		t.Errorf("content = %q, want nil", *got.Content)
	}
}

func TestFromResponsesTextOnly(t *testing.T) {
	resp := oairesp.Response{
		Status: "completed",
		Output: []oairesp.Item{
			{Type: "message", Role: "assistant", Content: []oairesp.ContentPart{
				{Type: "output_text", Text: "Hello "},
				{Type: "output_text", Text: "world"},
			}},
		},
	}
	got := FromResponses(resp, "gpt-proxy", 1, "id").Choices[0]
	if got.FinishReason != "stop" {
		t.Errorf("finish_reason = %q", got.FinishReason)
	}
	if got.Message.Content == nil || *got.Message.Content != "Hello world" {
		t.Errorf("content = %v", got.Message.Content)
	}
}

func TestFromResponsesCarriesRefusal(t *testing.T) {
	resp := oairesp.Response{
		Status: "completed",
		Output: []oairesp.Item{
			{Type: "message", Role: "assistant", Content: []oairesp.ContentPart{
				{Type: "refusal", Refusal: "I can't "},
				{Type: "refusal", Refusal: "help with that."},
			}},
		},
	}
	got := FromResponses(resp, "gpt-proxy", 1, "id").Choices[0].Message
	if got.Refusal == nil || *got.Refusal != "I can't help with that." {
		t.Fatalf("refusal = %v", got.Refusal)
	}
}

func TestFromResponsesIncompleteIsLength(t *testing.T) {
	resp := oairesp.Response{
		Status:            "incomplete",
		IncompleteDetails: &oairesp.IncompleteDetails{Reason: "max_output_tokens"},
		Output: []oairesp.Item{
			{Type: "message", Role: "assistant", Content: []oairesp.ContentPart{{Type: "output_text", Text: "partial"}}},
		},
	}
	if got := FromResponses(resp, "gpt-proxy", 1, "id").Choices[0].FinishReason; got != "length" {
		t.Errorf("finish_reason = %q, want length", got)
	}
}

func TestFromResponsesContentFilterFinishReason(t *testing.T) {
	resp := oairesp.Response{
		Status:            "incomplete",
		IncompleteDetails: &oairesp.IncompleteDetails{Reason: "content_filter"},
		Output:            []oairesp.Item{{Type: "message", Role: "assistant"}},
	}
	if got := FromResponses(resp, "gpt-proxy", 1, "id").Choices[0].FinishReason; got != "content_filter" {
		t.Errorf("finish_reason = %q, want content_filter", got)
	}
}

func TestResponsesFinishReasonOrder(t *testing.T) {
	toolCall := oairesp.Item{Type: "function_call", CallID: "call_1"}
	message := oairesp.Item{Type: "message", Role: "assistant"}
	cases := []struct {
		name string
		resp oairesp.Response
		want string
	}{
		{"text", oairesp.Response{Status: "completed", Output: []oairesp.Item{message}}, "stop"},
		{"no output", oairesp.Response{Status: "completed"}, "stop"},
		{"tool call", oairesp.Response{Status: "completed", Output: []oairesp.Item{message, toolCall}}, "tool_calls"},
		{
			"truncation beats a partial tool call",
			oairesp.Response{
				Status:            "incomplete",
				IncompleteDetails: &oairesp.IncompleteDetails{Reason: "max_output_tokens"},
				Output:            []oairesp.Item{toolCall},
			},
			"length",
		},
		{
			"content filter beats a partial tool call",
			oairesp.Response{
				Status:            "incomplete",
				IncompleteDetails: &oairesp.IncompleteDetails{Reason: "content_filter"},
				Output:            []oairesp.Item{toolCall},
			},
			"content_filter",
		},
		{"incomplete without a reason", oairesp.Response{Status: "incomplete"}, "stop"},
		{
			"unknown incomplete reason",
			oairesp.Response{Status: "incomplete", IncompleteDetails: &oairesp.IncompleteDetails{Reason: "something_new"}},
			"stop",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResponsesFinishReason(tc.resp); got != tc.want {
				t.Errorf("ResponsesFinishReason = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResponsesStoreUsage(t *testing.T) {
	cases := []struct {
		name string
		in   *oairesp.Usage
		want store.Usage
	}{
		{"cached subset of input", responsesUsageFixture(), store.Usage{InputTokens: 60, OutputTokens: 50, CacheReadTokens: 40}},
		{
			"cached above input clamps",
			&oairesp.Usage{InputTokens: 100, InputTokensDetails: &oairesp.InputTokenDetails{CachedTokens: 500}},
			store.Usage{InputTokens: 0, CacheReadTokens: 100},
		},
		{
			"negative cached clamps",
			&oairesp.Usage{InputTokens: 10, InputTokensDetails: &oairesp.InputTokenDetails{CachedTokens: -5}, OutputTokens: 2},
			store.Usage{InputTokens: 10, OutputTokens: 2},
		},
		{"no details", &oairesp.Usage{InputTokens: 7, OutputTokens: 3}, store.Usage{InputTokens: 7, OutputTokens: 3}},
		{"nil", nil, store.Usage{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResponsesStoreUsage(tc.in); got != tc.want {
				t.Errorf("ResponsesStoreUsage = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestResponsesOAIUsage(t *testing.T) {
	if got := ResponsesOAIUsage(nil); got.PromptTokens != 0 || got.CompletionTokens != 0 ||
		got.TotalTokens != 0 || got.PromptTokensDetails != nil || got.CompletionTokensDetails != nil {
		t.Errorf("ResponsesOAIUsage(nil) = %+v, want zero", got)
	}

	got := ResponsesOAIUsage(&oairesp.Usage{InputTokens: 12, OutputTokens: 8})
	if got.PromptTokens != 12 || got.CompletionTokens != 8 || got.TotalTokens != 20 {
		t.Errorf("ResponsesOAIUsage = %+v", got)
	}
	if got.PromptTokensDetails != nil || got.CompletionTokensDetails != nil {
		t.Errorf("unexpected details: %+v %+v", got.PromptTokensDetails, got.CompletionTokensDetails)
	}
}
