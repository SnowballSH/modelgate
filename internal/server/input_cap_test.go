package server

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SnowballSH/modelgate/internal/anthro"
	"github.com/SnowballSH/modelgate/internal/translate"
)

const (
	fableRow      = `"input_usd_per_mtok":10,"output_usd_per_mtok":50,"cache_read_usd_per_mtok":0.25,"cache_write_usd_per_mtok":20,"default_max_tokens":32000,"image_input":true`
	astraBase     = `"input_usd_per_mtok":10,"output_usd_per_mtok":50,"cache_read_usd_per_mtok":1,"cache_write_usd_per_mtok":12.5,`
	astraTier     = `"input_usd_per_mtok":20,"output_usd_per_mtok":75,"cache_read_usd_per_mtok":2,"cache_write_usd_per_mtok":25}]`
	astraPrice    = astraBase + `"price_tiers":[{"above_input_tokens":272000,` + astraTier
	smallTiered   = astraBase + `"price_tiers":[{"above_input_tokens":10000,` + astraTier
	contextWindow = 1_000_000
	smallWindow   = 20_000
	inputCapJSON  = `{"models":{
		"capped":{"provider_model":"capped","max_input_tokens":1000000,` + fableRow + `},
		"uncapped":{"provider_model":"uncapped",` + fableRow + `},
		"capped-small":{"provider_model":"capped-small","max_input_tokens":20000,` + fableRow + `},
		"capped-tiered":{"provider_model":"capped-tiered","max_input_tokens":1000000,` + astraPrice + `}}}`
	dualInputCapJSON = `{"models":{
		"gpt-capped":{"provider":"openai","upstream_api":"responses","provider_model":"gpt-capped","max_input_tokens":20000,"image_input":true,` + smallTiered + `},
		"gpt-pass-capped":{"provider":"openai","provider_model":"gpt-pass-capped","max_input_tokens":20000,` + smallTiered + `}}}`
)

func textBody(model string, textBytes int) string {
	return fmt.Sprintf(`{"model":%q,"max_tokens":1000,"messages":[{"role":"user","content":%q}]}`, model, strings.Repeat("a", textBytes))
}

func imagesBody(model string, images int) string {
	parts := []string{`{"type":"text","text":"what are these?"}`}
	for range images {
		parts = append(parts, `{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}`)
	}
	return fmt.Sprintf(`{"model":%q,"max_tokens":1000,"messages":[{"role":"user","content":[%s]}]}`, model, strings.Join(parts, ","))
}

func bound(promptTokens int64, inputRate, outputRate float64) float64 {
	return (float64(promptTokens)*inputRate + 1000*outputRate) / 1e6
}

func reservedInFlight(t *testing.T, tableJSON, body string) float64 {
	t.Helper()
	var env *publicEnv
	var reserved float64
	answer := fullResponseHandler()
	env = newPublicEnvWith(t, func(w http.ResponseWriter, r *http.Request) {
		reserved = env.acct.Reserved()
		answer(w, r)
	}, publicEnvOptions{maxBodyBytes: 4 << 20, budgetUSD: 100, rpm: 1000, tableJSON: tableJSON})
	auth, _ := insertTestKey(t, env.store, nil)
	if rec := doPublic(env, http.MethodPost, "/v1/chat/completions", auth, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %.300s", rec.Code, rec.Body.String())
	}
	return reserved
}

