//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/SnowballSH/modelgate/internal/accounting"
)

func openaiClient(gw gateway, apiKey string) openai.Client {
	return openai.NewClient(
		option.WithBaseURL("http://"+gw.PublicAddr+"/v1"),
		option.WithAPIKey(apiKey),
	)
}

func TestOpenAIClientNonStreaming(t *testing.T) {
	gw := startGateway(t, 20, defaultTable)
	fullKey := adminCreateKey(t, gw.AdminAddr, "nonstreaming")
	client := openaiClient(gw, fullKey)

	resp, err := client.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
		Model:    testModel,
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices: got %d, want 1", len(resp.Choices))
	}
	if got := resp.Choices[0].Message.Content; got != "hello from fake" {
		t.Errorf("content: got %q, want %q", got, "hello from fake")
	}
	if got := resp.Choices[0].FinishReason; got != "stop" {
		t.Errorf("finish reason: got %q, want %q", got, "stop")
	}
	if resp.Usage.PromptTokens != 100 || resp.Usage.CompletionTokens != 50 {
		t.Errorf("usage: got prompt %d completion %d, want 100 and 50",
			resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	}
}

func TestOpenAIClientStreaming(t *testing.T) {
	gw := startGateway(t, 20, defaultTable)
	fullKey := adminCreateKey(t, gw.AdminAddr, "streaming")
	client := openaiClient(gw, fullKey)

	stream := client.Chat.Completions.NewStreaming(context.Background(), openai.ChatCompletionNewParams{
		Model:    testModel,
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")},
	})
	var content strings.Builder
	finishReason := ""
	for stream.Next() {
		chunk := stream.Current()
		if len(chunk.Choices) == 0 {
			continue
		}
		content.WriteString(chunk.Choices[0].Delta.Content)
		if chunk.Choices[0].FinishReason != "" {
			finishReason = chunk.Choices[0].FinishReason
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if got := content.String(); got != "hello" {
		t.Errorf("accumulated content: got %q, want %q", got, "hello")
	}
	if finishReason != "stop" {
		t.Errorf("finish reason: got %q, want %q", finishReason, "stop")
	}
}

func TestAdminLifecycle(t *testing.T) {
	gw := startGateway(t, 20, defaultTable)
	fullKey := adminCreateKey(t, gw.AdminAddr, "lifecycle")

	var keyList struct {
		Keys []struct {
			ID     string `json:"id"`
			Label  string `json:"label"`
			Prefix string `json:"prefix"`
		} `json:"keys"`
	}
	adminGetJSON(t, gw.AdminAddr, "/api/keys", &keyList)
	keyID := ""
	for _, k := range keyList.Keys {
		if k.Label == "lifecycle" {
			keyID = k.ID
		}
	}
	if keyID == "" {
		t.Fatal("created key not listed")
	}
	secret := fullKey[strings.LastIndex(fullKey, "_")+1:]
	listReq, err := http.NewRequest(http.MethodGet, "http://"+gw.AdminAddr+"/api/keys", nil)
	if err != nil {
		t.Fatal(err)
	}
	asAdmin(listReq)
	listRes, err := http.DefaultClient.Do(listReq)
	if err != nil {
		t.Fatal(err)
	}
	rawList, err := io.ReadAll(listRes.Body)
	listRes.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawList), secret) {
		t.Fatal("key listing leaks the secret")
	}

	res := chatCompletionRaw(t, gw.PublicAddr, fullKey)
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("chat with fresh key: got %d, want 200", res.StatusCode)
	}

	var usage struct {
		Month    string  `json:"month"`
		SpendUSD float64 `json:"spend_usd"`
	}
	adminGetJSON(t, gw.AdminAddr, "/api/usage", &usage)
	if usage.SpendUSD <= 0 {
		t.Errorf("spend after one request: got %v, want > 0", usage.SpendUSD)
	}
	if want := accounting.Month(time.Now()); usage.Month != want {
		t.Errorf("usage month: got %q, want %q", usage.Month, want)
	}

	revokeReq, err := http.NewRequest(http.MethodPost,
		"http://"+gw.AdminAddr+"/api/keys/"+keyID+"/revoke", nil)
	if err != nil {
		t.Fatal(err)
	}
	asAdmin(revokeReq)
	revokeRes, err := http.DefaultClient.Do(revokeReq)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, revokeRes.Body)
	revokeRes.Body.Close()
	if revokeRes.StatusCode != http.StatusOK {
		t.Fatalf("revoke: got %d, want 200", revokeRes.StatusCode)
	}

	res = chatCompletionRaw(t, gw.PublicAddr, fullKey)
	if res.StatusCode != http.StatusUnauthorized {
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		t.Fatalf("chat with revoked key: got %d, want 401", res.StatusCode)
	}
	if code := decodeErrorCode(t, res); code != "invalid_api_key" {
		t.Errorf("revoked key error code: got %q, want %q", code, "invalid_api_key")
	}
}

