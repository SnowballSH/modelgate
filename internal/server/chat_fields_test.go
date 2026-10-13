package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/SnowballSH/modelgate/internal/anthro"
	"github.com/SnowballSH/modelgate/internal/oairesp"
	"github.com/SnowballSH/modelgate/internal/store"
	"github.com/SnowballSH/modelgate/internal/translate"
)

const chatReadyModelJSON = `{"models":{
	"claude-chat":{"provider_model":"claude-chat","sampling":false,"image_input":true,"default_max_tokens":16000,"cache_ttl":"1h","input_usd_per_mtok":2,"output_usd_per_mtok":10,"cache_read_usd_per_mtok":0.1,"cache_write_usd_per_mtok":4},
	"claude-plain":{"provider_model":"claude-plain","input_usd_per_mtok":2,"output_usd_per_mtok":10,"cache_read_usd_per_mtok":0.2,"cache_write_usd_per_mtok":2.5},
	"gpt-vision":{"provider":"openai","upstream_api":"responses","provider_model":"gpt-vision","image_input":true,"default_max_tokens":8000,"input_usd_per_mtok":2,"output_usd_per_mtok":10,"cache_read_usd_per_mtok":0.1,"cache_write_usd_per_mtok":2.5},
	"gpt-pass":{"provider":"openai","provider_model":"gpt-pass","sampling":false,"input_usd_per_mtok":1,"output_usd_per_mtok":1,"cache_read_usd_per_mtok":1,"cache_write_usd_per_mtok":1}}}`

type upstreamCapture struct {
	bodies [][]byte
}

