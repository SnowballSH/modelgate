package server

import (
	"encoding/json"
	"strings"
	"testing"

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
