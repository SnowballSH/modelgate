package models

import (
	"errors"
	"fmt"
	"math"
)

// The provider-native search tool each upstream accepts: Anthropic's basic
// web search, which calls no code execution and is eligible for zero data
// retention, and the Responses API's web_search tool.
const (
	AnthropicWebSearchTool = "web_search_20250305"
	ResponsesWebSearchTool = "web_search"
)

const (
	MaxWebSearchUses             = 10
	MaxWebSearchTokensPerSearch  = 10_000_000
	webSearchUnsupportedUpstream = "web_search is only valid for anthropic models and openai responses models"
)

// WebSearch is a row's provider-native web search: the tool to send, its fee,
// the most searches one request may run, and the input tokens one search may
// add to every later iteration of the provider's server-side loop.
type WebSearch struct {
	Tool                 string
	USDPerSearch         float64
	MaxUses              int
	InputTokensPerSearch int64
}

type webSearchEntry struct {
	Tool                 *string  `json:"tool"`
	USDPerSearch         *float64 `json:"usd_per_search"`
	MaxUses              *int     `json:"max_uses"`
	InputTokensPerSearch *int64   `json:"input_tokens_per_search"`
}

func webSearchTool(provider, upstream string) (string, error) {
	switch {
	case provider == ProviderAnthropic:
		return AnthropicWebSearchTool, nil
	case upstream == UpstreamResponses:
		return ResponsesWebSearchTool, nil
	}
	return "", errors.New(webSearchUnsupportedUpstream)
}

func (e *webSearchEntry) validate(provider, upstream string) (*WebSearch, error) {
	if e == nil {
		return nil, nil
	}
	tool, err := webSearchTool(provider, upstream)
	if err != nil {
		return nil, err
	}
	switch {
	case e.Tool == nil:
		return nil, errors.New("web_search.tool absent")
	case *e.Tool != tool:
		return nil, fmt.Errorf("web_search.tool %q is not %s, the tool this row's upstream accepts", *e.Tool, tool)
	case e.USDPerSearch == nil:
		return nil, errors.New("web_search.usd_per_search absent")
	case !(*e.USDPerSearch > 0) || math.IsInf(*e.USDPerSearch, 0):
		return nil, fmt.Errorf("web_search.usd_per_search must be positive, got %v", *e.USDPerSearch)
	case e.MaxUses == nil:
		return nil, errors.New("web_search.max_uses absent")
	case *e.MaxUses < 1 || *e.MaxUses > MaxWebSearchUses:
		return nil, fmt.Errorf("web_search.max_uses must be between 1 and %d, got %d", MaxWebSearchUses, *e.MaxUses)
	case e.InputTokensPerSearch == nil:
		return nil, errors.New("web_search.input_tokens_per_search absent")
	case *e.InputTokensPerSearch < 1 || *e.InputTokensPerSearch > MaxWebSearchTokensPerSearch:
		return nil, fmt.Errorf("web_search.input_tokens_per_search must be between 1 and %d, got %d", MaxWebSearchTokensPerSearch, *e.InputTokensPerSearch)
	}
	return &WebSearch{
		Tool:                 tool,
		USDPerSearch:         *e.USDPerSearch,
		MaxUses:              *e.MaxUses,
		InputTokensPerSearch: *e.InputTokensPerSearch,
	}, nil
}

// AnthropicServerLoopIterations is Anthropic's documented limit on the
// server-side sampling loop of one request: past it the turn stops with
// pause_turn. max_uses does not shorten the loop: past the last search,
// Claude can keep asking and get max_uses_exceeded back.
const AnthropicServerLoopIterations = 10

// SearchLoop bounds a searching request's provider-side loop: how many
// times the model may be sampled, and whether each sampling re-reads, as
// input, the output the earlier samplings wrote.
type SearchLoop struct {
	Iterations    int64
	RereadsOutput bool
}

// Loop is the row's loop bound. Anthropic's loop runs up to its iteration
// limit and re-reads earlier output; the Responses API's max_tool_calls
// stops the model after max_uses built-in calls, one sampling each, plus
// the answer.
func (w WebSearch) Loop() SearchLoop {
	if w.Tool == AnthropicWebSearchTool {
		return SearchLoop{Iterations: AnthropicServerLoopIterations, RereadsOutput: true}
	}
	return SearchLoop{Iterations: int64(w.MaxUses) + 1}
}

// withSearchFee prices a search into the row's base price and every tier, so
// whichever tier a request is booked at carries the fee.
func withSearchFee(pricing Pricing, tiers []PriceTier, search *WebSearch) (Pricing, []PriceTier) {
	if search == nil {
		return pricing, tiers
	}
	pricing.USDPerSearch = search.USDPerSearch
	for i := range tiers {
		tiers[i].Pricing.USDPerSearch = search.USDPerSearch
	}
	return pricing, tiers
}
