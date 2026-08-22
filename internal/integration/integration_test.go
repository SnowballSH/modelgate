//go:build integration

package integration

import (
	"context"
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
	gw := startGateway(t, 20)
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
	gw := startGateway(t, 20)
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
	gw := startGateway(t, 20)
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
	listReq.Header.Set("Remote-User", "tester")
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
	revokeReq.Header.Set("Remote-User", "tester")
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
	gw := startGateway(t, 0.001)
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
	if code := decodeErrorCode(t, res); code != "budget_exhausted" {
		t.Errorf("over-budget error code: got %q, want %q", code, "budget_exhausted")
	}
}

func TestReady(t *testing.T) {
	gw := startGateway(t, 20)
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
	gw := startGateway(t, 20)
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
