package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/SnowballSH/modelgate/internal/accounting"
	"github.com/SnowballSH/modelgate/internal/keys"
	"github.com/SnowballSH/modelgate/internal/provider"
	"github.com/SnowballSH/modelgate/internal/store"
)

var updateCompat = flag.Bool("update-compat", false, "rewrite the compat goldens from the current build")

// compatModelJSON mirrors the shape of the production table's rows: an
// Anthropic row with the hour-long cache, images and a tier, a Responses row
// and a Chat Completions passthrough row.
const compatModelJSON = `{"models":{
	"claude-row":{"provider_model":"claude-real","sampling":false,"image_input":true,"default_max_tokens":16000,"cache_ttl":"1h","max_input_tokens":1048576,
		"input_usd_per_mtok":4,"output_usd_per_mtok":20,"cache_read_usd_per_mtok":0.2,"cache_write_usd_per_mtok":8,
		"price_tiers":[{"above_input_tokens":200000,"input_usd_per_mtok":8,"output_usd_per_mtok":30,"cache_read_usd_per_mtok":0.4,"cache_write_usd_per_mtok":16}]},
	"gpt-row":{"provider":"openai","upstream_api":"responses","provider_model":"gpt-real","image_input":true,"default_max_tokens":32000,"max_input_tokens":1050000,
		"input_usd_per_mtok":2,"output_usd_per_mtok":12,"cache_read_usd_per_mtok":0.2,"cache_write_usd_per_mtok":2.5},
	"pass-row":{"provider":"openai","provider_model":"gpt-pass-2026","input_usd_per_mtok":1,"output_usd_per_mtok":4,"cache_read_usd_per_mtok":0.1,"cache_write_usd_per_mtok":1.25}}}`

type compatCase struct {
	name     string
	body     string
	upstream string
}

func compatRequest(model string, stream bool, rest string) string {
	return fmt.Sprintf(`{"model":%q,"stream":%t,%s}`, model, stream, rest)
}

const (
	compatPlain     = `"messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"hi"}]`
	compatTools     = `"tools":[{"type":"function","function":{"name":"get_weather","description":"Weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}},{"type":"function","function":{"name":"get_time"}}],"tool_choice":"auto","parallel_tool_calls":false`
	compatRoundtrip = `"messages":[{"role":"user","content":"weather in Paris?"},{"role":"assistant","content":"Checking.","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":""},{"role":"user","content":"thanks"}]`
	compatFields    = `"temperature":0.2,"top_p":0.9,"seed":7,"stop":["END"],"user":"alice","reasoning_effort":"high","max_completion_tokens":500,"prompt_cache_key":"conv-1","stream_options":{"include_usage":true},"unknown_field":{"x":1}`
)

var compatCases = []compatCase{
	{"anthropic_plain", compatRequest("claude-row", false, compatPlain), "anthropic_text.json"},
	{"anthropic_plain_stream", compatRequest("claude-row", true, compatPlain), "anthropic_text.sse"},
	{"anthropic_fields_stream", compatRequest("claude-row", true, compatFields+","+compatPlain), "anthropic_text.sse"},
	{"anthropic_tools", compatRequest("claude-row", false, compatTools+","+compatRoundtrip), "anthropic_tool_use.json"},
	{"anthropic_tools_stream", compatRequest("claude-row", true, compatTools+","+compatRoundtrip), "anthropic_tool_use.sse"},
	{"responses_plain", compatRequest("gpt-row", false, compatPlain), "responses_text.json"},
	{"responses_tools", compatRequest("gpt-row", false, compatTools+","+compatRoundtrip), "responses_tool_call.json"},
	{"responses_refusal", compatRequest("gpt-row", false, compatPlain), "responses_refusal.json"},
	{"responses_tools_stream", compatRequest("gpt-row", true, compatFields+","+compatTools+","+compatRoundtrip), "responses_tool_call.events"},
	{"responses_refusal_stream", compatRequest("gpt-row", true, compatPlain), "responses_refusal.events"},
	{"passthrough_plain", compatRequest("pass-row", false, compatFields+","+compatPlain), "chat_text.json"},
	{"passthrough_stream", compatRequest("pass-row", true, compatTools+","+compatPlain), "chat_text.sse"},
}

