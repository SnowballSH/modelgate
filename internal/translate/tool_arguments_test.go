package translate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/SnowballSH/modelgate/internal/anthro"
	"github.com/SnowballSH/modelgate/internal/oai"
)

func noArgumentToolEvents(argumentDeltas ...string) []anthro.StreamEvent {
	events := []anthro.StreamEvent{
		{Type: "message_start", Message: &anthro.MessagesResponse{Usage: anthro.Usage{InputTokens: 12}}},
		{Type: "content_block_start", Index: 0, ContentBlock: &anthro.ContentBlock{Type: "tool_use", ID: "toolu_now", Name: "get_time", Input: json.RawMessage(`{}`)}},
	}
	for _, partial := range argumentDeltas {
		events = append(events, anthro.StreamEvent{Type: "content_block_delta", Index: 0, Delta: &anthro.StreamDelta{Type: "input_json_delta", PartialJSON: partial}})
	}
	return append(events,
		anthro.StreamEvent{Type: "content_block_stop", Index: 0},
		anthro.StreamEvent{Type: "message_delta", Delta: &anthro.StreamDelta{StopReason: "tool_use"}, Usage: &anthro.Usage{OutputTokens: 9}},
		anthro.StreamEvent{Type: "message_stop"},
	)
}

// accumulateToolCalls rebuilds the tool calls a Chat Completions client
// assembles from a stream: fragments concatenated per index.
func accumulateToolCalls(chunks []oai.ChatChunk) []oai.ToolCall {
	var calls []oai.ToolCall
	for _, chunk := range chunks {
		for _, delta := range chunk.Choices[0].Delta.ToolCalls {
			for len(calls) <= delta.Index {
				calls = append(calls, oai.ToolCall{Type: "function"})
			}
			call := &calls[delta.Index]
			if delta.ID != "" {
				call.ID = delta.ID
			}
			if delta.Function != nil {
				call.Function.Name += delta.Function.Name
				call.Function.Arguments += delta.Function.Arguments
			}
		}
	}
	return calls
}

func TestStreamNoArgumentToolCallSendsEmptyObject(t *testing.T) {
	for name, deltas := range map[string][]string{
		"no argument deltas":    nil,
		"one empty delta":       {""},
		"several empty deltas":  {"", ""},
		"empty object streamed": {"", "{}"},
	} {
		t.Run(name, func(t *testing.T) {
			st := NewStreamTranslator("gpt-proxy", 1700000000, "chatcmpl-s")
			chunks := drive(t, st, noArgumentToolEvents(deltas...))
			checkEnvelope(t, chunks)
			calls := accumulateToolCalls(chunks)
			if len(calls) != 1 || calls[0].ID != "toolu_now" || calls[0].Function.Name != "get_time" {
				t.Fatalf("tool calls = %+v", calls)
			}
			if calls[0].Function.Arguments != "{}" {
				t.Errorf("arguments = %q, want %q", calls[0].Function.Arguments, "{}")
			}
		})
	}
}

func TestStreamToolCallWithArgumentsGetsNoExtraObject(t *testing.T) {
	st := NewStreamTranslator("gpt-proxy", 1700000000, "chatcmpl-s")
	events := []anthro.StreamEvent{
		{Type: "message_start", Message: &anthro.MessagesResponse{}},
		{Type: "content_block_start", Index: 0, ContentBlock: &anthro.ContentBlock{Type: "tool_use", ID: "toolu_a", Name: "get_time"}},
		{Type: "content_block_stop", Index: 0},
		{Type: "content_block_start", Index: 1, ContentBlock: &anthro.ContentBlock{Type: "tool_use", ID: "toolu_b", Name: "get_weather"}},
		{Type: "content_block_delta", Index: 1, Delta: &anthro.StreamDelta{Type: "input_json_delta", PartialJSON: ""}},
		{Type: "content_block_delta", Index: 1, Delta: &anthro.StreamDelta{Type: "input_json_delta", PartialJSON: `{"city":"Paris"}`}},
		{Type: "content_block_stop", Index: 1},
		{Type: "message_delta", Delta: &anthro.StreamDelta{StopReason: "tool_use"}},
		{Type: "message_stop"},
	}
	calls := accumulateToolCalls(drive(t, st, events))
	if len(calls) != 2 {
		t.Fatalf("tool calls = %+v", calls)
	}
	if calls[0].Function.Arguments != "{}" {
		t.Errorf("no-argument call arguments = %q, want {}", calls[0].Function.Arguments)
	}
	if calls[1].Function.Arguments != `{"city":"Paris"}` {
		t.Errorf("argument call arguments = %q", calls[1].Function.Arguments)
	}
}

