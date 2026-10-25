package server

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SnowballSH/modelgate/internal/accounting"
	"github.com/SnowballSH/modelgate/internal/keys"
	"github.com/SnowballSH/modelgate/internal/provider"
	"github.com/SnowballSH/modelgate/internal/store"
)

const searchBlock = `"web_search":{"tool":%q,"usd_per_search":0.01,"max_uses":3,"input_tokens_per_search":25000}`

var searchModelJSON = `{"models":{
	"claude-search":{"provider_model":"claude-real","sampling":false,"cache_ttl":"1h","max_input_tokens":1048576,` + fmt.Sprintf(searchBlock, "web_search_20250305") + `,
		"input_usd_per_mtok":4,"output_usd_per_mtok":20,"cache_read_usd_per_mtok":0.2,"cache_write_usd_per_mtok":8},
	"claude-plain":{"provider_model":"claude-plain","input_usd_per_mtok":4,"output_usd_per_mtok":20,"cache_read_usd_per_mtok":0.2,"cache_write_usd_per_mtok":8},
	"gpt-search":{"provider":"openai","upstream_api":"responses","provider_model":"gpt-real",` + fmt.Sprintf(searchBlock, "web_search") + `,
		"input_usd_per_mtok":2,"output_usd_per_mtok":10,"cache_read_usd_per_mtok":0.2,"cache_write_usd_per_mtok":2.5},
	"gpt-plain":{"provider":"openai","upstream_api":"responses","provider_model":"gpt-plain","input_usd_per_mtok":2,"output_usd_per_mtok":10,"cache_read_usd_per_mtok":0.2,"cache_write_usd_per_mtok":2.5},
	"pass-row":{"provider":"openai","provider_model":"gpt-pass","input_usd_per_mtok":1,"output_usd_per_mtok":4,"cache_read_usd_per_mtok":0.1,"cache_write_usd_per_mtok":1.25}}}`

type searchEnv struct {
	handler http.Handler
	store   *store.Store
	acct    *accounting.Accountant
	metrics *Metrics
	now     time.Time

	mu    sync.Mutex
	calls []upstreamCall
}

func (e *searchEnv) upstreamCalls() []upstreamCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]upstreamCall(nil), e.calls...)
}

type searchEnvOptions struct {
	enabled   bool
	anthropic http.HandlerFunc
	openai    http.HandlerFunc
	budgetUSD float64
}

func fixtureHandler(t *testing.T, name string) http.HandlerFunc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "translate", "testdata", "websearch", name))
	if err != nil {
		t.Fatal(err)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(name, ".events") {
			w.Header().Set("Content-Type", "text/event-stream")
			for line := range strings.Lines(string(raw)) {
				fmt.Fprintf(w, "data: %s\n\n", strings.TrimSpace(line))
			}
			return
		}
		if strings.HasSuffix(name, ".sse") {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		_, _ = w.Write(raw)
	}
}

func newSearchEnv(t *testing.T, opts searchEnvOptions) *searchEnv {
	t.Helper()
	env := &searchEnv{now: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)}
	capture := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			env.mu.Lock()
			env.calls = append(env.calls, upstreamCall{path: r.URL.Path, body: body})
			env.mu.Unlock()
			r.Body = io.NopCloser(bytes.NewReader(body))
			next(w, r)
		}
	}
	if opts.anthropic == nil {
		opts.anthropic = fixtureHandler(t, "anthropic_response.json")
	}
	if opts.openai == nil {
		chat, responses := openaiChatResponse(), fixtureHandler(t, "responses_response.json")
		opts.openai = func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/responses" {
				responses(w, r)
				return
			}
			chat(w, r)
		}
	}
	if opts.budgetUSD == 0 {
		opts.budgetUSD = 100
	}
	anthropicUp := httptest.NewServer(capture(opts.anthropic))
	t.Cleanup(anthropicUp.Close)
	openaiUp := httptest.NewServer(capture(opts.openai))
	t.Cleanup(openaiUp.Close)

	env.store = testStore(t)
	table := tableFrom(t, searchModelJSON)
	nowFn := func() time.Time { return env.now }
	env.acct = accounting.New(env.store, opts.budgetUSD, nil)
	guards := NewGuards(env.store, env.acct, table, 1000, 8, nowFn)
	up := Upstreams{
		Anthropic:        provider.NewClient(anthropicUp.URL, "ant-key", anthropicUp.Client()),
		AnthropicBreaker: provider.NewBreaker(100, time.Minute, nowFn),
		OpenAI:           provider.NewOpenAIClient(openaiUp.URL, "oai-key", openaiUp.Client()),
		OpenAIBreaker:    provider.NewBreaker(100, time.Minute, nowFn),
	}
	env.metrics = NewMetrics(opts.budgetUSD)
	env.handler = NewPublicHandler(guards, table, env.acct, env.store, up, env.metrics, PublicConfig{
		DefaultMaxTokens: 4096, MaxOutputTokens: 128_000, MaxBodyBytes: 1 << 20,
		RequestDeadline: 5 * time.Second, Version: "test", WebSearchEnabled: opts.enabled,
	}, nowFn)
	return env
}