func (c *upstreamCapture) anthropic() http.HandlerFunc {
	answer := fullResponseHandler()
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.bodies = append(c.bodies, body)
		var req anthro.MessagesRequest
		if json.Unmarshal(body, &req) != nil || !req.Stream {
			answer(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"message_start","message":{"type":"message","role":"assistant","usage":{"input_tokens":10,"output_tokens":1}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"type":"message_delta","stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
			`{"type":"message_stop"}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
	}
}

func lastAnthropicRequest(t *testing.T, c *upstreamCapture) anthro.MessagesRequest {
	t.Helper()
	if len(c.bodies) == 0 {
		t.Fatal("the anthropic upstream saw no request")
	}
	var req anthro.MessagesRequest
	if err := json.Unmarshal(c.bodies[len(c.bodies)-1], &req); err != nil {
		t.Fatal(err)
	}
	return req
}

func lastResponsesRequest(t *testing.T, env *dualEnv) oairesp.Request {
	t.Helper()
	calls := *env.openaiCalls
	if len(calls) == 0 || calls[len(calls)-1].path != "/v1/responses" {
		t.Fatalf("openai upstream calls = %v, want a last call to /v1/responses", upstreamPaths(calls))
	}
	var req oairesp.Request
	if err := json.Unmarshal(calls[len(calls)-1].body, &req); err != nil {
		t.Fatal(err)
	}
	return req
}

func chatReadyEnv(t *testing.T) (*dualEnv, *upstreamCapture, string) {
	t.Helper()
	capture := &upstreamCapture{}
	env := newDualEnvWithTable(t, chatReadyModelJSON, dispatchOpenAI(), capture.anthropic(), 100)
	auth, _ := insertTestKey(t, env.store, nil)
	return env, capture, auth
}

func chatReadyRequest(model, fields string) string {
	return fmt.Sprintf(`{"model":%q,%s"messages":[{"role":"user","content":"hi"}]}`, model, fields)
}

func TestRowDefaultMaxTokensReachesEachUpstream(t *testing.T) {
	tests := []struct {
		name   string
		model  string
		fields string
		want   int
	}{
		{"row default on anthropic", "claude-chat", ``, 16_000},
		{"one cap below the row default", "claude-chat", `"max_completion_tokens":100,`, 100},
		{"both caps prefer max_completion_tokens", "claude-chat", `"max_tokens":9000,"max_completion_tokens":100,`, 100},
		{"row without a default", "claude-plain", ``, 4096},
		{"row default on responses", "gpt-vision", ``, 8000},
		{"passthrough row without a default", "gpt-pass", ``, 4096},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, capture, auth := chatReadyEnv(t)
			rec := doDual(env, auth, chatReadyRequest(tc.model, tc.fields))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
			}
			var got int
			switch {
			case len(capture.bodies) > 0:
				got = lastAnthropicRequest(t, capture).MaxTokens
			case len(*env.openaiSeen) > 0:
				got = *(*env.openaiSeen)[0].MaxCompletionTokens
			default:
				got = *lastResponsesRequest(t, env).MaxOutputTokens
			}
			if got != tc.want {
				t.Errorf("upstream output cap = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestDroppedParamsAreNamedInTheResponseHeader(t *testing.T) {
	tests := []struct {
		name   string
		model  string
		fields string
		want   string
	}{
		{"anthropic row without sampling", "claude-chat", `"temperature":0.7,"seed":7,"frequency_penalty":0.5,`, "temperature, frequency_penalty, seed"},
		{"anthropic stream", "claude-chat", `"stream":true,"temperature":0.7,`, "temperature"},
		{"anthropic row that forwards sampling", "claude-plain", `"temperature":0.7,`, ""},
		{"responses row", "gpt-vision", `"temperature":0.7,"stop":"END",`, "temperature, stop"},
		{"passthrough row without sampling", "gpt-pass", `"temperature":0.7,"seed":7,`, "temperature"},
		{"reasoning effort displaces sampling on a row that forwards it", "claude-plain", `"reasoning_effort":"high","temperature":0.7,"top_p":0.9,`, "temperature, top_p"},
		{"nothing to drop", "claude-chat", ``, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, _, auth := chatReadyEnv(t)
			rec := doDual(env, auth, chatReadyRequest(tc.model, tc.fields))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
			}
			values := rec.Header().Values(translate.DroppedParamsHeader)
			if tc.want == "" && len(values) != 0 {
				t.Fatalf("%s = %v, want the header absent", translate.DroppedParamsHeader, values)
			}
			if tc.want != "" && (len(values) != 1 || values[0] != tc.want) {
				t.Errorf("%s = %v, want %q", translate.DroppedParamsHeader, values, tc.want)
			}
		})
	}
}

func TestSamplingFollowsTheRow(t *testing.T) {
	env, capture, auth := chatReadyEnv(t)

	doDual(env, auth, chatReadyRequest("claude-chat", `"temperature":0.7,"top_p":0.9,`))
	if got := lastAnthropicRequest(t, capture); got.Temperature != nil || got.TopP != nil {
		t.Errorf("claude-chat (sampling: false) received temperature %v, top_p %v", got.Temperature, got.TopP)
	}
	doDual(env, auth, chatReadyRequest("claude-plain", `"temperature":0.7,`))
	if got := lastAnthropicRequest(t, capture); got.Temperature == nil || *got.Temperature != 0.7 {
		t.Errorf("claude-plain lost its temperature: %v", got.Temperature)
	}
	doDual(env, auth, chatReadyRequest("gpt-pass", `"temperature":0.7,"seed":7,`))
	seen := *env.openaiSeen
	if len(seen) != 1 || seen[0].Temperature != nil || seen[0].Seed == nil || *seen[0].Seed != 7 {
		t.Errorf("gpt-pass (sampling: false) upstream request = %+v; want no temperature and the seed forwarded", seen)
	}
}

func TestCacheLifetimeFollowsTheRow(t *testing.T) {
	env, capture, auth := chatReadyEnv(t)

	doDual(env, auth, `{"model":"claude-chat","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"hi"}]}`)
	got := lastAnthropicRequest(t, capture)
	if got.CacheControl == nil || got.CacheControl.TTL != "1h" || len(got.System) != 1 || got.System[0].CacheControl == nil || got.System[0].CacheControl.TTL != "1h" {
		t.Errorf("claude-chat breakpoints = request %+v, system %+v; want 1h on both", got.CacheControl, got.System)
	}

	doDual(env, auth, `{"model":"claude-plain","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"hi"}]}`)
	if raw := capture.bodies[len(capture.bodies)-1]; bytes.Contains(raw, []byte(`"ttl"`)) || !bytes.Contains(raw, []byte(`"cache_control"`)) {
		t.Errorf("claude-plain must cache at the default lifetime with no ttl on the wire: %s", raw)
	}
}

func TestPromptCacheKeyIsNamespacedPerGatewayKey(t *testing.T) {
	env, capture, first := chatReadyEnv(t)
	second, _ := insertTestKey(t, env.store, nil)
	const fields = `"prompt_cache_key":"thread-1",`

	doDual(env, first, chatReadyRequest("gpt-vision", fields))
	firstKey := lastResponsesRequest(t, env).PromptCacheKey
	doDual(env, second, chatReadyRequest("gpt-vision", fields))
	secondKey := lastResponsesRequest(t, env).PromptCacheKey
	if len(firstKey) != 32 || firstKey == "thread-1" || firstKey == secondKey {
		t.Errorf("responses prompt_cache_key = %q and %q; want two distinct 32-character digests", firstKey, secondKey)
	}

	doDual(env, first, chatReadyRequest("gpt-pass", fields))
	seen := *env.openaiSeen
	if len(seen) != 1 || seen[0].PromptCacheKey != firstKey {
		t.Errorf("passthrough prompt_cache_key = %+v, want %q", seen, firstKey)
	}

	doDual(env, first, chatReadyRequest("claude-chat", fields))
	if raw := capture.bodies[len(capture.bodies)-1]; bytes.Contains(raw, []byte("prompt_cache_key")) || bytes.Contains(raw, []byte("thread-1")) {
		t.Errorf("prompt_cache_key reached the anthropic upstream: %s", raw)
	}
}

func TestUntranslatableRequestIsRefusedBeforeTheCaps(t *testing.T) {
	exhausted := 0.0
	capture := &upstreamCapture{}
	env := newDualEnvWithTable(t, chatReadyModelJSON, dispatchOpenAI(), capture.anthropic(), 100)
	gen := insertKey(t, env.store, env.now, func(k *store.KeyRecord) { k.QuotaUSD = &exhausted })

	rec := doDual(env, bearer(gen.Full), chatReadyRequest("claude-chat", `"n":2,`))
	if rec.Code != http.StatusBadRequest || decodeErrorCode(t, rec) != CodeInvalidRequest {
		t.Errorf("status %d code %s; want the 400 a request no retry can satisfy, not the key's exhausted quota", rec.Code, decodeErrorCode(t, rec))
	}
	if len(capture.bodies) != 0 {
		t.Error("an untranslatable request reached the upstream")
	}
}

func TestPassthroughForwardsLogitBias(t *testing.T) {
	env, _, auth := chatReadyEnv(t)

	doDual(env, auth, chatReadyRequest("gpt-pass", `"logit_bias":{"50256":-100},`))
	seen := *env.openaiSeen
	if len(seen) != 1 || string(seen[0].LogitBias) != `{"50256":-100}` {
		t.Errorf("gpt-pass upstream logit_bias = %+v, want the client's object", seen)
	}
}

func TestReasoningEffortDisplacesSamplingOnTheWire(t *testing.T) {
	env, capture, auth := chatReadyEnv(t)

	doDual(env, auth, chatReadyRequest("claude-plain", `"reasoning_effort":"high","temperature":0.7,"top_p":0.9,`))
	if got := lastAnthropicRequest(t, capture); got.Temperature != nil || got.TopP != nil {
		t.Errorf("claude-plain with a reasoning effort received temperature %v, top_p %v", got.Temperature, got.TopP)
	}
}
