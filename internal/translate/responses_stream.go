package translate

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

	"github.com/SnowballSH/modelgate/internal/oai"
	"github.com/SnowballSH/modelgate/internal/oairesp"
)

type ResponsesStreamTranslator struct {
	publicModel   string
	created       int64
	id            string
	sentRole      bool
	toolOrdinals  map[int]int
	toolCount     int
	argsDeltas    map[int]bool
	refusalDeltas map[int]bool
	usage         *oairesp.Usage
	status        string
	incomplete    *oairesp.IncompleteDetails
	// searchesSeen counts the web_search_call items opened so far;
	// searches is the count the terminal response reported, once it has.
	searchesSeen int64
	searches     *int64
	citations    map[textPart][]source
	cited        citationIndex
}

// textPart names one output_text part: its item's output index and its
// index among the item's content parts.
type textPart struct {
	output, content int
}

func NewResponsesStreamTranslator(publicModel string, created int64, id string) *ResponsesStreamTranslator {
	return &ResponsesStreamTranslator{
		publicModel:   publicModel,
		created:       created,
		id:            id,
		toolOrdinals:  map[int]int{},
		argsDeltas:    map[int]bool{},
		refusalDeltas: map[int]bool{},
		citations:     map[textPart][]source{},
	}
}

func (st *ResponsesStreamTranslator) Next(ev oairesp.StreamEvent) ([]oai.ChatChunk, error) {
	switch ev.Type {
	case "response.output_item.added":
		if ev.Item != nil && ev.Item.Type == "web_search_call" {
			st.searchesSeen++
		}
		if ev.Item == nil || ev.Item.Type != "function_call" {
			return nil, nil
		}
		pos := st.toolCount
		st.toolCount++
		st.toolOrdinals[ev.OutputIndex] = pos
		delta := oai.Delta{ToolCalls: []oai.ChunkToolCall{{
			Index:    pos,
			ID:       ev.Item.CallID,
			Type:     "function",
			Function: &oai.ChunkFunctionCall{Name: ev.Item.Name},
		}}}
		return []oai.ChatChunk{st.chunk(delta, nil)}, nil
	case "response.output_text.delta":
		return []oai.ChatChunk{st.chunk(oai.Delta{Content: ev.Delta}, nil)}, nil
	case "response.output_text.annotation.added":
		if ev.Annotation != nil {
			part := textPart{ev.OutputIndex, ev.ContentIndex}
			st.citations[part] = append(st.citations[part], responsesSources([]oairesp.Annotation{*ev.Annotation})...)
		}
		return nil, nil
	case "response.output_text.done":
		return st.citationChunks(func(part textPart) bool {
			return part == textPart{ev.OutputIndex, ev.ContentIndex}
		}), nil
	case "response.output_item.done":
		return st.citationChunks(func(part textPart) bool { return part.output == ev.OutputIndex }), nil
	case "response.refusal.delta":
		st.refusalDeltas[ev.OutputIndex] = true
		refusal := ev.Delta
		return []oai.ChatChunk{st.chunk(oai.Delta{Refusal: &refusal}, nil)}, nil
	case "response.refusal.done":
		if st.refusalDeltas[ev.OutputIndex] {
			return nil, nil
		}
		refusal := ev.Refusal
		return []oai.ChatChunk{st.chunk(oai.Delta{Refusal: &refusal}, nil)}, nil
	case "response.function_call_arguments.delta":
		chunks, err := st.argumentsChunk(ev.OutputIndex, ev.Delta)
		if err != nil {
			return nil, err
		}
		st.argsDeltas[ev.OutputIndex] = true
		return chunks, nil
	case "response.function_call_arguments.done":
		if st.argsDeltas[ev.OutputIndex] {
			return nil, nil
		}
		return st.argumentsChunk(ev.OutputIndex, ev.Arguments)
	case "response.completed", "response.incomplete":
		st.record(ev.Response)
		reason := st.FinishReason()
		chunks := st.citationChunks(func(textPart) bool { return true })
		return append(chunks, st.chunk(oai.Delta{}, &reason)), nil
	case "response.failed":
		st.record(ev.Response)
		if ev.Response != nil && ev.Response.Error != nil {
			return nil, fmt.Errorf("upstream response failed (%s): %s", ev.Response.Error.Code, ev.Response.Error.Message)
		}
		return nil, errors.New("upstream response failed")
	case "error":
		if ev.Message != "" {
			return nil, fmt.Errorf("upstream stream error (%s): %s", ev.Code, ev.Message)
		}
		return nil, errors.New("upstream stream error")
	default:
		return nil, nil
	}
}

