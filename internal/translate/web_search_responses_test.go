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

	"github.com/SnowballSH/modelgate/internal/oai"
	"github.com/SnowballSH/modelgate/internal/oairesp"
	"github.com/SnowballSH/modelgate/internal/store"
)

func TestToResponsesSendsTheSearchTool(t *testing.T) {
	tests := []struct {
		name, options, wantTools string
	}{
		{"defaults", `{}`, `[{"type":"web_search"}]`},
		{"context size and location", `{"search_context_size":"high","user_location":{"type":"approximate","approximate":{"city":"Boston","country":"US"}}}`,
			`[{"type":"web_search","search_context_size":"high","user_location":{"type":"approximate","city":"Boston","country":"US"}}]`},
		{"an empty approximate location opts out of the US default", `{"user_location":{"type":"approximate","approximate":{}}}`,
			`[{"type":"web_search","user_location":{"type":"approximate"}}]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := oai.ChatRequest{Model: "m", Messages: []oai.Message{{Role: "user", Content: json.RawMessage(`"news?"`)}}}
			opts := ResponsesOptions{ProviderModel: "gpt-real", DefaultMaxTokens: 1000,
				WebSearch: &WebSearchTool{Tool: "web_search", MaxUses: 3, Options: searchOptions(t, tc.options)}}
			got, err := ToResponsesWith(req, opts)
			if err != nil {
				t.Fatal(err)
			}
			tools, _ := json.Marshal(got.Tools)
			if string(tools) != tc.wantTools {
				t.Errorf("tools = %s\nwant    %s", tools, tc.wantTools)
			}
			if got.MaxToolCalls == nil || *got.MaxToolCalls != 3 {
				t.Errorf("max_tool_calls = %v, want 3", got.MaxToolCalls)
			}
			if opts.Dropped(req) != nil {
				t.Errorf("Dropped = %v, want none", opts.Dropped(req))
			}
		})
	}
}

func TestToResponsesWithoutSearchSendsNoToolCallLimit(t *testing.T) {
	req := oai.ChatRequest{Model: "m", Messages: []oai.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
	got, err := ToResponsesWith(req, ResponsesOptions{ProviderModel: "gpt-real", DefaultMaxTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "max_tool_calls") || strings.Contains(string(raw), "web_search") {
		t.Errorf("a request without search carries search fields: %s", raw)
	}
}

func loadResponsesFixture(t *testing.T) oairesp.Response {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "websearch", "responses_response.json"))
	if err != nil {
		t.Fatal(err)
	}
	var resp oairesp.Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestFromResponsesCitesSearchSources(t *testing.T) {
	resp := loadResponsesFixture(t)
	got := FromResponses(resp, "gpt", 1, "chatcmpl-x")
	msg := got.Choices[0].Message
	want := "A river was restored this week ([example.com](https://www.example.com/good-news)). Volunteers planted trees ([trees.org](https://trees.org/a)). [1][2]"
	if deref(msg.Content) != want {
		t.Errorf("content = %q\nwant      %q", deref(msg.Content), want)
	}
	wantAnnotations := []oai.Annotation{
		oai.URLCitationAnnotation("https://www.example.com/good-news", "Good news"),
		oai.URLCitationAnnotation("https://trees.org/a", "Trees"),
	}
	if !reflect.DeepEqual(msg.Annotations, wantAnnotations) {
		t.Errorf("annotations = %+v", msg.Annotations)
	}
	if msg.ToolCalls != nil || got.Choices[0].FinishReason != "stop" {
		t.Errorf("a web search call is no client tool call: %+v, %q", msg.ToolCalls, got.Choices[0].FinishReason)
	}
	if n := ResponsesWebSearches(resp.Output); n != 2 {
		t.Errorf("ResponsesWebSearches = %d, want 2 (every web_search_call item)", n)
	}
	if u := ResponsesStoreUsage(resp.Usage); u != (store.Usage{InputTokens: 6000, OutputTokens: 300, CacheReadTokens: 1000, CacheWriteTokens: 2000}) {
		t.Errorf("usage = %+v", u)
	}
}

func readResponsesEvents(t *testing.T, name string) []oairesp.StreamEvent {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "websearch", name))
	if err != nil {
		t.Fatal(err)
	}
	var events []oairesp.StreamEvent
	lines := bufio.NewScanner(bytes.NewReader(raw))
	for lines.Scan() {
		var ev oairesp.StreamEvent
		if err := json.Unmarshal(lines.Bytes(), &ev); err != nil {
			t.Fatalf("decode %s: %v", lines.Text(), err)
		}
		events = append(events, ev)
	}
	return events
}

func TestResponsesStreamCitesSourcesAndCountsSearches(t *testing.T) {
	st := NewResponsesStreamTranslator("gpt", 1700000000, "chatcmpl-s")
	var text strings.Builder
	var annotations []oai.Annotation
	var finish string
	seenBeforeText := int64(-1)
	for _, ev := range readResponsesEvents(t, "responses_stream.events") {
		if ev.Type == "response.output_text.delta" && seenBeforeText < 0 {
			seenBeforeText = st.WebSearchesSeen()
		}
		chunks, err := st.Next(ev)
		if err != nil {
			t.Fatalf("Next(%s): %v", ev.Type, err)
		}
		for _, ch := range chunks {
			d := ch.Choices[0].Delta
			if d.ToolCalls != nil {
				t.Fatalf("a web search call leaked as a tool call: %+v", d.ToolCalls)
			}
			text.WriteString(d.Content)
			annotations = append(annotations, d.Annotations...)
			if ch.Choices[0].FinishReason != nil {
				finish = *ch.Choices[0].FinishReason
			}
		}
	}
	if want := "A river was restored ([example.com](https://www.example.com/good-news)). [1]"; text.String() != want {
		t.Errorf("text = %q\nwant   %q", text.String(), want)
	}
	if want := []oai.Annotation{oai.URLCitationAnnotation("https://www.example.com/good-news", "Good news")}; !reflect.DeepEqual(annotations, want) {
		t.Errorf("annotations = %+v", annotations)
	}
	if finish != "stop" {
		t.Errorf("finish_reason = %q", finish)
	}
	if seenBeforeText != 1 || st.WebSearches() != 1 {
		t.Errorf("searches seen before the text = %d, final = %d; want 1 and 1", seenBeforeText, st.WebSearches())
	}
}
