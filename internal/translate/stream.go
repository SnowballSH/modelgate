package translate

import (
	"errors"
	"fmt"

	"github.com/SnowballSH/modelgate/internal/anthro"
	"github.com/SnowballSH/modelgate/internal/oai"
)

type StreamTranslator struct {
	publicModel string
	created     int64
	id          string
	usage       anthro.Usage
	stopReason  string
	toolBlocks  map[int]int
	toolCount   int
}

func NewStreamTranslator(publicModel string, created int64, id string) *StreamTranslator {
	return &StreamTranslator{
		publicModel: publicModel,
		created:     created,
		id:          id,
		toolBlocks:  map[int]int{},
	}
}

func (st *StreamTranslator) Next(ev anthro.StreamEvent) ([]oai.ChatChunk, error) {
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			st.usage = ev.Message.Usage
		}
		return []oai.ChatChunk{st.chunk(oai.Delta{Role: "assistant"}, nil)}, nil
	case "content_block_start":
		if ev.ContentBlock == nil || ev.ContentBlock.Type != "tool_use" {
			return nil, nil
		}
		pos := st.toolCount
		st.toolCount++
		st.toolBlocks[ev.Index] = pos
		delta := oai.Delta{ToolCalls: []oai.ChunkToolCall{{
			Index:    pos,
			ID:       ev.ContentBlock.ID,
			Type:     "function",
			Function: &oai.ChunkFunctionCall{Name: ev.ContentBlock.Name},
		}}}
		return []oai.ChatChunk{st.chunk(delta, nil)}, nil
	case "content_block_delta":
		if ev.Delta == nil {
			return nil, nil
		}
		switch ev.Delta.Type {
		case "text_delta":
			return []oai.ChatChunk{st.chunk(oai.Delta{Content: ev.Delta.Text}, nil)}, nil
		case "input_json_delta":
			pos, ok := st.toolBlocks[ev.Index]
			if !ok {
				return nil, fmt.Errorf("input_json_delta for unknown block index %d", ev.Index)
			}
			delta := oai.Delta{ToolCalls: []oai.ChunkToolCall{{
				Index:    pos,
				Function: &oai.ChunkFunctionCall{Arguments: ev.Delta.PartialJSON},
			}}}
			return []oai.ChatChunk{st.chunk(delta, nil)}, nil
		}
		return nil, nil
	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			st.stopReason = ev.Delta.StopReason
		}
		if ev.Usage != nil {
			st.usage.OutputTokens = ev.Usage.OutputTokens
			if ev.Usage.InputTokens > 0 {
				st.usage.InputTokens = ev.Usage.InputTokens
			}
			if ev.Usage.CacheCreationInputTokens > 0 {
				st.usage.CacheCreationInputTokens = ev.Usage.CacheCreationInputTokens
			}
			if ev.Usage.CacheReadInputTokens > 0 {
				st.usage.CacheReadInputTokens = ev.Usage.CacheReadInputTokens
			}
		}
		return nil, nil
	case "message_stop":
		reason := st.FinishReason()
		return []oai.ChatChunk{st.chunk(oai.Delta{}, &reason)}, nil
	case "error":
		if ev.Error != nil {
			return nil, fmt.Errorf("upstream stream error (%s): %s", ev.Error.Type, ev.Error.Message)
		}
		return nil, errors.New("upstream stream error")
	default:
		return nil, nil
	}
}

func (st *StreamTranslator) Usage() anthro.Usage {
	return st.usage
}

func (st *StreamTranslator) FinishReason() string {
	return FinishReason(st.stopReason)
}

func (st *StreamTranslator) chunk(delta oai.Delta, finishReason *string) oai.ChatChunk {
	return oai.ChatChunk{
		ID:      st.id,
		Object:  "chat.completion.chunk",
		Created: st.created,
		Model:   st.publicModel,
		Choices: []oai.ChunkChoice{{Index: 0, Delta: delta, FinishReason: finishReason}},
	}
}