func TestBudgetHardStop(t *testing.T) {
	gw := startGateway(t, 0.001, defaultTable)
	fullKey := adminCreateKey(t, gw.AdminAddr, "budget")

	res := chatCompletionRaw(t, gw.PublicAddr, fullKey)
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("first request under budget: got %d, want 200", res.StatusCode)
	}

	res = chatCompletionRaw(t, gw.PublicAddr, fullKey)
	if res.StatusCode != http.StatusTooManyRequests {
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		t.Fatalf("second request over budget: got %d, want 429", res.StatusCode)
	}
	if code := decodeErrorCode(t, res); code != "insufficient_quota" {
		t.Errorf("over-budget error code: got %q, want %q", code, "insufficient_quota")
	}
}

func TestReady(t *testing.T) {
	gw := startGateway(t, 20, defaultTable)
	res, err := http.Get("http://" + gw.MetricsAddr + "/ready")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/ready: got %d, want 200", res.StatusCode)
	}
}

func TestModelsEndpoint(t *testing.T) {
	gw := startGateway(t, 20, defaultTable)
	fullKey := adminCreateKey(t, gw.AdminAddr, "models")
	client := openaiClient(gw, fullKey)

	page, err := client.Models.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(page.Data))
	for _, m := range page.Data {
		ids = append(ids, m.ID)
	}
	if len(ids) != 1 || ids[0] != testModel {
		t.Fatalf("models: got %v, want [%s]", ids, testModel)
	}
}

func TestResponsesUpstreamRoundTrip(t *testing.T) {
	g := startGateway(t, 100, farmTable)
	key := adminCreateKey(t, g.AdminAddr, "farm/test")

	res := chatCompletion(t, g.PublicAddr, key, `{"model":"gpt-flag","reasoning_effort":"xhigh","messages":[{"role":"user","content":"weather?"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status: got %d, want 200; body %s", res.StatusCode, body)
	}
	var chat struct {
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(res.Body).Decode(&chat); err != nil {
		t.Fatal(err)
	}
	if chat.Model != "gpt-flag" {
		t.Errorf("model: got %q, want %q", chat.Model, "gpt-flag")
	}
	if len(chat.Choices) != 1 {
		t.Fatalf("choices: got %d, want 1", len(chat.Choices))
	}
	if got := chat.Choices[0].FinishReason; got != "tool_calls" {
		t.Errorf("finish reason: got %q, want %q", got, "tool_calls")
	}
	calls := chat.Choices[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("tool calls: got %d, want 1", len(calls))
	}
	if calls[0].ID != "call_1" || calls[0].Function.Name != "get_weather" {
		t.Errorf("tool call: got id %q name %q, want call_1 and get_weather", calls[0].ID, calls[0].Function.Name)
	}

	seen := g.OpenAI.seen()
	if len(seen) != 1 || seen[0].Path != "/v1/responses" {
		t.Fatalf("upstream requests: got %v, want one to /v1/responses", upstreamPaths(seen))
	}
	var upstream struct {
		Model     string `json:"model"`
		Store     *bool  `json:"store"`
		Reasoning *struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	if err := json.Unmarshal(seen[0].Body, &upstream); err != nil {
		t.Fatal(err)
	}
	if upstream.Model != "gpt-5.6-luna" {
		t.Errorf("upstream model: got %q, want %q", upstream.Model, "gpt-5.6-luna")
	}
	if upstream.Reasoning == nil || upstream.Reasoning.Effort != "xhigh" {
		t.Errorf("upstream reasoning: got %+v, want effort xhigh", upstream.Reasoning)
	}
	if upstream.Store == nil || *upstream.Store {
		t.Errorf("upstream store: got %v, want an explicit false", upstream.Store)
	}

	if got := metricValue(t, g.MetricsAddr, `modelgate_tokens_total{direction="output",model="gpt-flag"}`); got < 50 {
		t.Errorf("booked output tokens: got %v, want at least 50 (reasoning included)", got)
	}
}

func TestChatCompletionsUpstreamStillDefault(t *testing.T) {
	g := startGateway(t, 100, farmTable)
	key := adminCreateKey(t, g.AdminAddr, "farm/default")

	res := chatCompletion(t, g.PublicAddr, key, `{"model":"gpt-plain","messages":[{"role":"user","content":"hi"}]}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status: got %d, want 200; body %s", res.StatusCode, body)
	}
	var chat struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(res.Body).Decode(&chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Choices) != 1 || chat.Choices[0].Message.Content != "hello from fake chat completions" {
		t.Errorf("choices: got %+v, want the chat completions answer", chat.Choices)
	}

	paths := upstreamPaths(g.OpenAI.seen())
	if len(paths) != 1 || paths[0] != "/v1/chat/completions" {
		t.Fatalf("upstream paths: got %v, want only /v1/chat/completions", paths)
	}
}

