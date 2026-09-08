package translate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SnowballSH/modelgate/internal/oai"
	"github.com/SnowballSH/modelgate/internal/oairesp"
)

func loadStreamEvents(t *testing.T, name string) []oairesp.StreamEvent {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var events []oairesp.StreamEvent
	for i, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var ev oairesp.StreamEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("%s line %d: %v", name, i+1, err)
		}
		events = append(events, ev)
	}
	return events
}

func driveResponses(t *testing.T, st *ResponsesStreamTranslator, events []oairesp.StreamEvent) []oai.ChatChunk {
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

func TestResponsesStreamToolCallSequence(t *testing.T) {
	st := NewResponsesStreamTranslator("gpt-proxy", 1700000000, "chatcmpl-s")
	chunks := driveResponses(t, st, loadStreamEvents(t, "responses_stream_tool_call.events"))
	checkEnvelope(t, chunks)

	if len(chunks) != 6 {
		t.Fatalf("chunks = %d, want 6", len(chunks))
	}

	if d := chunks[0].Choices[0].Delta; d.Role != "assistant" || d.Content != "Let me " {
		t.Errorf("first chunk delta = %+v", d)
	}
	if d := chunks[1].Choices[0].Delta; d.Role != "" || d.Content != "check." {
		t.Errorf("second content chunk = %+v", d)
	}

	open := chunks[2].Choices[0].Delta.ToolCalls
	if len(open) != 1 || open[0].Index != 0 || open[0].ID != "call_1" || open[0].Type != "function" ||
		open[0].Function == nil || open[0].Function.Name != "get_weather" || open[0].Function.Arguments != "" {
		t.Errorf("tool open chunk = %+v", open)
	}

	var args strings.Builder
	for _, ch := range chunks[3:5] {
		tc := ch.Choices[0].Delta.ToolCalls
		if len(tc) != 1 || tc[0].Index != 0 || tc[0].ID != "" || tc[0].Function == nil || tc[0].Function.Name != "" {
			t.Fatalf("argument chunk = %+v", tc)
		}
		args.WriteString(tc[0].Function.Arguments)
	}
	if args.String() != `{"city":"Paris"}` {
		t.Errorf("arguments = %q", args.String())
	}

	final := chunks[5].Choices[0]
	if final.Delta.Role != "" || final.Delta.Content != "" || final.Delta.ToolCalls != nil || final.Delta.Refusal != nil {
		t.Errorf("final delta = %+v", final.Delta)
	}
	if final.FinishReason == nil || *final.FinishReason != "tool_calls" {
		t.Errorf("final finish_reason = %v", final.FinishReason)
	}
	if st.FinishReason() != "tool_calls" {
		t.Errorf("FinishReason() = %q", st.FinishReason())
	}

	u := st.Usage()
	if u == nil {
		t.Fatal("Usage() = nil")
	}
	if u.InputTokens != 100 || u.OutputTokens != 50 || u.TotalTokens != 150 {
		t.Errorf("Usage() = %+v", u)
	}
	if u.InputTokensDetails == nil || u.InputTokensDetails.CachedTokens != 40 {
		t.Errorf("input_tokens_details = %+v", u.InputTokensDetails)
	}
	if u.OutputTokensDetails == nil || u.OutputTokensDetails.ReasoningTokens != 30 {
		t.Errorf("output_tokens_details = %+v", u.OutputTokensDetails)
	}
}

func TestStreamCarriesRefusal(t *testing.T) {
	st := NewResponsesStreamTranslator("gpt-proxy", 1700000000, "chatcmpl-s")
	chunks := driveResponses(t, st, loadStreamEvents(t, "responses_stream_refusal.events"))
	checkEnvelope(t, chunks)

	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3", len(chunks))
	}
	var refusal strings.Builder
	for i, ch := range chunks {
		d := ch.Choices[0].Delta
		if d.Content != "" {
			t.Errorf("chunk %d carries content %q", i, d.Content)
		}
		if d.Refusal != nil {
			refusal.WriteString(*d.Refusal)
		}
	}
	if refusal.String() != "I can't help with that." {
		t.Errorf("refusal = %q", refusal.String())
	}
	if chunks[0].Choices[0].Delta.Role != "assistant" {
		t.Errorf("first chunk role = %q", chunks[0].Choices[0].Delta.Role)
	}
	if final := chunks[2].Choices[0]; final.FinishReason == nil || *final.FinishReason != "stop" {
		t.Errorf("final finish_reason = %v", final.FinishReason)
	}
}