func TestMaxInputTokensCapsTheReservedPrompt(t *testing.T) {
	large, small := textBody("capped", 1<<20), textBody("capped", 1000)
	underCap, overCap := imagesBody("capped-small", 2), imagesBody("capped-small", 5)
	tests := []struct {
		name string
		body string
		want float64
	}{
		{"a 1 MiB text body is held at the window", large, bound(contextWindow, 20, 50)},
		{"a small body is unchanged", small, bound(int64(len(small))+promptOverheadTokens, 20, 50)},
		{"an uncapped row still counts every byte", textBody("uncapped", 1<<20), bound(int64(len(textBody("uncapped", 1<<20)))+promptOverheadTokens, 20, 50)},
		{"images under the window keep their allowance", underCap, bound(int64(len(underCap))+2*anthropicImageTokenAllowance+promptOverheadTokens, 20, 50)},
		{"images over the window are held at it", overCap, bound(smallWindow, 20, 50)},
		{"a tiered row reserves the window at its dearest tier", textBody("capped-tiered", 1<<20), bound(contextWindow, 25, 75)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := reservedInFlight(t, inputCapJSON, tc.body); math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("reservation while in flight = USD %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMaxInputTokensCapsTheOpenAIRows(t *testing.T) {
	tests := []struct {
		name string
		body string
		want float64
	}{
		{"responses text over the window", textBody("gpt-capped", 100_000), bound(smallWindow, 25, 75)},
		{"responses images over the window", imagesBody("gpt-capped", 1), bound(smallWindow, 25, 75)},
		{"passthrough text over the window", textBody("gpt-pass-capped", 100_000), bound(smallWindow, 25, 75)},
		{"passthrough text under the window", textBody("gpt-pass-capped", 1000), bound(int64(len(textBody("gpt-pass-capped", 1000)))+promptOverheadTokens, 25, 75)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var env *dualEnv
			var reserved float64
			chat, responses := openaiChatResponse(), openaiResponsesHandler()
			env = newDualEnvWithTable(t, dualInputCapJSON, func(w http.ResponseWriter, r *http.Request) {
				reserved = env.acct.Reserved()
				if r.URL.Path == "/v1/responses" {
					responses(w, r)
					return
				}
				chat(w, r)
			}, fullResponseHandler(), 100)
			auth, _ := insertTestKey(t, env.store, nil)
			if rec := doDual(env, auth, tc.body); rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %.300s", rec.Code, rec.Body.String())
			}
			if math.Abs(reserved-tc.want) > 1e-9 {
				t.Errorf("reservation while in flight = USD %v, want %v", reserved, tc.want)
			}
		})
	}
}

func TestTablesWithoutTheFieldReserveAsBefore(t *testing.T) {
	for _, fixture := range []string{"models_v5.json", "models_v6.json"} {
		contents, err := os.ReadFile(filepath.Join("..", "models", "testdata", fixture))
		if err != nil {
			t.Fatal(err)
		}
		table := tableFrom(t, string(contents))
		for _, id := range table.IDs() {
			t.Run(fixture+"/"+id, func(t *testing.T) {
				var env *dualEnv
				var reserved float64
				chat, responses, answer := openaiChatResponse(), openaiResponsesHandler(), fullResponseHandler()
				hold := func(next http.HandlerFunc) http.HandlerFunc {
					return func(w http.ResponseWriter, r *http.Request) {
						reserved = env.acct.Reserved()
						next(w, r)
					}
				}
				env = newDualEnvWithTable(t, string(contents), hold(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/v1/responses" {
						responses(w, r)
						return
					}
					chat(w, r)
				}), hold(answer), 100)
				auth, _ := insertTestKey(t, env.store, nil)
				body := textBody(id, 900_000)
				if rec := doDual(env, auth, body); rec.Code != http.StatusOK {
					t.Fatalf("status = %d, body %.300s", rec.Code, rec.Body.String())
				}
				row, _ := table.Resolve(id)
				worst := row.WorstCasePricing()
				inputRate := max(worst.InputUSDPerMTok, worst.CacheReadUSDPerMTok, worst.CacheWriteUSDPerMTok)
				want := bound(int64(len(body))+promptOverheadTokens, inputRate, worst.OutputUSDPerMTok)
				if math.Abs(reserved-want) > 1e-9 {
					t.Errorf("reservation = USD %v, want v0.7.0's %v: one token per body byte", reserved, want)
				}
			})
		}
	}
}

// sendHeld sends each request in turn to an upstream that holds every call
// until all have settled, so each later request is admitted against the
// reservations of those still in flight, and returns the statuses in order.
func sendHeld(t *testing.T, env *publicEnv, arrivals *atomic.Int32, release chan struct{}, requests []struct{ auth, body string }) []int {
	t.Helper()
	var returned atomic.Int32
	results := make([]chan int, len(requests))
	for i, req := range requests {
		results[i] = make(chan int, 1)
		go func() {
			results[i] <- doPublic(env, http.MethodPost, "/v1/chat/completions", req.auth, req.body).Code
			returned.Add(1)
		}()
		eventually(t, func() bool { return int(arrivals.Load()+returned.Load()) == i+1 },
			func() string { return fmt.Sprintf("request %d never settled", i) })
	}
	close(release)
	codes := make([]int, len(requests))
	for i, ch := range results {
		codes[i] = <-ch
	}
	return codes
}

func TestContextWindowLetsConcurrentLargeRequestsShareACap(t *testing.T) {
	const ok, held = http.StatusOK, http.StatusTooManyRequests
	tests := []struct {
		name      string
		model     string
		textBytes int
		want      []int
	}{
		{"1 MiB, capped", "capped", 1 << 20, []int{ok, ok, ok}},
		{"1 MiB, uncapped (v0.7.0)", "uncapped", 1 << 20, []int{ok, ok, ok}},
		{"2 MiB, capped", "capped", 2 << 20, []int{ok, ok, ok}},
		{"2 MiB, uncapped (v0.7.0): the key is held by one request", "uncapped", 2 << 20, []int{ok, held, ok}},
		{"3 MiB, capped", "capped", 3 << 20, []int{ok, ok, ok}},
		{"3 MiB, uncapped (v0.7.0): the budget is held by one request", "uncapped", 3 << 20, []int{ok, held, held}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstream, arrivals, release := holdingUpstream()
			env := newPublicEnvWith(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				upstream(w, r)
			}, publicEnvOptions{maxBodyBytes: 4 << 20, budgetUSD: 50, rpm: 1000, tableJSON: inputCapJSON})
			quotaKey := insertQuotaKey(t, env, 40)
			otherKey, _ := insertTestKey(t, env.store, nil)
			large := textBody(tc.model, tc.textBytes)

			codes := sendHeld(t, env, arrivals, release, []struct{ auth, body string }{
				{quotaKey, large}, {quotaKey, large}, {otherKey, textBody(tc.model, 100)},
			})

			if fmt.Sprint(codes) != fmt.Sprint(tc.want) {
				t.Errorf("statuses = %v, want %v", codes, tc.want)
			}
		})
	}
}