func (e *searchEnv) key(t *testing.T, webSearch bool, quotaUSD *float64) (auth, id string) {
	t.Helper()
	gen, err := keys.Generate(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.InsertKey(t.Context(), store.KeyRecord{
		ID: gen.ID, Prefix: gen.Prefix, SecretSHA256: gen.SecretSHA256[:], Label: "search",
		QuotaUSD: quotaUSD, WebSearch: webSearch, CreatedAt: e.now, CreatedBy: "test",
	}); err != nil {
		t.Fatal(err)
	}
	return "Bearer " + gen.Full, gen.ID
}

func (e *searchEnv) post(auth, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", auth)
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func searchRequest(model string, stream bool, options string) string {
	field := ""
	if options != "" {
		field = `"web_search_options":` + options + `,`
	}
	return fmt.Sprintf(`{"model":%q,"stream":%t,%s"messages":[{"role":"user","content":"news?"}]}`, model, stream, field)
}

// TestRawWebSearchOptionsNeverReachAnUpstream drives every row kind through
// every switch setting: whatever the gateway decides, the client's own
// web_search_options object is never forwarded, and search reaches an
// upstream only as the tool the row's block names, with both switches on.
func TestRawWebSearchOptionsNeverReachAnUpstream(t *testing.T) {
	const options = `{"search_context_size":"low","user_location":{"type":"approximate","approximate":{"country":"GB"}}}`
	switches := []struct {
		name          string
		global, onKey bool
	}{{"gateway off", false, true}, {"key off", true, false}, {"both off", false, false}, {"both on", true, true}}
	rows := []struct {
		model     string
		path      string
		searchTag string
	}{
		{"claude-search", "/v1/messages", `"type":"web_search_20250305"`},
		{"claude-plain", "/v1/messages", ""},
		{"gpt-search", "/v1/responses", `"type":"web_search"`},
		{"gpt-plain", "/v1/responses", ""},
		{"pass-row", "/v1/chat/completions", ""},
	}
	for _, sw := range switches {
		for _, row := range rows {
			for _, opts := range []string{options, `null`} {
				t.Run(sw.name+"/"+row.model+"/"+opts[:4], func(t *testing.T) {
					env := newSearchEnv(t, searchEnvOptions{enabled: sw.global})
					auth, _ := env.key(t, sw.onKey, nil)
					rec := env.post(auth, searchRequest(row.model, false, opts))
					calls := env.upstreamCalls()
					for _, call := range calls {
						if bytes.Contains(call.body, []byte("web_search_options")) || bytes.Contains(call.body, []byte(`"search_context_size":"low","user_location":{"type":"approximate","approximate"`)) {
							t.Fatalf("the raw field reached %s: %s", call.path, call.body)
						}
					}
					searching := sw.global && sw.onKey && opts != `null`
					switch {
					case searching && row.searchTag == "":
						if rec.Code != http.StatusBadRequest || len(calls) != 0 {
							t.Fatalf("search on a row without a block: status %d, %d upstream calls; want 400 and none", rec.Code, len(calls))
						}
						if !strings.Contains(rec.Body.String(), "does not support web search") {
							t.Errorf("refusal body = %s", rec.Body.String())
						}
						return
					case rec.Code != http.StatusOK || len(calls) != 1 || calls[0].path != row.path:
						t.Fatalf("status %d, upstream calls %v; want 200 and one call to %s (body %s)", rec.Code, upstreamPaths(calls), row.path, rec.Body.String())
					}
					hasTool := bytes.Contains(calls[0].body, []byte(row.searchTag)) && row.searchTag != ""
					if hasTool != searching {
						t.Errorf("search tool sent = %v, want %v: %s", hasTool, searching, calls[0].body)
					}
					dropped := rec.Header().Get("X-Modelgate-Dropped-Params")
					switch {
					case opts == `null`:
						if strings.Contains(dropped, "web_search_options") {
							t.Errorf("a null field is named dropped: %q", dropped)
						}
					case !searching:
						if !strings.Contains(dropped, "web_search_options") {
							t.Errorf("a switched-off search is not named dropped: %q", dropped)
						}
					case row.model == "claude-search":
						if dropped != "web_search_options.search_context_size" {
							t.Errorf("dropped = %q, want the context size Anthropic cannot honour", dropped)
						}
					default:
						if dropped != "" {
							t.Errorf("dropped = %q, want nothing", dropped)
						}
					}
				})
			}
		}
	}
}

func TestSwitchedOffSearchDropsEvenAMalformedField(t *testing.T) {
	env := newSearchEnv(t, searchEnvOptions{enabled: false})
	auth, _ := env.key(t, true, nil)
	rec := env.post(auth, searchRequest("pass-row", false, `{"bogus":1}`))
	if rec.Code != http.StatusOK || rec.Header().Get("X-Modelgate-Dropped-Params") != "web_search_options" {
		t.Fatalf("status %d dropped %q; the kill switch must keep chats working", rec.Code, rec.Header().Get("X-Modelgate-Dropped-Params"))
	}
}

func TestSearchRefusals(t *testing.T) {
	tests := []struct {
		name, body, message, outcome string
	}{
		{"malformed options", searchRequest("claude-search", false, `{"max_uses":9}`), `invalid web_search_options: json: unknown field`, "web_search_invalid"},
		{"not an object", searchRequest("gpt-search", false, `true`), "web_search_options must be an object", "web_search_invalid"},
		{"a row without a block", searchRequest("claude-plain", false, `{}`), "model claude-plain does not support web search", "web_search_unsupported"},
		{"client tools alongside search", `{"model":"claude-search","web_search_options":{},"tools":[{"type":"function","function":{"name":"f"}}],"messages":[{"role":"user","content":"hi"}]}`,
			"web_search_options cannot be combined with tools", "web_search_with_tools"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newSearchEnv(t, searchEnvOptions{enabled: true})
			auth, _ := env.key(t, true, nil)
			rec := env.post(auth, tc.body)
			if rec.Code != http.StatusBadRequest || len(env.upstreamCalls()) != 0 {
				t.Fatalf("status %d with %d upstream calls; want 400 and none", rec.Code, len(env.upstreamCalls()))
			}
			if !strings.Contains(rec.Body.String(), tc.message) || decodeErrorCode(t, rec) != CodeInvalidRequest {
				t.Errorf("body = %s, want %q as invalid_request_error", rec.Body.String(), tc.message)
			}
			model := strings.Split(strings.Split(tc.body, `"model":"`)[1], `"`)[0]
			if got := scrape(t, env.metrics, fmt.Sprintf(`modelgate_requests_total{model=%q,outcome=%q}`, model, tc.outcome)); got != 1 {
				t.Errorf("outcome %s counted %v times", tc.outcome, got)
			}
		})
	}
}

func TestSearchIsBookedWithItsFee(t *testing.T) {
	tests := []struct {
		name, model, fixture string
		stream               bool
		wantSearches         int64
		wantCost             float64
	}{
		{"anthropic", "claude-search", "anthropic_response.json", false, 1,
			(6039*4.0+931*20.0+7123*0.2+7345*8.0)/1e6 + 0.01},
		{"anthropic stream", "claude-search", "anthropic_stream.sse", true, 1,
			(10682*4.0+510*20.0)/1e6 + 0.01},
		{"responses", "gpt-search", "responses_response.json", false, 2,
			(6000*2.0+300*10.0+1000*0.2+2000*2.5)/1e6 + 0.02},
		{"responses stream", "gpt-search", "responses_stream.events", true, 1,
			(5000*2.0+120*10.0)/1e6 + 0.01},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := fixtureHandler(t, tc.fixture)
			env := newSearchEnv(t, searchEnvOptions{enabled: true, anthropic: handler, openai: handler})
			auth, id := env.key(t, true, nil)
			rec := env.post(auth, searchRequest(tc.model, tc.stream, `{}`))
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), `"url_citation"`) {
				t.Errorf("the answer carries no citation: %s", rec.Body.String())
			}
			usage, err := env.store.MonthUsage(t.Context(), accounting.Month(env.now))
			if err != nil {
				t.Fatal(err)
			}
			got := usage[id]
			if got.WebSearchRequests != tc.wantSearches || math.Abs(got.CostUSD-tc.wantCost) > 1e-12 {
				t.Errorf("booked %d searches at $%v, want %d at $%v", got.WebSearchRequests, got.CostUSD, tc.wantSearches, tc.wantCost)
			}
			if n := scrape(t, env.metrics, fmt.Sprintf(`modelgate_web_searches_total{model=%q}`, tc.model)); n != float64(tc.wantSearches) {
				t.Errorf("modelgate_web_searches_total = %v, want %d", n, tc.wantSearches)
			}
		})
	}
}