// TestCompatGoldens pins the wire behaviour of requests that do not ask for
// web search to what v0.8.0 produced: the upstream request bytes, the client
// response bytes and headers, and the booked usage. The goldens were written
// by v0.8.0 itself; regenerate them only for a deliberate wire change.
func TestCompatGoldens(t *testing.T) {
	for _, tc := range compatCases {
		t.Run(tc.name, func(t *testing.T) {
			got := runCompatCase(t, tc)
			path := filepath.Join("testdata", "compat", "golden", tc.name+".golden")
			if *updateCompat {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("wire behaviour changed\n--- got\n%s\n--- want\n%s", got, want)
			}
		})
	}
}

var completionID = regexp.MustCompile(`chatcmpl-[0-9a-f]{16}`)

func runCompatCase(t *testing.T, tc compatCase) []byte {
	t.Helper()
	fixture, err := os.ReadFile(filepath.Join("testdata", "compat", "upstream", tc.upstream))
	if err != nil {
		t.Fatal(err)
	}
	var upstreamPath string
	var upstreamBody []byte
	serve := func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		upstreamBody, _ = io.ReadAll(r.Body)
		switch filepath.Ext(tc.upstream) {
		case ".json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture)
		case ".sse":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write(fixture)
		case ".events":
			w.Header().Set("Content-Type", "text/event-stream")
			lines := bufio.NewScanner(bytes.NewReader(fixture))
			for lines.Scan() {
				fmt.Fprintf(w, "data: %s\n\n", lines.Text())
			}
		}
	}
	anthropicUp := httptest.NewServer(http.HandlerFunc(serve))
	t.Cleanup(anthropicUp.Close)
	openaiUp := httptest.NewServer(http.HandlerFunc(serve))
	t.Cleanup(openaiUp.Close)

	s := testStore(t)
	table := tableFrom(t, compatModelJSON)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return now }
	acct := accounting.New(s, 100, nil)
	guards := NewGuards(s, acct, table, 1000, 8, nowFn)
	up := Upstreams{
		Anthropic:        provider.NewClient(anthropicUp.URL, "ant-key", anthropicUp.Client()),
		AnthropicBreaker: provider.NewBreaker(100, time.Minute, nowFn),
		OpenAI:           provider.NewOpenAIClient(openaiUp.URL, "oai-key", openaiUp.Client()),
		OpenAIBreaker:    provider.NewBreaker(100, time.Minute, nowFn),
	}
	handler := NewPublicHandler(guards, table, acct, s, up, NewMetrics(100), PublicConfig{
		DefaultMaxTokens: 4096, MaxOutputTokens: 128_000, MaxBodyBytes: 1 << 20,
		RequestDeadline: 5 * time.Second, Version: "compat",
	}, nowFn)
	gen, err := keys.Generate(bytes.NewReader(bytes.Repeat([]byte{7}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertKey(t.Context(), store.KeyRecord{
		ID: gen.ID, Prefix: gen.Prefix, SecretSHA256: gen.SecretSHA256[:],
		Label: "compat", CreatedAt: now, CreatedBy: "test",
	}); err != nil {
		t.Fatal(err)
	}
	auth := "Bearer " + gen.Full

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body))
	req.Header.Set("Authorization", auth)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	usage, err := s.MonthUsage(t.Context(), accounting.Month(now))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	fmt.Fprintf(&out, "upstream %s\n%s\n", upstreamPath, upstreamBody)
	fmt.Fprintf(&out, "status %d\ncontent-type %s\ndropped %s\n", rec.Code,
		rec.Header().Get("Content-Type"), rec.Header().Get("X-Modelgate-Dropped-Params"))
	out.Write(completionID.ReplaceAll(rec.Body.Bytes(), []byte("chatcmpl-ID")))
	fmt.Fprintf(&out, "usage %s\n", compatUsage(usage[gen.ID]))
	return out.Bytes()
}

func compatUsage(u store.Usage) string {
	encoded, _ := json.Marshal(map[string]any{
		"requests": u.Requests, "input": u.InputTokens, "output": u.OutputTokens,
		"cache_read": u.CacheReadTokens, "cache_write": u.CacheWriteTokens,
		"cost_usd": fmt.Sprintf("%.9f", u.CostUSD),
	})
	return string(encoded)
}
