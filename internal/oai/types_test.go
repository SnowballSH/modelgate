package oai

import (
	"encoding/json"
	"strings"
	"testing"
)

const logprobs = `{"content":[{"token":"Hi","logprob":-0.01,"bytes":[72,105],"top_logprobs":[]}]}`

func TestChoiceCarriesLogprobs(t *testing.T) {
	var resp ChatResponse
	body := `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"logprobs":` + logprobs + `,"finish_reason":"stop"}]}`
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"logprobs":`+logprobs) {
		t.Errorf("re-encoded response lost logprobs: %s", out)
	}
}

func TestChunkChoiceCarriesLogprobs(t *testing.T) {
	var chunk ChatChunk
	body := `{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hi"},"logprobs":` + logprobs + `,"finish_reason":null}]}`
	if err := json.Unmarshal([]byte(body), &chunk); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(chunk)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"logprobs":`+logprobs) {
		t.Errorf("re-encoded chunk lost logprobs: %s", out)
	}
}

func TestChoiceWithoutLogprobsOmitsThem(t *testing.T) {
	out, err := json.Marshal(ChatResponse{Choices: []Choice{{Message: ResponseMessage{Role: "assistant"}, FinishReason: "stop"}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "logprobs") {
		t.Errorf("a translated response grew a logprobs key: %s", out)
	}
}

func TestForcesToolCall(t *testing.T) {
	tests := []struct {
		toolChoice string
		want       bool
	}{
		{``, false},
		{`null`, false},
		{`"auto"`, false},
		{`"none"`, false},
		{`"required"`, true},
		{`{"type":"function","function":{"name":"get_weather"}}`, true},
		{`{"type":"custom","custom":{"name":"grammar"}}`, true},
		{`{"type":"allowed_tools","allowed_tools":{"mode":"required","tools":[]}}`, true},
		{`{"type":"allowed_tools","allowed_tools":{"mode":"auto","tools":[]}}`, false},
	}
	for _, tc := range tests {
		req := ChatRequest{ToolChoice: json.RawMessage(tc.toolChoice)}
		if got := req.ForcesToolCall(); got != tc.want {
			t.Errorf("ForcesToolCall(%s) = %v, want %v", tc.toolChoice, got, tc.want)
		}
	}
}
