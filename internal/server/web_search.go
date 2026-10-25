package server

import (
	"bytes"
	"fmt"

	"github.com/SnowballSH/modelgate/internal/accounting"
	"github.com/SnowballSH/modelgate/internal/models"
	"github.com/SnowballSH/modelgate/internal/oai"
	"github.com/SnowballSH/modelgate/internal/store"
	"github.com/SnowballSH/modelgate/internal/translate"
)

// Outcomes that refuse a request's web search, counted apart from other 400s.
const (
	outcomeWebSearchInvalid     = "web_search_invalid"
	outcomeWebSearchUnsupported = "web_search_unsupported"
	outcomeWebSearchWithTools   = "web_search_with_tools"
)

const droppedWebSearchOptions = "web_search_options"

// webSearchPlan is what the gateway decided about a request's
// web_search_options: the search to run, nil for none, and whether a
// switched-off gateway or key dropped the field.
type webSearchPlan struct {
	tool    *translate.WebSearchTool
	row     *models.WebSearch
	dropped bool
}

type webSearchRefusal struct {
	outcome, message string
}

// planWebSearch reads the request's web_search_options. A gateway or key with
// search off drops the field unread, so the kill switches never break a chat;
// with both on, the options are read strictly, and a model without a
// web_search block or a request that also brings its own tools is refused.
func planWebSearch(enabled bool, key store.KeyRecord, publicID string, model models.Model, req oai.ChatRequest) (webSearchPlan, *webSearchRefusal) {
	raw := bytes.TrimSpace(req.WebSearchOptions)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return webSearchPlan{}, nil
	}
	if !enabled || !key.WebSearch {
		return webSearchPlan{dropped: true}, nil
	}
	options, err := oai.ParseWebSearchOptions(raw)
	switch {
	case err != nil:
		return webSearchPlan{}, &webSearchRefusal{outcomeWebSearchInvalid, err.Error()}
	case model.WebSearch == nil:
		return webSearchPlan{}, &webSearchRefusal{outcomeWebSearchUnsupported, fmt.Sprintf("model %s does not support web search", publicID)}
	case len(req.Tools) > 0:
		return webSearchPlan{}, &webSearchRefusal{outcomeWebSearchWithTools, "web_search_options cannot be combined with tools"}
	}
	return webSearchPlan{
		tool: &translate.WebSearchTool{Tool: model.WebSearch.Tool, MaxUses: model.WebSearch.MaxUses, Options: *options},
		row:  model.WebSearch,
	}, nil
}

// withWebSearch bounds a searching request. The provider's server-side loop
// bills the whole prompt again on every iteration, and each search adds its
// results to every later one, so U searches bill at most
// (U+1)·P + A·U(U+1)/2 input tokens for a prompt of P and an allowance of A
// per search, and U fees. The loop's input is not bounded by the context
// window, so neither is this; P is, since the provider refuses a longer
// prompt.
func withWebSearch(d accounting.Demand, search *models.WebSearch) accounting.Demand {
	if search == nil {
		return d
	}
	uses := int64(search.MaxUses)
	d.InputTokens = (uses+1)*d.InputTokens + search.InputTokensPerSearch*uses*(uses+1)/2
	d.WebSearches = uses
	return d
}

// searchLoopInput estimates the input a search loop billed by the time it
// had run searches searches on a prompt of prompt tokens, on the same model
// withWebSearch reserves by.
func searchLoopInput(prompt, searches, allowance int64) int64 {
	return (searches+1)*prompt + allowance*searches*(searches+1)/2
}
