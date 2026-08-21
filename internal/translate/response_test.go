package translate

import (
	"encoding/json"
	"testing"

	"github.com/SnowballSH/modelgate/internal/anthro"
	"github.com/SnowballSH/modelgate/internal/store"
)

func TestFromAnthropicText(t *testing.T) {
	resp := anthro.MessagesResponse{
		ID:   "msg_1",
		Role: "assistant",
		Content: []anthro.ContentBlock{
			{Type: "text", Text: "Hello "},
			{Type: "text", Text: "world"},
		},
		StopReason: "end_turn",
		Usage:      anthro.Usage{InputTokens: 10, OutputTokens: 5},
	}
	got := FromAnthropic(resp, "gpt-proxy", 1700000000, "chatcmpl-abc")

	if got.ID != "chatcmpl-abc" || got.Object != "chat.completion" || got.Created != 1700000000 || got.Model != "gpt-proxy" {
		t.Errorf("envelope mismatch: %+v", got)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(got.Choices))
	}
	c := got.Choices[0]
	if c.Index != 0 || c.FinishReason != "stop" {
		t.Errorf("choice = %+v", c)
	}
	if c.Message.Role != "assistant" || c.Message.Content == nil || *c.Message.Content != "Hello world" {
		t.Errorf("message = %+v", c.Message)
	}
	if len(c.Message.ToolCalls) != 0 {
		t.Errorf("unexpected tool calls: %+v", c.Message.ToolCalls)
	}
	if got.Usage.PromptTokens != 10 || got.Usage.CompletionTokens != 5 || got.Usage.TotalTokens != 15 {
		t.Errorf("usage = %+v", got.Usage)
	}
	if got.Usage.PromptTokensDetails != nil {
		t.Errorf("unexpected prompt_tokens_details: %+v", got.Usage.PromptTokensDetails)
	}
}

func TestFromAnthropicToolUse(t *testing.T) {
	resp := anthro.MessagesResponse{
		ID: "msg_2",
		Content: []anthro.ContentBlock{
			{Type: "tool_use", ID: "toolu_1", Name: "get_weather", Input: json.RawMessage(`{"city":"Paris"}`)},
		},
		StopReason: "tool_use",
	}
	got := FromAnthropic(resp, "gpt-proxy", 1, "chatcmpl-x")
	c := got.Choices[0]
	if c.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", c.FinishReason)
	}
	if c.Message.Content != nil {
		t.Errorf("content = %q, want nil", *c.Message.Content)
	}
	if len(c.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(c.Message.ToolCalls))
	}
	tc := c.Message.ToolCalls[0]
	if tc.ID != "toolu_1" || tc.Type != "function" || tc.Function.Name != "get_weather" || tc.Function.Arguments != `{"city":"Paris"}` {
		t.Errorf("tool call = %+v", tc)
	}
}

func TestFromAnthropicEmptyContent(t *testing.T) {
	got := FromAnthropic(anthro.MessagesResponse{StopReason: "end_turn"}, "m", 1, "id")
	c := got.Choices[0].Message
	if c.Content == nil || *c.Content != "" {
		t.Errorf("content = %v, want empty-string pointer", c.Content)
	}
}

func TestFinishReason(t *testing.T) {
	cases := map[string]string{
		"end_turn":      "stop",
		"stop_sequence": "stop",
		"max_tokens":    "length",
		"tool_use":      "tool_calls",
		"":              "stop",
		"pause_turn":    "stop",
	}
	for in, want := range cases {
		if got := FinishReason(in); got != want {
			t.Errorf("FinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUsageMath(t *testing.T) {
	u := anthro.Usage{InputTokens: 100, OutputTokens: 30, CacheCreationInputTokens: 40, CacheReadInputTokens: 60}
	resp := FromAnthropic(anthro.MessagesResponse{Usage: u, StopReason: "end_turn"}, "m", 1, "id")
	if resp.Usage.PromptTokens != 200 || resp.Usage.CompletionTokens != 30 || resp.Usage.TotalTokens != 230 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if resp.Usage.PromptTokensDetails == nil || resp.Usage.PromptTokensDetails.CachedTokens != 60 {
		t.Errorf("prompt_tokens_details = %+v", resp.Usage.PromptTokensDetails)
	}

	su := ToStoreUsage(u)
	want := store.Usage{InputTokens: 100, OutputTokens: 30, CacheReadTokens: 60, CacheWriteTokens: 40}
	if su != want {
		t.Errorf("ToStoreUsage = %+v, want %+v", su, want)
	}
}
