//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"
)

// TestNoArgumentToolCallRoundTrips drives the loop an agent runs through the
// official client: a streamed call to a tool that takes no arguments, then the
// accumulated assistant message echoed back beside the tool result.
func TestNoArgumentToolCallRoundTrips(t *testing.T) {
	gw := startGateway(t, 20, defaultTable)
	client := openaiClient(gw, adminCreateKey(t, gw.AdminAddr, "no-argument-tool"))
	tools := []openai.ChatCompletionToolUnionParam{openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
		Name:        "get_time",
		Description: openai.String("Current time"),
	})}
	history := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage("You are an agent."),
		openai.UserMessage("what time is it?"),
	}

	stream := client.Chat.Completions.NewStreaming(context.Background(), openai.ChatCompletionNewParams{
		Model:               testModel,
		Messages:            history,
		Tools:               tools,
		MaxCompletionTokens: openai.Int(64),
		ParallelToolCalls:   openai.Bool(false),
	})
	var acc openai.ChatCompletionAccumulator
	for stream.Next() {
		acc.AddChunk(stream.Current())
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if len(acc.Choices) != 1 || acc.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("choices = %+v", acc.Choices)
	}
	calls := acc.Choices[0].Message.ToolCalls
	if len(calls) != 1 || calls[0].Function.Name != "get_time" {
		t.Fatalf("tool calls = %+v", calls)
	}
	if calls[0].Function.Arguments != "{}" {
		t.Fatalf("streamed arguments = %q, want {}", calls[0].Function.Arguments)
	}

	history = append(history, acc.Choices[0].Message.ToParam(), openai.ToolMessage("14:05", calls[0].ID))
	resp, err := client.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
		Model:    testModel,
		Messages: history,
		Tools:    tools,
	})
	if err != nil {
		t.Fatalf("echoing the tool call back: %v", err)
	}
	if resp.Choices[0].Message.Content != "hello from fake" {
		t.Errorf("second turn content = %q", resp.Choices[0].Message.Content)
	}

	seen := gw.Anthropic.seen()
	if len(seen) != 2 {
		t.Fatalf("upstream requests = %d, want 2", len(seen))
	}
	first := decodeUpstream(t, seen[0].Body)
	if first.MaxTokens != 64 {
		t.Errorf("first turn max_tokens = %d, want max_completion_tokens 64", first.MaxTokens)
	}
	if first.ToolChoice == nil || !first.ToolChoice.DisableParallelToolUse {
		t.Errorf("first turn tool_choice = %+v, want disable_parallel_tool_use", first.ToolChoice)
	}
	for i, req := range seen {
		body := decodeUpstream(t, req.Body)
		if body.CacheControl == nil || body.CacheControl.Type != "ephemeral" {
			t.Errorf("request %d cache_control = %+v, want the automatic breakpoint", i, body.CacheControl)
		}
		if len(body.System) != 1 || body.System[0].CacheControl == nil {
			t.Errorf("request %d system = %+v, want the prefix breakpoint", i, body.System)
		}
	}
	second := decodeUpstream(t, seen[1].Body)
	echoed := second.Messages[1].Content
	if len(echoed) != 1 || echoed[0].Type != "tool_use" || string(echoed[0].Input) != "{}" {
		t.Errorf("echoed assistant turn = %+v, want one tool_use with input {}", echoed)
	}
}

type upstreamMessages struct {
	MaxTokens    int `json:"max_tokens"`
	CacheControl *struct {
		Type string `json:"type"`
	} `json:"cache_control"`
	System []struct {
		Text         string          `json:"text"`
		CacheControl json.RawMessage `json:"cache_control"`
	} `json:"system"`
	ToolChoice *struct {
		Type                   string `json:"type"`
		DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
	} `json:"tool_choice"`
	Messages []struct {
		Role    string `json:"role"`
		Content []struct {
			Type  string          `json:"type"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"messages"`
}

func decodeUpstream(t *testing.T, body []byte) upstreamMessages {
	t.Helper()
	var out upstreamMessages
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode upstream body: %v", err)
	}
	return out
}
