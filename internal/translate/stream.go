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
	toolBlocks  map[int]*streamedCall
	toolCount   int
	// serverBlocks holds the open server_tool_use blocks, which run on the
	// provider and never reach the client.
	serverBlocks map[int]bool
	// searches counts the web searches requested so far, less those whose
	// result came back as an error, which the provider does not bill.
	searches  int64
	citations map[int][]source
	cited     citationIndex
}

// streamedCall is one open tool_use block: its position among the response's
// tool calls, and whether any argument text has been sent for it yet.
type streamedCall struct {
	position     int
	hasArguments bool
}

func NewStreamTranslator(publicModel string, created int64, id string) *StreamTranslator {
	return &StreamTranslator{
		publicModel:  publicModel,
		created:      created,
		id:           id,
		toolBlocks:   map[int]*streamedCall{},
		serverBlocks: map[int]bool{},
		citations:    map[int][]source{},
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
		if ev.ContentBlock == nil {
			return nil, nil
		}
		switch ev.ContentBlock.Type {
		case "tool_use":
		case "server_tool_use":
			st.serverBlocks[ev.Index] = true
			if ev.ContentBlock.Name == "web_search" {
				st.searches++
			}
			return nil, nil
		case "web_search_tool_result":
			st.countSearchResult(ev.ContentBlock.ResultContent)
			return nil, nil
		case "text":
			st.addCitations(ev.Index, ev.ContentBlock.Citations)
			return nil, nil
		default:
			return nil, nil
		}
		pos := st.toolCount
		st.toolCount++
		st.toolBlocks[ev.Index] = &streamedCall{position: pos}
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
		case "citations_delta":
			if ev.Delta.Citation != nil {
				st.addCitations(ev.Index, []anthro.Citation{*ev.Delta.Citation})
			}
			return nil, nil
		case "input_json_delta":
			if st.serverBlocks[ev.Index] {
				return nil, nil
			}
			call, ok := st.toolBlocks[ev.Index]
			if !ok {
				return nil, fmt.Errorf("input_json_delta for unknown block index %d", ev.Index)
			}
			if ev.Delta.PartialJSON == "" {
				return nil, nil
			}
			call.hasArguments = true
			return []oai.ChatChunk{st.arguments(call.position, ev.Delta.PartialJSON)}, nil
		}
		return nil, nil
	case "content_block_stop":
		if sources, ok := st.citations[ev.Index]; ok {
			delete(st.citations, ev.Index)
			return st.citationChunks(sources), nil
		}
		delete(st.serverBlocks, ev.Index)
		call, ok := st.toolBlocks[ev.Index]
		if !ok || call.hasArguments {
			return nil, nil
		}
		call.hasArguments = true
		return []oai.ChatChunk{st.arguments(call.position, emptyArguments)}, nil
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
			if ev.Usage.ServerToolUse != nil {
				st.usage.ServerToolUse = ev.Usage.ServerToolUse
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

// WebSearchesSeen is the number of billable searches the stream has shown so
// far: what a stream cut before its final usage is booked at.
func (st *StreamTranslator) WebSearchesSeen() int64 {
	return st.searches
}

func (st *StreamTranslator) countSearchResult(content []byte) {
	result, err := anthro.DecodeWebSearchResult(content)
	if err == nil && result.ErrorCode != "" && st.searches > 0 {
		st.searches--
	}
}

func (st *StreamTranslator) addCitations(index int, citations []anthro.Citation) {
	sources := anthropicSources(citations)
	if len(sources) > 0 {
		st.citations[index] = append(st.citations[index], sources...)
	}
}

// citationChunks closes a cited text block: its source markers follow the
// block's text, with an annotation for each source cited here first.
func (st *StreamTranslator) citationChunks(sources []source) []oai.ChatChunk {
	markers, added := st.cited.cite(sources)
	return []oai.ChatChunk{st.chunk(oai.Delta{Content: markers, Annotations: added}, nil)}
}

// arguments carries argument text for the call at position. A call that took
// no arguments streams none upstream, so its block's stop sends "{}": the
// client must see arguments it can parse and echo back.
func (st *StreamTranslator) arguments(position int, text string) oai.ChatChunk {
	return st.chunk(oai.Delta{ToolCalls: []oai.ChunkToolCall{{
		Index:    position,
		Function: &oai.ChunkFunctionCall{Arguments: text},
	}}}, nil)
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
