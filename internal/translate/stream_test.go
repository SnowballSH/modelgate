package translate

import (
	"strings"
	"testing"

	"github.com/SnowballSH/modelgate/internal/anthro"
	"github.com/SnowballSH/modelgate/internal/oai"
)

func drive(t *testing.T, st *StreamTranslator, events []anthro.StreamEvent) []oai.ChatChunk {
	t.Helper()
	var chunks []oai.ChatChunk
	for _, ev := range events {
		out, err := st.Next(ev)
		if err != nil {
			t.Fatalf("Next(%s): %v", ev.Type, err)
		}
		chunks = append(chunks, out...)
	}
	return chunks
}

func checkEnvelope(t *testing.T, chunks []oai.ChatChunk) {
	t.Helper()
	for i, ch := range chunks {
		if ch.ID != "chatcmpl-s" || ch.Object != "chat.completion.chunk" || ch.Created != 1700000000 || ch.Model != "gpt-proxy" {
			t.Errorf("chunk %d envelope: %+v", i, ch)
		}
		if len(ch.Choices) != 1 || ch.Choices[0].Index != 0 {
			t.Errorf("chunk %d choices: %+v", i, ch.Choices)
		}
	}
}

func TestStreamToolCallSequence(t *testing.T) {
	st := NewStreamTranslator("gpt-proxy", 1700000000, "chatcmpl-s")
	events := []anthro.StreamEvent{
		{Type: "message_start", Message: &anthro.MessagesResponse{Usage: anthro.Usage{InputTokens: 25, CacheReadInputTokens: 10}}},
		{Type: "content_block_start", Index: 0, ContentBlock: &anthro.ContentBlock{Type: "text"}},
		{Type: "content_block_delta", Index: 0, Delta: &anthro.StreamDelta{Type: "text_delta", Text: "Let me "}},
		{Type: "content_block_delta", Index: 0, Delta: &anthro.StreamDelta{Type: "text_delta", Text: "check."}},
		{Type: "content_block_stop", Index: 0},
		{Type: "content_block_start", Index: 1, ContentBlock: &anthro.ContentBlock{Type: "tool_use", ID: "toolu_9", Name: "get_weather"}},
		{Type: "content_block_delta", Index: 1, Delta: &anthro.StreamDelta{Type: "input_json_delta", PartialJSON: `{"city":`}},
		{Type: "content_block_delta", Index: 1, Delta: &anthro.StreamDelta{Type: "input_json_delta", PartialJSON: `"Paris"}`}},
		{Type: "content_block_stop", Index: 1},
		{Type: "message_delta", Delta: &anthro.StreamDelta{StopReason: "tool_use"}, Usage: &anthro.Usage{OutputTokens: 50}},
		{Type: "message_stop"},
	}
	chunks := drive(t, st, events)
	checkEnvelope(t, chunks)

	if len(chunks) != 7 {
		t.Fatalf("chunks = %d, want 7", len(chunks))
	}

	if d := chunks[0].Choices[0].Delta; d.Role != "assistant" || d.Content != "" || d.ToolCalls != nil {
		t.Errorf("role chunk delta = %+v", d)
	}
	if chunks[0].Choices[0].FinishReason != nil {
		t.Errorf("role chunk finish_reason = %v", *chunks[0].Choices[0].FinishReason)
	}

	if d := chunks[1].Choices[0].Delta; d.Content != "Let me " {
		t.Errorf("content chunk 1 = %+v", d)
	}
	if d := chunks[2].Choices[0].Delta; d.Content != "check." {
		t.Errorf("content chunk 2 = %+v", d)
	}

	openTC := chunks[3].Choices[0].Delta.ToolCalls
	if len(openTC) != 1 || openTC[0].Index != 0 || openTC[0].ID != "toolu_9" || openTC[0].Type != "function" ||
		openTC[0].Function == nil || openTC[0].Function.Name != "get_weather" || openTC[0].Function.Arguments != "" {
		t.Errorf("tool open chunk = %+v", openTC)
	}

	arg1 := chunks[4].Choices[0].Delta.ToolCalls
	if len(arg1) != 1 || arg1[0].Index != 0 || arg1[0].ID != "" || arg1[0].Function == nil ||
		arg1[0].Function.Name != "" || arg1[0].Function.Arguments != `{"city":` {
		t.Errorf("arg chunk 1 = %+v", arg1)
	}
	arg2 := chunks[5].Choices[0].Delta.ToolCalls
	if len(arg2) != 1 || arg2[0].Function == nil || arg2[0].Function.Arguments != `"Paris"}` {
		t.Errorf("arg chunk 2 = %+v", arg2)
	}

	final := chunks[6].Choices[0]
	if final.Delta.Role != "" || final.Delta.Content != "" || final.Delta.ToolCalls != nil {
		t.Errorf("final delta = %+v", final.Delta)
	}
	if final.FinishReason == nil || *final.FinishReason != "tool_calls" {
		t.Errorf("final finish_reason = %v", final.FinishReason)
	}

	u := st.Usage()
	if u.InputTokens != 25 || u.CacheReadInputTokens != 10 || u.OutputTokens != 50 {
		t.Errorf("Usage() = %+v", u)
	}
	if st.FinishReason() != "tool_calls" {
		t.Errorf("FinishReason() = %q", st.FinishReason())
	}
}

func TestStreamTextOnly(t *testing.T) {
	st := NewStreamTranslator("gpt-proxy", 1700000000, "chatcmpl-s")
	events := []anthro.StreamEvent{
		{Type: "message_start", Message: &anthro.MessagesResponse{Usage: anthro.Usage{InputTokens: 5}}},
		{Type: "content_block_start", Index: 0, ContentBlock: &anthro.ContentBlock{Type: "text"}},
		{Type: "content_block_delta", Index: 0, Delta: &anthro.StreamDelta{Type: "text_delta", Text: "hi"}},
		{Type: "content_block_stop", Index: 0},
		{Type: "message_delta", Delta: &anthro.StreamDelta{StopReason: "end_turn"}, Usage: &anthro.Usage{OutputTokens: 2}},
		{Type: "message_stop"},
	}
	chunks := drive(t, st, events)
	checkEnvelope(t, chunks)
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3", len(chunks))
	}
	last := chunks[2].Choices[0]
	if last.FinishReason == nil || *last.FinishReason != "stop" {
		t.Errorf("finish_reason = %v", last.FinishReason)
	}
	if st.FinishReason() != "stop" {
		t.Errorf("FinishReason() = %q", st.FinishReason())
	}
}

func TestStreamPingIgnored(t *testing.T) {
	st := NewStreamTranslator("m", 1, "id")
	for _, typ := range []string{"ping", "some_future_event"} {
		out, err := st.Next(anthro.StreamEvent{Type: typ})
		if err != nil {
			t.Errorf("Next(%s) error: %v", typ, err)
		}
		if len(out) != 0 {
			t.Errorf("Next(%s) chunks: %+v", typ, out)
		}
	}
}

func TestStreamErrorEvent(t *testing.T) {
	st := NewStreamTranslator("m", 1, "id")
	_, err := st.Next(anthro.StreamEvent{Type: "error", Error: &anthro.APIError{Type: "overloaded_error", Message: "server overloaded"}})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "server overloaded") {
		t.Errorf("error = %q", err)
	}
}
