package translate

import (
	"errors"
	"fmt"

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
}

func NewResponsesStreamTranslator(publicModel string, created int64, id string) *ResponsesStreamTranslator {
	return &ResponsesStreamTranslator{
		publicModel:   publicModel,
		created:       created,
		id:            id,
		toolOrdinals:  map[int]int{},
		argsDeltas:    map[int]bool{},
		refusalDeltas: map[int]bool{},
	}
}

func (st *ResponsesStreamTranslator) Next(ev oairesp.StreamEvent) ([]oai.ChatChunk, error) {
	switch ev.Type {
	case "response.output_item.added":
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
		return []oai.ChatChunk{st.chunk(oai.Delta{}, &reason)}, nil
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