func (st *ResponsesStreamTranslator) Usage() *oairesp.Usage {
	return st.usage
}

func (st *ResponsesStreamTranslator) FinishReason() string {
	return responsesFinishReason(st.status, st.incomplete, st.toolCount > 0)
}

// WebSearchesSeen is the number of web_search_call items the stream has
// opened so far: what a stream cut before its terminal event is booked at.
func (st *ResponsesStreamTranslator) WebSearchesSeen() int64 {
	return st.searchesSeen
}

// WebSearchCallsSeen equals WebSearchesSeen: every call is counted as billed.
func (st *ResponsesStreamTranslator) WebSearchCallsSeen() int64 {
	return st.searchesSeen
}

// WebSearches is the count to book: the terminal response's web_search_call
// items once it arrived with its output, otherwise the items seen.
func (st *ResponsesStreamTranslator) WebSearches() int64 {
	if st.searches != nil {
		return *st.searches
	}
	return st.searchesSeen
}

// citationChunks closes the cited text parts that match: each part's source
// markers follow its text, with an annotation for each source cited first
// there. Parts close in output order.
func (st *ResponsesStreamTranslator) citationChunks(matches func(textPart) bool) []oai.ChatChunk {
	var parts []textPart
	for part := range st.citations {
		if matches(part) {
			parts = append(parts, part)
		}
	}
	slices.SortFunc(parts, func(a, b textPart) int {
		return cmp.Or(cmp.Compare(a.output, b.output), cmp.Compare(a.content, b.content))
	})
	var chunks []oai.ChatChunk
	for _, part := range parts {
		sources := st.citations[part]
		delete(st.citations, part)
		if markers, added := st.cited.cite(sources); markers != "" {
			chunks = append(chunks, st.chunk(oai.Delta{Content: markers, Annotations: added}, nil))
		}
	}
	return chunks
}

func (st *ResponsesStreamTranslator) argumentsChunk(outputIndex int, arguments string) ([]oai.ChatChunk, error) {
	pos, ok := st.toolOrdinals[outputIndex]
	if !ok {
		return nil, fmt.Errorf("function call arguments for unopened output index %d", outputIndex)
	}
	delta := oai.Delta{ToolCalls: []oai.ChunkToolCall{{
		Index:    pos,
		Function: &oai.ChunkFunctionCall{Arguments: arguments},
	}}}
	return []oai.ChatChunk{st.chunk(delta, nil)}, nil
}

// record keeps whatever usage a terminal event reports, including a failed or
// truncated one: a run that burns a reasoning budget and returns no text is
// still billed upstream, and the fallback estimate would book almost nothing.
func (st *ResponsesStreamTranslator) record(resp *oairesp.Response) {
	if resp == nil {
		return
	}
	st.status = resp.Status
	st.incomplete = resp.IncompleteDetails
	if len(resp.Output) > 0 {
		n := ResponsesWebSearches(resp.Output)
		st.searches = &n
	}
	if resp.Usage != nil {
		st.usage = resp.Usage
	}
}

func (st *ResponsesStreamTranslator) chunk(delta oai.Delta, finishReason *string) oai.ChatChunk {
	if !st.sentRole {
		delta.Role = "assistant"
		st.sentRole = true
	}
	return oai.ChatChunk{
		ID:      st.id,
		Object:  "chat.completion.chunk",
		Created: st.created,
		Model:   st.publicModel,
		Choices: []oai.ChunkChoice{{Index: 0, Delta: delta, FinishReason: finishReason}},
	}
}
