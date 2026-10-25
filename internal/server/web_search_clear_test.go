package server

import (
	"encoding/json"
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
