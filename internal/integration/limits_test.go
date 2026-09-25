//go:build integration

package integration

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"
)

const limitedTable = `{"models":{
	"claude-opus-5-5":{"provider_model":"claude-opus-5-5","forced_tool_choice":false,
		"input_usd_per_mtok":4,"output_usd_per_mtok":20,
		"cache_read_usd_per_mtok":0.2,"cache_write_usd_per_mtok":5},
	"gpt-6-luna":{"provider":"openai","upstream_api":"responses","provider_model":"gpt-6-luna",
		"reasoning_efforts":["none","low","medium","high","xhigh","max"],
		"input_usd_per_mtok":0.1,"output_usd_per_mtok":0.5,
		"cache_read_usd_per_mtok":0.01,"cache_write_usd_per_mtok":0.1}}}`

func TestDeclaredLimitsReachTheSDKAsBadRequests(t *testing.T) {
	gw := startGateway(t, 20, limitedTable)
	client := openaiClient(gw, adminCreateKey(t, gw.AdminAddr, "limits"))
	weather := openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{Name: "get_weather"})

	tests := []struct {
		name    string
		params  openai.ChatCompletionNewParams
		message string
	}{
		{
			name: "forced tool_choice on claude-opus-5-5",
			params: openai.ChatCompletionNewParams{
				Model:      "claude-opus-5-5",
				Messages:   []openai.ChatCompletionMessageParamUnion{openai.UserMessage("weather?")},
				Tools:      []openai.ChatCompletionToolUnionParam{weather},
				ToolChoice: openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: openai.String("required")},
			},
			message: `model claude-opus-5-5 does not support forced tool_choice; use "auto"`,
		},
		{
			name: "minimal effort on gpt-6-luna",
			params: openai.ChatCompletionNewParams{
				Model:           "gpt-6-luna",
				Messages:        []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")},
				ReasoningEffort: shared.ReasoningEffortMinimal,
			},
			message: `model gpt-6-luna does not support reasoning_effort "minimal"; use one of none, low, medium, high, xhigh, max`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.Chat.Completions.New(context.Background(), tc.params)
			assertBadRequest(t, err, tc.message)

			stream := client.Chat.Completions.NewStreaming(context.Background(), tc.params)
			for stream.Next() {
				t.Fatalf("streamed a chunk for a refused request: %+v", stream.Current())
			}
			assertBadRequest(t, stream.Err(), tc.message)
		})
	}

	if seen := gw.Anthropic.seen(); len(seen) != 0 {
		t.Errorf("anthropic upstream requests: got %v, want none", upstreamPaths(seen))
	}
	if seen := gw.OpenAI.seen(); len(seen) != 0 {
		t.Errorf("openai upstream requests: got %v, want none", upstreamPaths(seen))
	}
}

func assertBadRequest(t *testing.T, err error, message string) {
	t.Helper()
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error: got %v, want an *openai.Error", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest || apiErr.Type != "invalid_request_error" {
		t.Errorf("status/type: got %d/%q, want 400/invalid_request_error", apiErr.StatusCode, apiErr.Type)
	}
	if apiErr.Message != message {
		t.Errorf("message: got %q, want %q", apiErr.Message, message)
	}
}
