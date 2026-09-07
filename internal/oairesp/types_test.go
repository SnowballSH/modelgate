package oairesp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func loadResponse(t *testing.T, name string) Response {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return resp
}

func TestResponseWithToolCall(t *testing.T) {
	resp := loadResponse(t, "response_tool_call.json")
	if resp.Status != "completed" {
		t.Fatalf("status = %q, want completed", resp.Status)
	}
	if len(resp.Output) != 2 {
		t.Fatalf("len(Output) = %d, want 2", len(resp.Output))
	}
	if resp.Output[0].Type != "reasoning" {
		t.Fatalf("Output[0].Type = %q, want reasoning", resp.Output[0].Type)
	}
	if resp.Output[1].CallID != "call_abc" {
		t.Fatalf("Output[1].CallID = %q, want call_abc", resp.Output[1].CallID)
	}
	if resp.Output[1].Arguments != `{"city":"Paris"}` {
		t.Fatalf("Output[1].Arguments = %q", resp.Output[1].Arguments)
	}
	if resp.Usage == nil {
		t.Fatal("usage is nil")
	}
	if resp.Usage.InputTokensDetails == nil || resp.Usage.InputTokensDetails.CachedTokens != 896 {
		t.Fatalf("input token details = %+v, want cached 896", resp.Usage.InputTokensDetails)
	}
	if resp.Usage.OutputTokensDetails == nil || resp.Usage.OutputTokensDetails.ReasoningTokens != 320 {
		t.Fatalf("output token details = %+v, want reasoning 320", resp.Usage.OutputTokensDetails)
	}
}

func TestResponseWithRefusal(t *testing.T) {
	resp := loadResponse(t, "response_refusal.json")
	if len(resp.Output) != 1 || len(resp.Output[0].Content) != 1 {
		t.Fatalf("output = %+v, want one message item with one content part", resp.Output)
	}
	part := resp.Output[0].Content[0]
	if part.Type != "refusal" {
		t.Fatalf("part.Type = %q, want refusal", part.Type)
	}
	if part.Refusal != "I can't help with that." {
		t.Fatalf("part.Refusal = %q", part.Refusal)
	}
	if part.Text != "" {
		t.Fatalf("part.Text = %q, want empty", part.Text)
	}
}

func TestRequestKeepsStoreFalse(t *testing.T) {
	raw, err := json.Marshal(Request{
		Model: "gpt-5.6-luna",
		Input: []Item{{Type: "message", Role: "user", Content: []ContentPart{{Type: "input_text", Text: "hi"}}}},
		Store: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	store, ok := fields["store"]
	if !ok {
		t.Fatalf("store was omitted, so the upstream would default it to true: %s", raw)
	}
	if string(store) != "false" {
		t.Fatalf("store = %s, want false", store)
	}
}

func TestItemMarshalsAssistantTextAsShorthand(t *testing.T) {
	raw, err := json.Marshal(Item{Role: "assistant", Text: "It is 18C."})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(raw), `{"role":"assistant","content":"It is 18C."}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestItemMarshalsContentParts(t *testing.T) {
	raw, err := json.Marshal(Item{Type: "message", Role: "user", Content: []ContentPart{{Type: "input_text", Text: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(raw), `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestItemMarshalsEmptyToolOutput(t *testing.T) {
	empty := ""
	raw, err := json.Marshal(Item{Type: "function_call_output", CallID: "call_abc", Output: &empty})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(raw), `{"type":"function_call_output","call_id":"call_abc","output":""}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