func TestCutStreamEstimateIsHeldAtTheWindow(t *testing.T) {
	const window = 1000
	table := `{"models":{"capped":{"provider_model":"capped","max_input_tokens":1000,` + fableRow + `}}}`
	cut := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"abcd\"}}\n\n")
	}
	streamed := func(textBytes int) string {
		return strings.Replace(textBody("capped", textBytes), `"max_tokens"`, `"stream":true,"max_tokens"`, 1)
	}
	under, over := streamed(1000), streamed(40_000)
	tests := []struct {
		name        string
		body        string
		inputTokens int64
	}{
		{"a body under the window keeps its estimate", under, int64((len(under) + 3) / 4)},
		{"a body over the window is estimated at the window", over, window},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newPublicEnvWith(t, cut, publicEnvOptions{maxBodyBytes: 1 << 20, budgetUSD: 100, rpm: 1000, tableJSON: table})
			auth, _ := insertTestKey(t, env.store, nil)
			doPublic(env, http.MethodPost, "/v1/chat/completions", auth, tc.body)
			want := (float64(tc.inputTokens)*10 + 1*50) / 1e6
			if got := monthSpend(t, env); math.Abs(got-want) > 1e-12 {
				t.Errorf("cut stream booked USD %v, want %v (%d input tokens)", got, want, tc.inputTokens)
			}
		})
	}
}

func TestBookingIsNotHeldAtTheWindow(t *testing.T) {
	table := `{"models":{"capped":{"provider_model":"capped","max_input_tokens":1000,
		"input_usd_per_mtok":1,"output_usd_per_mtok":5,"cache_read_usd_per_mtok":0.1,"cache_write_usd_per_mtok":2,
		"price_tiers":[{"above_input_tokens":500,"input_usd_per_mtok":5,"output_usd_per_mtok":25,"cache_read_usd_per_mtok":0.5,"cache_write_usd_per_mtok":10}]}}}`
	usage := anthro.Usage{InputTokens: 4000, CacheReadInputTokens: 3000, CacheCreationInputTokens: 2000, OutputTokens: 10}
	env := newPublicEnvWith(t, anthropicUsage(usage), publicEnvOptions{maxBodyBytes: 1 << 20, budgetUSD: 100, rpm: 1000, tableJSON: table})
	auth, _ := insertTestKey(t, env.store, nil)
	if rec := doPublic(env, http.MethodPost, "/v1/chat/completions", auth, textBody("capped", 10)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	want := (4000*5.0 + 3000*0.5 + 2000*10.0 + 10*25.0) / 1e6
	if got := monthSpend(t, env); math.Abs(got-want) > 1e-12 {
		t.Errorf("spend = %v, want %v: a provider that billed 9,000 prompt tokens past a 1,000-token window is booked in full", got, want)
	}
}

func TestPromptSizeCappedAt(t *testing.T) {
	prompt := measurePrompt(40_000, translate.ImageLoad{Count: 2}, anthropicImageTokenAllowance)
	uncappedWorst, uncappedEstimate := prompt.worstCaseTokens(), prompt.estimatedTokens()
	tests := []struct {
		name                 string
		window               int64
		worstCase, estimated int64
	}{
		{"no window", 0, uncappedWorst, uncappedEstimate},
		{"a window above both", uncappedWorst + 1, uncappedWorst, uncappedEstimate},
		{"a window at the worst case", uncappedWorst, uncappedWorst, uncappedEstimate},
		{"a window between the two", uncappedEstimate + 1, uncappedEstimate + 1, uncappedEstimate},
		{"a window below both", 100, 100, 100},
	}
	for _, tc := range tests {
		capped := prompt.cappedAt(tc.window)
		if capped.worstCaseTokens() != tc.worstCase || capped.estimatedTokens() != tc.estimated {
			t.Errorf("%s: worst case %d, estimate %d; want %d, %d", tc.name, capped.worstCaseTokens(), capped.estimatedTokens(), tc.worstCase, tc.estimated)
		}
	}
}
