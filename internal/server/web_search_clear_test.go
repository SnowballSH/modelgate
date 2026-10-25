package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/SnowballSH/modelgate/internal/accounting"
	"github.com/SnowballSH/modelgate/internal/models"
	"github.com/SnowballSH/modelgate/internal/oai"
	"github.com/SnowballSH/modelgate/internal/store"
)

func requestWithSearch(raw string) oai.ChatRequest {
	return oai.ChatRequest{Model: "m", WebSearchOptions: json.RawMessage(raw),
		Messages: []oai.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
}

// Each of the two clears that keep the client's web_search_options off the
// wire is pinned on its own: the plan hands every path a request without the
// field, and the passthrough strips it again from whatever it is given.
func TestPlanWebSearchHandsOnARequestWithoutTheRawField(t *testing.T) {
	row := models.Model{WebSearch: &models.WebSearch{Tool: models.AnthropicWebSearchTool, MaxUses: 3, InputTokensPerSearch: 1}}
	for _, tc := range []struct {
		name, raw string
		enabled   bool
		key       store.KeyRecord
	}{
		{"null", `null`, true, store.KeyRecord{WebSearch: true}},
		{"gateway off", `{"search_context_size":"low"}`, false, store.KeyRecord{WebSearch: true}},
		{"key off", `{}`, true, store.KeyRecord{}},
		{"searching", `{"search_context_size":"low"}`, true, store.KeyRecord{WebSearch: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, req, refusal := planWebSearch(tc.enabled, tc.key, "m", row, requestWithSearch(tc.raw))
			if refusal != nil {
				t.Fatalf("refused: %+v", refusal)
			}
			if req.WebSearchOptions != nil {
				t.Errorf("the planned request still carries %s", req.WebSearchOptions)
			}
		})
	}
}

func TestPassthroughRequestNeverCarriesTheRawField(t *testing.T) {
	model := models.Model{Provider: models.ProviderOpenAI, UpstreamAPI: models.UpstreamChatCompletions, ProviderModel: "gpt-pass"}
	for _, raw := range []string{`{"search_context_size":"low"}`, `null`, `{}`} {
		up := passthroughRequest(requestWithSearch(raw), model, 100, "key")
		body, err := json.Marshal(up)
		if err != nil {
			t.Fatal(err)
		}
		if up.WebSearchOptions != nil || strings.Contains(string(body), "web_search_options") {
			t.Errorf("passthrough of %s sends %s", raw, body)
		}
	}
}

// The Anthropic server loop may sample up to ten times, whatever max_uses
// says: past the last search, Claude can keep asking and get
// max_uses_exceeded back, and every sampling re-reads the prompt, the
// results so far and the output the earlier samplings wrote.
func TestSearchDemandBoundsTheProvidersLoop(t *testing.T) {
	base := accounting.Demand{InputTokens: 10_000, OutputTokens: 32_000}
	anthropic := &models.WebSearch{Tool: models.AnthropicWebSearchTool, USDPerSearch: 0.01, MaxUses: 3, InputTokensPerSearch: 25_000}
	responses := &models.WebSearch{Tool: models.ResponsesWebSearchTool, USDPerSearch: 0.01, MaxUses: 3, InputTokensPerSearch: 25_000}
	tests := []struct {
		name   string
		search *models.WebSearch
		want   accounting.Demand
	}{
		{"no search", nil, base},
		// 10·P + A·(0+1+2+3·7) + 9·O
		{"anthropic", anthropic, accounting.Demand{InputTokens: 100_000 + 25_000*24 + 9*32_000, OutputTokens: 32_000, WebSearches: 3}},
		// (U+1)·P + A·U(U+1)/2
		{"responses", responses, accounting.Demand{InputTokens: 40_000 + 25_000*6, OutputTokens: 32_000, WebSearches: 3}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := withWebSearch(base, tc.search); got != tc.want {
				t.Errorf("withWebSearch = %+v, want %+v", got, tc.want)
			}
		})
	}
}

const passthroughAnnotations = `[{"type":"url_citation","url_citation":{"url":"https://ok.example/a","title":"OK","start_index":0,"end_index":2}},` +
	`{"type":"url_citation","url_citation":{"url":"javascript:alert(1)","title":"bad"}},` +
	`{"type":"url_citation","url_citation":{"url":"ftp://files.example/x","title":"ftp"}},` +
	`{"type":"file_citation","file_citation":{"file_id":"f"}}]`

// A passthrough upstream's annotations go through the same source filter as
// the translated paths: only http(s) url_citations reach the client.
func TestPassthroughAnnotationsKeepOnlyWebSources(t *testing.T) {
	want := `"annotations":[{"type":"url_citation","url_citation":{"url":"https://ok.example/a","title":"OK"}}]`
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint("stream=", stream), func(t *testing.T) {
			env := newSearchEnv(t, searchEnvOptions{openai: func(w http.ResponseWriter, r *http.Request) {
				if !stream {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"id":"c","object":"chat.completion","model":"gpt-pass","choices":[{"index":0,"message":{"role":"assistant","content":"hi","annotations":%s},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, passthroughAnnotations)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-pass\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\",\"annotations\":%s},\"finish_reason\":null}]}\n\n", passthroughAnnotations)
				fmt.Fprint(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-pass\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n")
			}})
			auth, _ := env.key(t, false, nil)
			rec := env.post(auth, searchRequest("pass-row", stream, ""))
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if body := rec.Body.String(); !strings.Contains(body, want) || strings.Contains(body, "javascript:") || strings.Contains(body, "ftp:") || strings.Contains(body, "file_citation") {
				t.Errorf("body = %s\nwant annotations %s only", body, want)
			}
		})
	}
}