func TestStreamedNoArgumentCallRoundTrips(t *testing.T) {
	st := NewStreamTranslator("gpt-proxy", 1700000000, "chatcmpl-s")
	calls := accumulateToolCalls(drive(t, st, noArgumentToolEvents("")))
	history := oai.ChatRequest{Messages: []oai.Message{
		{Role: "user", Content: json.RawMessage(`"what time is it?"`)},
		{Role: "assistant", ToolCalls: calls},
		{Role: "tool", ToolCallID: calls[0].ID, Content: json.RawMessage(`"14:05"`)},
	}}

	areq, err := ToAnthropic(history, "claude-real", 1024)
	if err != nil {
		t.Fatalf("ToAnthropic on the echoed history: %v", err)
	}
	if input := string(areq.Messages[1].Content[0].Input); input != "{}" {
		t.Errorf("tool_use input = %s, want {}", input)
	}

	rreq, err := ToResponses(history, "gpt-real", 1024)
	if err != nil {
		t.Fatalf("ToResponses on the echoed history: %v", err)
	}
	if args := rreq.Input[1].Arguments; args != "{}" {
		t.Errorf("function_call arguments = %q, want {}", args)
	}
}

func TestHistoryAcceptsEmptyArgumentsAsEmptyObject(t *testing.T) {
	for _, arguments := range []string{"", " ", "\n"} {
		req := oai.ChatRequest{Messages: []oai.Message{
			{Role: "user", Content: json.RawMessage(`"hi"`)},
			{Role: "assistant", ToolCalls: []oai.ToolCall{{ID: "c1", Type: "function", Function: oai.FunctionCall{Name: "get_time", Arguments: arguments}}}},
			{Role: "tool", ToolCallID: "c1", Content: json.RawMessage(`"14:05"`)},
		}}
		areq, err := ToAnthropic(req, "claude-real", 1024)
		if err != nil {
			t.Fatalf("ToAnthropic(arguments %q): %v", arguments, err)
		}
		if input := string(areq.Messages[1].Content[0].Input); input != "{}" {
			t.Errorf("arguments %q: tool_use input = %s, want {}", arguments, input)
		}
		if req.Messages[1].ToolCalls[0].Function.Arguments != arguments {
			t.Errorf("translation rewrote the caller's history in place")
		}
	}
}

func TestHistoryStillRefusesMalformedArguments(t *testing.T) {
	req := oai.ChatRequest{Messages: []oai.Message{
		{Role: "user", Content: json.RawMessage(`"hi"`)},
		{Role: "assistant", ToolCalls: []oai.ToolCall{{ID: "c1", Type: "function", Function: oai.FunctionCall{Name: "f", Arguments: "{oops"}}}},
	}}
	if _, err := ToAnthropic(req, "claude-real", 1024); err == nil || !strings.Contains(err.Error(), "invalid arguments JSON") {
		t.Fatalf("err = %v, want an invalid arguments error", err)
	}
}

func TestFromAnthropicToolUseWithoutInput(t *testing.T) {
	resp := anthro.MessagesResponse{
		Content:    []anthro.ContentBlock{{Type: "tool_use", ID: "toolu_now", Name: "get_time"}},
		StopReason: "tool_use",
	}
	got := FromAnthropic(resp, "m", 1, "id")
	if args := got.Choices[0].Message.ToolCalls[0].Function.Arguments; args != "{}" {
		t.Errorf("arguments = %q, want {}", args)
	}
}
