package server

import (
	"testing"

	"github.com/SnowballSH/modelgate/internal/accounting"
	"github.com/SnowballSH/modelgate/internal/oai"
)

func intPtr(v int) *int { return &v }

func TestWorstCaseDemand(t *testing.T) {
	const body, def, ceiling = 1000, 4096, 128_000
	input := int64(body + promptOverheadTokens)
	cases := []struct {
		name string
		req  oai.ChatRequest
		want int64
	}{
		{"defaults", oai.ChatRequest{}, def},
		{"max_tokens alone", oai.ChatRequest{MaxTokens: intPtr(100)}, 100},
		{"max_completion_tokens alone keeps the default in reach", oai.ChatRequest{MaxCompletionTokens: intPtr(100)}, def},
		{"max_completion_tokens above the default", oai.ChatRequest{MaxCompletionTokens: intPtr(9000)}, 9000},
		{"both, larger wins", oai.ChatRequest{MaxTokens: intPtr(100), MaxCompletionTokens: intPtr(700)}, 700},
		{"n multiplies", oai.ChatRequest{MaxTokens: intPtr(100), N: intPtr(3)}, 300},
		{"at the ceiling", oai.ChatRequest{MaxTokens: intPtr(ceiling)}, ceiling},
	}
	for _, tc := range cases {
		got, err := worstCaseDemand(tc.req, body, def, ceiling)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got != (accounting.Demand{InputTokens: input, OutputTokens: tc.want}) {
			t.Errorf("%s: demand = %+v, want output %d", tc.name, got, tc.want)
		}
	}

	for name, req := range map[string]oai.ChatRequest{
		"above the ceiling":          {MaxTokens: intPtr(ceiling + 1)},
		"n pushes above the ceiling": {MaxTokens: intPtr(ceiling/2 + 1), N: intPtr(2)},
		"huge n":                     {N: intPtr(1 << 40)},
		"zero max_tokens":            {MaxTokens: intPtr(0)},
		"negative max_completion":    {MaxCompletionTokens: intPtr(-5)},
		"zero n":                     {N: intPtr(0)},
	} {
		if _, err := worstCaseDemand(req, body, def, ceiling); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