// A stream cut before its final usage still bills the searches it ran: the
// second sampling re-read the prompt, the search's results and the one
// output token the first sampling had reported.
// Claude can issue several searches in one sampling, so every later sampling
// re-reads all of them: two parallel searches cut before the final usage book
// as three samplings, each after the first reading both results and the one
// output token reported.
func TestCutParallelSearchStreamBooksEveryResultOnEveryLaterSampling(t *testing.T) {
	env := newSearchEnv(t, searchEnvOptions{enabled: true, anthropic: fixtureHandler(t, "anthropic_parallel_cut.sse")})
	auth, id := env.key(t, true, nil)
	env.post(auth, searchRequest("claude-search", true, `{}`))
	usage, err := env.store.MonthUsage(t.Context(), accounting.Month(env.now))
	if err != nil {
		t.Fatal(err)
	}
	got := usage[id]
	wantInput := int64(3*1000 + 25_000*2*2 + 2*1)
	if got.WebSearchRequests != 2 || got.InputTokens != wantInput {
		t.Errorf("cut parallel stream booked %+v; want 2 searches and %d input tokens", got, wantInput)
	}
}

func TestCutSearchStreamBooksTheSearchesSeen(t *testing.T) {
	events := []string{
		`{"type":"message_start","message":{"type":"message","role":"assistant","usage":{"input_tokens":1000,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"x\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"web_search_result","url":"https://a.example","title":"A","encrypted_content":"x"}]}}`,
		`{"type":"content_block_stop","index":1}`,
	}
	env := newSearchEnv(t, searchEnvOptions{enabled: true, anthropic: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range events {
			fmt.Fprintf(w, "data: %s\n\n", ev)
		}
	}})
	auth, id := env.key(t, true, nil)
	env.post(auth, searchRequest("claude-search", true, `{}`))

	usage, err := env.store.MonthUsage(t.Context(), accounting.Month(env.now))
	if err != nil {
		t.Fatal(err)
	}
	got := usage[id]
	wantInput := int64(2*1000 + 25_000 + 1)
	if got.WebSearchRequests != 1 || got.InputTokens != wantInput {
		t.Errorf("cut stream booked %+v; want 1 search and %d input tokens", got, wantInput)
	}
}

