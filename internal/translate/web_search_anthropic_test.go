package translate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SnowballSH/modelgate/internal/anthro"
	"github.com/SnowballSH/modelgate/internal/oai"
)

func searchOptions(t *testing.T, raw string) oai.WebSearchOptions {
	t.Helper()
	opts, err := oai.ParseWebSearchOptions(json.RawMessage(raw))
	if err != nil || opts == nil {
		t.Fatalf("ParseWebSearchOptions(%s) = %v, %v", raw, opts, err)
	}
	return *opts
}

func TestToAnthropicSendsTheServerSearchTool(t *testing.T) {
	tests := []struct {
		name, options, system, wantTools string
	}{
		{"no location, no system", `{}`, ``,
			`[{"type":"web_search_20250305","name":"web_search","cache_control":{"type":"ephemeral","ttl":"1h"},"max_uses":3}]`},
		{"full location under a system prompt", `{"search_context_size":"high","user_location":{"type":"approximate","approximate":{"city":"Boston","region":"Massachusetts","country":"US","timezone":"America/New_York"}}}`, `{"role":"system","content":"Be brief."},`,
			`[{"type":"web_search_20250305","name":"web_search","max_uses":3,"user_location":{"type":"approximate","city":"Boston","region":"Massachusetts","country":"US","timezone":"America/New_York"}}]`},
		{"an empty approximate location sends none", `{"user_location":{"type":"approximate","approximate":{}}}`, `{"role":"system","content":"Be brief."},`,
			`[{"type":"web_search_20250305","name":"web_search","max_uses":3}]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var req oai.ChatRequest
			if err := json.Unmarshal([]byte(`{"model":"m","messages":[`+tc.system+`{"role":"user","content":"news?"}]}`), &req); err != nil {
				t.Fatal(err)
			}
			opts := AnthropicOptions{ProviderModel: "claude-real", DefaultMaxTokens: 1024, CacheTTL: "1h",
				WebSearch: &WebSearchTool{Tool: "web_search_20250305", MaxUses: 3, Options: searchOptions(t, tc.options)}}
			got, err := ToAnthropicWith(req, opts)
			if err != nil {
				t.Fatal(err)
			}
			tools, _ := json.Marshal(got.Tools)
			if string(tools) != tc.wantTools {
				t.Errorf("tools = %s\nwant    %s", tools, tc.wantTools)
			}
			if got.ToolChoice != nil {
				t.Errorf("tool_choice = %+v, want none", got.ToolChoice)
			}
		})
	}
}

func TestAnthropicDropsTheContextSizeItCannotHonour(t *testing.T) {
	req := oai.ChatRequest{Model: "m"}
	opts := AnthropicOptions{WebSearch: &WebSearchTool{Tool: "web_search_20250305", MaxUses: 3, Options: oai.WebSearchOptions{SearchContextSize: "low"}}}
	if got := opts.Dropped(req); !reflect.DeepEqual(got, []string{"web_search_options.search_context_size"}) {
		t.Errorf("Dropped = %v", got)
	}
	opts.WebSearch.Options.SearchContextSize = ""
	if got := opts.Dropped(req); got != nil {
		t.Errorf("Dropped = %v, want none", got)
	}
}

func loadAnthropicResponse(t *testing.T) anthro.MessagesResponse {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "websearch", "anthropic_response.json"))
	if err != nil {
		t.Fatal(err)
	}
	var resp anthro.MessagesResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("the documented web search response must decode: %v", err)
	}
	return resp
}

func TestAnthropicSearchBlocksDecode(t *testing.T) {
	resp := loadAnthropicResponse(t)
	results, err := anthro.DecodeWebSearchResult(resp.Content[2].ResultContent)
	if err != nil || results.ErrorCode != "" || len(results.Results) != 1 || results.Results[0].URL != "https://en.wikipedia.org/wiki/Claude_Shannon" {
		t.Errorf("result array = %+v, %v", results, err)
	}
	failed, err := anthro.DecodeWebSearchResult(resp.Content[6].ResultContent)
	if err != nil || failed.ErrorCode != "max_uses_exceeded" || failed.Results != nil {
		t.Errorf("error object = %+v, %v", failed, err)
	}
	if _, err := anthro.DecodeWebSearchResult(json.RawMessage(`"text"`)); err == nil {
		t.Error("a string result decoded")
	}
	if resp.Usage.ServerToolUse == nil || resp.Usage.ServerToolUse.WebSearchRequests != 1 {
		t.Errorf("server_tool_use = %+v", resp.Usage.ServerToolUse)
	}
	if got := ToStoreUsage(resp.Usage).WebSearchRequests; got != 1 {
		t.Errorf("booked searches = %d, want 1", got)
	}
}

func TestToolResultContentStillMarshalsAsAString(t *testing.T) {
	blocks := []anthro.ContentBlock{
		{Type: "tool_result", ToolUseID: "t1", Content: "18C"},
		{Type: "tool_result", ToolUseID: "t2"},
	}
	got, _ := json.Marshal(blocks)
	if want := `[{"type":"tool_result","tool_use_id":"t1","content":"18C"},{"type":"tool_result","tool_use_id":"t2"}]`; string(got) != want {
		t.Errorf("tool results = %s, want %s", got, want)
	}
	var back anthro.ContentBlock
	if err := json.Unmarshal([]byte(`{"type":"tool_result","tool_use_id":"t1","content":"18C"}`), &back); err != nil || back.Content != "18C" || back.ResultContent != nil {
		t.Errorf("string content decoded as %+v, %v", back, err)
	}
}

func TestFromAnthropicCitesSearchSources(t *testing.T) {
	resp := FromAnthropic(loadAnthropicResponse(t), "claude", 1, "chatcmpl-x")
	msg := resp.Choices[0].Message
	want := "I'll search for when Claude Shannon was born.Based on the search results, " +
		"Claude Shannon was born on April 30, 1916, in Petoskey, Michigan [1]" +
		". He founded information theory [2][1]."
	if msg.Content == nil || *msg.Content != want {
		t.Errorf("content = %q\nwant      %q", deref(msg.Content), want)
	}
	if msg.ToolCalls != nil {
		t.Errorf("server tool use leaked as tool calls: %+v", msg.ToolCalls)
	}
	wantAnnotations := []oai.Annotation{
		oai.URLCitationAnnotation("https://en.wikipedia.org/wiki/Claude_Shannon", "Claude Shannon - Wikipedia"),
		oai.URLCitationAnnotation("https://www.britannica.com/biography/Claude-Shannon", "Claude Shannon | Britannica"),
	}
	if !reflect.DeepEqual(msg.Annotations, wantAnnotations) {
		t.Errorf("annotations = %+v", msg.Annotations)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q", resp.Choices[0].FinishReason)
	}
}

func TestPauseTurnReportsAnUnfinishedAnswer(t *testing.T) {
	if got := FinishReason("pause_turn"); got != "length" {
		t.Errorf("FinishReason(pause_turn) = %q, want length", got)
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func readAnthropicSSE(t *testing.T, name string) []anthro.StreamEvent {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "websearch", name))
	if err != nil {
		t.Fatal(err)
	}
	var events []anthro.StreamEvent
	lines := bufio.NewScanner(bytes.NewReader(raw))
	for lines.Scan() {
		payload, ok := strings.CutPrefix(lines.Text(), "data:")
		if !ok {
			continue
		}
		var ev anthro.StreamEvent
		if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &ev); err != nil {
			t.Fatalf("decode %s: %v", payload, err)
		}
		events = append(events, ev)
	}
	return events
}

func TestStreamTranslatorSwallowsSearchAndCitesSources(t *testing.T) {
	st := NewStreamTranslator("claude", 1700000000, "chatcmpl-s")
	var text strings.Builder
	var annotations []oai.Annotation
	var finish string
	for _, ev := range readAnthropicSSE(t, "anthropic_stream.sse") {
		chunks, err := st.Next(ev)
		if err != nil {
			t.Fatalf("Next(%s): %v", ev.Type, err)
		}
		for _, ch := range chunks {
			d := ch.Choices[0].Delta
			if d.ToolCalls != nil {
				t.Fatalf("server tool use leaked as a tool call: %+v", d.ToolCalls)
			}
			text.WriteString(d.Content)
			annotations = append(annotations, d.Annotations...)
			if ch.Choices[0].FinishReason != nil {
				finish = *ch.Choices[0].FinishReason
			}
		}
	}
	want := "I'll check the current weather in New York City for you." +
		"Here's the current weather information for New York City:\n\n# Weather in New York City\n\n" +
		"Expect highs near 70F [1] and rain later this week [2][1]"
	if text.String() != want {
		t.Errorf("text = %q\nwant   %q", text.String(), want)
	}
	wantAnnotations := []oai.Annotation{
		oai.URLCitationAnnotation("https://world-weather.info/forecast/usa/new_york/may-2025/", "Weather in New York City in May 2025 (New York) - detailed Weather Forecast for a month"),
		oai.URLCitationAnnotation("https://weather.example/nyc", "NYC rain"),
	}
	if !reflect.DeepEqual(annotations, wantAnnotations) {
		t.Errorf("annotations = %+v", annotations)
	}
	if finish != "stop" {
		t.Errorf("finish_reason = %q", finish)
	}
	if u := st.Usage(); u.ServerToolUse == nil || u.ServerToolUse.WebSearchRequests != 1 || u.InputTokens != 10682 {
		t.Errorf("usage = %+v", u)
	}
	if got := st.WebSearchesSeen(); got != 1 {
		t.Errorf("searches seen = %d, want 1: the second came back as an unbilled error", got)
	}
	if got := st.WebSearchCallsSeen(); got != 2 {
		t.Errorf("search calls seen = %d, want 2: the failed one still made the loop sample again", got)
	}
}

func TestStreamTranslatorCountsSearchesBeforeTheirResults(t *testing.T) {
	st := NewStreamTranslator("claude", 1, "id")
	events := []anthro.StreamEvent{
		{Type: "message_start", Message: &anthro.MessagesResponse{}},
		{Type: "content_block_start", Index: 0, ContentBlock: &anthro.ContentBlock{Type: "server_tool_use", ID: "srvtoolu_1", Name: "web_search"}},
		{Type: "content_block_delta", Index: 0, Delta: &anthro.StreamDelta{Type: "input_json_delta", PartialJSON: `{"query":"x"}`}},
		{Type: "content_block_stop", Index: 0},
	}
	for _, ev := range events {
		chunks, err := st.Next(ev)
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type != "message_start" && len(chunks) != 0 {
			t.Errorf("%s produced %d chunks", ev.Type, len(chunks))
		}
	}
	if got := st.WebSearchesSeen(); got != 1 {
		t.Errorf("searches seen = %d, want 1 once the search was requested", got)
	}
}