func TestResponsesUpstreamStream(t *testing.T) {
	for _, includeUsage := range []bool{false, true} {
		t.Run(fmt.Sprintf("include_usage=%v", includeUsage), func(t *testing.T) {
			g := startGateway(t, 100, farmTable)
			key := adminCreateKey(t, g.AdminAddr, "farm/stream")
			body := `{"model":"gpt-flag","stream":true,"messages":[{"role":"user","content":"weather?"}]}`
			if includeUsage {
				body = `{"model":"gpt-flag","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"weather?"}]}`
			}

			res := chatCompletion(t, g.PublicAddr, key, body)
			if res.StatusCode != http.StatusOK {
				payload, _ := io.ReadAll(res.Body)
				res.Body.Close()
				t.Fatalf("status: got %d, want 200; body %s", res.StatusCode, payload)
			}
			payload, err := io.ReadAll(res.Body)
			res.Body.Close()
			if err != nil {
				t.Fatal(err)
			}

			var content, arguments strings.Builder
			finishReason, toolCallID := "", ""
			usageChunks := 0
			sawDone := false
			for line := range strings.Lines(string(payload)) {
				data, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
				if !ok {
					continue
				}
				if data == "[DONE]" {
					sawDone = true
					continue
				}
				if sawDone {
					t.Fatalf("chunk after [DONE]: %s", data)
				}
				var chunk struct {
					Object  string `json:"object"`
					Model   string `json:"model"`
					Choices []struct {
						FinishReason *string `json:"finish_reason"`
						Delta        struct {
							Content   string `json:"content"`
							ToolCalls []struct {
								ID       string `json:"id"`
								Function struct {
									Name      string `json:"name"`
									Arguments string `json:"arguments"`
								} `json:"function"`
							} `json:"tool_calls"`
						} `json:"delta"`
					} `json:"choices"`
					Usage *struct {
						CompletionTokens int64 `json:"completion_tokens"`
					} `json:"usage"`
				}
				if err := json.Unmarshal([]byte(data), &chunk); err != nil {
					t.Fatalf("bad chunk %q: %v", data, err)
				}
				if chunk.Object != "chat.completion.chunk" {
					t.Errorf("chunk object: got %q, want chat.completion.chunk", chunk.Object)
				}
				if chunk.Model != "gpt-flag" {
					t.Errorf("chunk model: got %q, want gpt-flag", chunk.Model)
				}
				if chunk.Usage != nil {
					usageChunks++
					if chunk.Usage.CompletionTokens != 60 {
						t.Errorf("usage chunk completion tokens: got %d, want 60", chunk.Usage.CompletionTokens)
					}
				}
				for _, choice := range chunk.Choices {
					content.WriteString(choice.Delta.Content)
					for _, call := range choice.Delta.ToolCalls {
						if call.ID != "" {
							toolCallID = call.ID
						}
						arguments.WriteString(call.Function.Arguments)
					}
					if choice.FinishReason != nil {
						finishReason = *choice.FinishReason
					}
				}
			}
			if !sawDone {
				t.Error("stream did not end with [DONE]")
			}
			if got := content.String(); got != "Let me check." {
				t.Errorf("streamed content: got %q, want %q", got, "Let me check.")
			}
			if toolCallID != "call_1" || arguments.String() != `{"city":"Paris"}` {
				t.Errorf("streamed tool call: got id %q arguments %q", toolCallID, arguments.String())
			}
			if finishReason != "tool_calls" {
				t.Errorf("finish reason: got %q, want tool_calls", finishReason)
			}
			if want := map[bool]int{false: 0, true: 1}[includeUsage]; usageChunks != want {
				t.Errorf("usage chunks: got %d, want %d", usageChunks, want)
			}

			if got := metricValue(t, g.MetricsAddr, `modelgate_tokens_total{direction="output",model="gpt-flag"}`); got != 60 {
				t.Errorf("booked output tokens: got %v, want exactly 60 (booked once)", got)
			}
			if got := metricValue(t, g.MetricsAddr, `modelgate_tokens_total{direction="input",model="gpt-flag"}`); got != 60 {
				t.Errorf("booked input tokens: got %v, want exactly 60 (100 less 40 cached, booked once)", got)
			}
		})
	}
}

func upstreamPaths(requests []upstreamRequest) []string {
	paths := make([]string, len(requests))
	for i, req := range requests {
		paths[i] = req.Path
	}
	return paths
}