// The reservation for a searching request is not capped at the context
// window: with U searches the loop can bill the prompt U+1 times, plus each
// search's allowance on every later iteration, plus U fees.
func TestSearchReservationCoversTheWholeLoop(t *testing.T) {
	for _, tc := range []struct {
		name, options string
		searches      int64
	}{{"plain", "", 0}, {"search", `{}`, 3}} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			arrived := make(chan struct{})
			answer := fixtureHandler(t, "anthropic_response.json")
			env := newSearchEnv(t, searchEnvOptions{enabled: true, anthropic: func(w http.ResponseWriter, r *http.Request) {
				close(arrived)
				<-release
				answer(w, r)
			}})
			auth, _ := env.key(t, true, nil)
			body := searchRequest("claude-search", false, tc.options)
			done := make(chan struct{})
			go func() {
				defer close(done)
				env.post(auth, body)
			}()
			waitFor(t, arrived, "the upstream call never started")
			prompt := float64(len(body) + promptOverheadTokens)
			input := prompt
			if tc.searches > 0 {
				input = 10*prompt + 25_000*3*9 + 9*4096
			}
			want := (input*8+4096*20)/1e6 + float64(tc.searches)*0.01
			if got := env.acct.Reserved(); math.Abs(got-want) > 1e-12 {
				t.Errorf("reserved $%v, want $%v", got, want)
			}
			close(release)
			waitFor(t, done, "the handler never returned")
		})
	}
}
