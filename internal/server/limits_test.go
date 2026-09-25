package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/SnowballSH/modelgate/internal/accounting"
	"github.com/SnowballSH/modelgate/internal/oai"
	"github.com/SnowballSH/modelgate/internal/store"
)

const limitedModelJSON = `{"models":{
	"claude-opus-5-5":{"provider_model":"claude-opus-5-5","forced_tool_choice":false,"input_usd_per_mtok":4,"output_usd_per_mtok":20,"cache_read_usd_per_mtok":0.2,"cache_write_usd_per_mtok":5},
	"gpt-6-luna":{"provider":"openai","upstream_api":"responses","provider_model":"gpt-6-luna","reasoning_efforts":["none","low","medium","high","xhigh","max"],"input_usd_per_mtok":0.1,"output_usd_per_mtok":0.5,"cache_read_usd_per_mtok":0.01,"cache_write_usd_per_mtok":0.1},
	"gpt-chat":{"provider":"openai","provider_model":"gpt-chat","forced_tool_choice":false,"reasoning_efforts":["low"],"input_usd_per_mtok":1,"output_usd_per_mtok":1,"cache_read_usd_per_mtok":1,"cache_write_usd_per_mtok":1}}}`

const weatherTool = `[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]`

func limitedRequest(model string, stream bool, extra string) string {
	return fmt.Sprintf(`{"model":%q,"stream":%t,"tools":%s,%s,"messages":[{"role":"user","content":"weather?"}]}`, model, stream, weatherTool, extra)
}

func TestDeclaredLimitsRefuseBeforeAnyUpstreamCall(t *testing.T) {
	tests := []struct {
		name    string
		model   string
		extra   string
		message string
	}{
		{"required tool_choice on anthropic", "claude-opus-5-5", `"tool_choice":"required"`,
			`model claude-opus-5-5 does not support forced tool_choice; use "auto"`},
		{"named tool_choice on anthropic", "claude-opus-5-5", `"tool_choice":{"type":"function","function":{"name":"get_weather"}}`,
			`model claude-opus-5-5 does not support forced tool_choice; use "auto"`},
		{"undeclared effort on responses", "gpt-6-luna", `"reasoning_effort":"minimal"`,
			`model gpt-6-luna does not support reasoning_effort "minimal"; use one of none, low, medium, high, xhigh, max`},
		{"effort outside the vocabulary on responses", "gpt-6-luna", `"reasoning_effort":"extreme"`,
			`model gpt-6-luna does not support reasoning_effort "extreme"; use one of none, low, medium, high, xhigh, max`},
		{"forced tool_choice on chat completions", "gpt-chat", `"tool_choice":"required"`,
			`model gpt-chat does not support forced tool_choice; use "auto"`},
		{"undeclared effort on chat completions", "gpt-chat", `"reasoning_effort":"high"`,
			`model gpt-chat does not support reasoning_effort "high"; use one of low`},
	}
	for _, tc := range tests {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				env := newDualEnvWithTable(t, limitedModelJSON, openaiChatResponse(), fullResponseHandler(), 100)
				auth, _ := insertTestKey(t, env.store, nil)

				rec := doDual(env, auth, limitedRequest(tc.model, stream, tc.extra))
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
				}
				var body oai.ErrorBody
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode %q: %v", rec.Body.String(), err)
				}
				if body.Error.Type != "invalid_request_error" || body.Error.Code != CodeInvalidRequest {
					t.Errorf("error type/code = %q/%q, want invalid_request_error/invalid_request_error", body.Error.Type, body.Error.Code)
				}
				if body.Error.Message != tc.message {
					t.Errorf("message = %q, want %q", body.Error.Message, tc.message)
				}
				if *env.anthropicHits != 0 || len(*env.openaiCalls) != 0 {
					t.Errorf("upstream calls: anthropic %d, openai %v; want none", *env.anthropicHits, upstreamPaths(*env.openaiCalls))
				}
				spend, err := env.store.MonthSpend(t.Context(), accounting.Month(env.now))
				if err != nil {
					t.Fatal(err)
				}
				if spend != 0 {
					t.Errorf("spend = %v, want nothing booked for a refused request", spend)
				}
			})
		}
	}
}

func TestRequestsWithinDeclaredLimitsReachTheUpstream(t *testing.T) {
	tests := []struct {
		name  string
		model string
		extra string
	}{
		{"auto tool_choice on anthropic", "claude-opus-5-5", `"tool_choice":"auto"`},
		{"declared effort on anthropic without an effort list", "claude-opus-5-5", `"reasoning_effort":"minimal"`},
		{"declared effort on responses", "gpt-6-luna", `"reasoning_effort":"none"`},
		{"forced tool_choice where the table allows it", "gpt-6-luna", `"tool_choice":"required"`},
		{"declared effort on chat completions", "gpt-chat", `"reasoning_effort":"low"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newDualEnvWithTable(t, limitedModelJSON, dispatchOpenAI(), fullResponseHandler(), 100)
			auth, _ := insertTestKey(t, env.store, nil)

			rec := doDual(env, auth, limitedRequest(tc.model, false, tc.extra))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
			}
			if *env.anthropicHits+len(*env.openaiCalls) != 1 {
				t.Errorf("upstream calls: anthropic %d, openai %v; want exactly one", *env.anthropicHits, upstreamPaths(*env.openaiCalls))
			}
		})
	}
}

func dispatchOpenAI() http.HandlerFunc {
	chat, responses := openaiChatResponse(), openaiResponsesHandler()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/responses" {
			responses(w, r)
			return
		}
		chat(w, r)
	}
}

func TestLimitRefusalFollowsTheAllowlistAndPrecedesTheCaps(t *testing.T) {
	exhausted := 0.0
	tests := []struct {
		name     string
		mutate   func(*store.KeyRecord)
		extra    string
		wantCode string
	}{
		{"unsatisfiable request on an exhausted key", func(k *store.KeyRecord) { k.QuotaUSD = &exhausted },
			`"tool_choice":"required"`, CodeInvalidRequest},
		{"satisfiable request on an exhausted key", func(k *store.KeyRecord) { k.QuotaUSD = &exhausted },
			`"tool_choice":"auto"`, insufficientQuota},
		{"unsatisfiable request for a model outside the allowlist", func(k *store.KeyRecord) { k.Models = []string{"gpt-chat"} },
			`"tool_choice":"required"`, CodeModelNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newDualEnvWithTable(t, limitedModelJSON, openaiChatResponse(), fullResponseHandler(), 100)
			gen := insertKey(t, env.store, env.now, tc.mutate)

			rec := doDual(env, bearer(gen.Full), limitedRequest("claude-opus-5-5", false, tc.extra))
			var body oai.ErrorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode %q: %v", rec.Body.String(), err)
			}
			if body.Error.Code != tc.wantCode {
				t.Errorf("status/code = %d/%q, want code %q", rec.Code, body.Error.Code, tc.wantCode)
			}
			if *env.anthropicHits != 0 {
				t.Errorf("anthropic upstream calls = %d, want none", *env.anthropicHits)
			}
		})
	}
}
