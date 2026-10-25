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

// planWebSearch reads the request's web_search_options and returns the
// request every upstream path is built from, which never carries the
// client's object: only the row's own tool reaches a provider. A gateway or
// key with search off drops the field unread, so the kill switches never
// break a chat; with both on, the options are read strictly, and a model
// without a web_search block or a request that also brings its own tools is
// refused.
func planWebSearch(enabled bool, key store.KeyRecord, publicID string, model models.Model, req oai.ChatRequest) (webSearchPlan, oai.ChatRequest, *webSearchRefusal) {
	raw := bytes.TrimSpace(req.WebSearchOptions)
	req.WebSearchOptions = nil
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return webSearchPlan{}, req, nil
	}
	if !enabled || !key.WebSearch {
		return webSearchPlan{dropped: true}, req, nil
	}
	options, err := oai.ParseWebSearchOptions(raw)
	switch {
	case err != nil:
		return webSearchPlan{}, req, &webSearchRefusal{outcomeWebSearchInvalid, err.Error()}
	case model.WebSearch == nil:
		return webSearchPlan{}, req, &webSearchRefusal{outcomeWebSearchUnsupported, fmt.Sprintf("model %s does not support web search", publicID)}
	case len(req.Tools) > 0:
		return webSearchPlan{}, req, &webSearchRefusal{outcomeWebSearchWithTools, "web_search_options cannot be combined with tools"}
	}
	return webSearchPlan{
		tool: &translate.WebSearchTool{Tool: model.WebSearch.Tool, MaxUses: model.WebSearch.MaxUses, Options: *options},
		row:  model.WebSearch,
	}, req, nil
}

// withWebSearch bounds a searching request: its row's fee for every search
// it may run, and the input its provider's loop may bill (see loopInput),
// for a prompt of the demand's input bound and output of the demand's
// output bound. The loop's input is not held at the context window, since
// every sampling bills it again; the prompt is, since the provider refuses a
// longer one.
func withWebSearch(d accounting.Demand, search *models.WebSearch) accounting.Demand {
	if search == nil {
		return d
	}
	uses := int64(search.MaxUses)
	loop := search.Loop()
	d.InputTokens = loopInput(d.InputTokens, search.InputTokensPerSearch, d.OutputTokens, uses, loop)
	d.WebSearches = uses
	return d
}

// loopInput bounds the input a loop of loop.Iterations samplings bills. The
// k-th sampling (from 0) reads the prompt, the results of the searches run
// before it, at most uses of them at allowance tokens each, and, on a loop
// that re-reads output, what the earlier samplings wrote, at most output in
// all:
//
//	Σ_k (prompt + allowance·min(k, uses)) + (iterations−1)·output
//
// Both the reservation and a cut stream's estimate use it.
func loopInput(prompt, allowance, output, uses int64, loop models.SearchLoop) int64 {
	var results int64
	for k := range loop.Iterations {
		results += min(k, uses)
	}
	input := loop.Iterations*prompt + allowance*results
	if loop.RereadsOutput && loop.Iterations > 1 {
		input += (loop.Iterations - 1) * output
	}
	return input
}