func TestResponsesStreamDoneEventsWithoutDeltas(t *testing.T) {
	st := NewResponsesStreamTranslator("gpt-proxy", 1700000000, "chatcmpl-s")
	events := []oairesp.StreamEvent{
		{Type: "response.output_item.added", OutputIndex: 0, Item: &oairesp.Item{Type: "message", Role: "assistant"}},
		{Type: "response.refusal.done", OutputIndex: 0, Refusal: "I can't help with that."},
		{Type: "response.output_item.added", OutputIndex: 1, Item: &oairesp.Item{Type: "function_call", CallID: "call_1", Name: "get_weather"}},
		{Type: "response.function_call_arguments.done", OutputIndex: 1, Arguments: `{"city":"Paris"}`},
		{Type: "response.completed", Response: &oairesp.Response{Status: "completed"}},
	}
	chunks := driveResponses(t, st, events)
	if len(chunks) != 4 {
		t.Fatalf("chunks = %d, want 4", len(chunks))
	}
	if d := chunks[0].Choices[0].Delta; d.Refusal == nil || *d.Refusal != "I can't help with that." {
		t.Errorf("refusal chunk = %+v", d)
	}
	if tc := chunks[2].Choices[0].Delta.ToolCalls; len(tc) != 1 || tc[0].Index != 0 ||
		tc[0].Function == nil || tc[0].Function.Arguments != `{"city":"Paris"}` {
		t.Errorf("arguments chunk = %+v", chunks[2].Choices[0].Delta.ToolCalls)
	}
	if final := chunks[3].Choices[0]; final.FinishReason == nil || *final.FinishReason != "tool_calls" {
		t.Errorf("final finish_reason = %v", final.FinishReason)
	}
}

func TestResponsesStreamUnknownOutputIndex(t *testing.T) {
	for _, ev := range []oairesp.StreamEvent{
		{Type: "response.function_call_arguments.delta", OutputIndex: 3, Delta: "{}"},
		{Type: "response.function_call_arguments.done", OutputIndex: 3, Arguments: "{}"},
	} {
		st := NewResponsesStreamTranslator("gpt-proxy", 1, "id")
		if _, err := st.Next(ev); err == nil {
			t.Errorf("Next(%s) with an unopened output index: want error", ev.Type)
		}
	}
}

func TestResponsesStreamIgnoresOtherEvents(t *testing.T) {
	st := NewResponsesStreamTranslator("gpt-proxy", 1, "id")
	for _, typ := range []string{"response.created", "response.in_progress", "response.output_text.done", "some_future_event"} {
		out, err := st.Next(oairesp.StreamEvent{Type: typ, Response: &oairesp.Response{Status: "in_progress"}})
		if err != nil {
			t.Errorf("Next(%s) error: %v", typ, err)
		}
		if len(out) != 0 {
			t.Errorf("Next(%s) chunks: %+v", typ, out)
		}
	}
}

func TestUsageFromIncompleteResponse(t *testing.T) {
	st := NewResponsesStreamTranslator("gpt-proxy", 1700000000, "chatcmpl-s")
	events := []oairesp.StreamEvent{
		{Type: "response.output_item.added", OutputIndex: 0, Item: &oairesp.Item{Type: "reasoning"}},
		{Type: "response.incomplete", Response: &oairesp.Response{
			Status:            "incomplete",
			IncompleteDetails: &oairesp.IncompleteDetails{Reason: "max_output_tokens"},
			Usage: &oairesp.Usage{
				InputTokens:         120,
				OutputTokens:        900000,
				OutputTokensDetails: &oairesp.OutputTokenDetails{ReasoningTokens: 899000},
				TotalTokens:         900120,
			},
		}},
	}
	chunks := driveResponses(t, st, events)
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want 1", len(chunks))
	}
	if final := chunks[0].Choices[0]; final.FinishReason == nil || *final.FinishReason != "length" {
		t.Errorf("finish_reason = %v", final.FinishReason)
	}
	u := st.Usage()
	if u == nil {
		t.Fatal("Usage() = nil after response.incomplete")
	}
	if u.OutputTokens != 900000 {
		t.Errorf("output tokens = %d, want 900000", u.OutputTokens)
	}
	want := ResponsesStoreUsage(u)
	if want.OutputTokens != 900000 || want.InputTokens != 120 {
		t.Errorf("booked usage = %+v", want)
	}
}

func TestResponsesStreamFailedRecordsUsageBeforeErroring(t *testing.T) {
	st := NewResponsesStreamTranslator("gpt-proxy", 1700000000, "chatcmpl-s")
	if _, err := st.Next(oairesp.StreamEvent{Type: "response.output_item.added", OutputIndex: 0, Item: &oairesp.Item{Type: "reasoning"}}); err != nil {
		t.Fatal(err)
	}
	_, err := st.Next(oairesp.StreamEvent{Type: "response.failed", Response: &oairesp.Response{
		Status: "failed",
		Error:  &oairesp.APIError{Code: "server_error", Message: "the model failed to generate a response"},
		Usage:  &oairesp.Usage{InputTokens: 300, OutputTokens: 4096, TotalTokens: 4396},
	}})
	if err == nil {
		t.Fatal("expected an error on response.failed")
	}
	if !strings.Contains(err.Error(), "the model failed to generate a response") {
		t.Errorf("error = %q", err)
	}
	u := st.Usage()
	if u == nil {
		t.Fatal("Usage() = nil after response.failed")
	}
	if u.InputTokens != 300 || u.OutputTokens != 4096 {
		t.Errorf("Usage() = %+v", u)
	}
}

func TestResponsesStreamErrorEvent(t *testing.T) {
	st := NewResponsesStreamTranslator("gpt-proxy", 1, "id")
	_, err := st.Next(oairesp.StreamEvent{Type: "error", Code: "rate_limit_exceeded", Message: "slow down"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "slow down") {
		t.Errorf("error = %q", err)
	}
	if _, err := st.Next(oairesp.StreamEvent{Type: "response.failed"}); err == nil {
		t.Error("expected an error on a response.failed carrying no response")
	}
}
