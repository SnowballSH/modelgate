package server

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SnowballSH/modelgate/internal/accounting"
	"github.com/SnowballSH/modelgate/internal/anthro"
	"github.com/SnowballSH/modelgate/internal/models"
)

const tieredModelJSON = `{"models":{"tiered":{"provider_model":"tiered",
	"input_usd_per_mtok":1,"output_usd_per_mtok":5,"cache_read_usd_per_mtok":0.1,"cache_write_usd_per_mtok":2,
	"price_tiers":[{"above_input_tokens":100000,"input_usd_per_mtok":5,"output_usd_per_mtok":25,"cache_read_usd_per_mtok":0.5,"cache_write_usd_per_mtok":10}]}}}`

func tableFrom(t *testing.T, contents string) *models.Table {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	table, err := models.LoadTable(path)
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func anthropicUsage(usage anthro.Usage) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(anthro.MessagesResponse{
			ID: "msg_1", Type: "message", Role: "assistant",
			Content:    []anthro.ContentBlock{{Type: "text", Text: "ok"}},
			StopReason: "end_turn",
			Usage:      usage,
		})
	}
}

func TestBookingRepricesTheWholeRequestAboveATier(t *testing.T) {
	tests := []struct {
		name  string
		usage anthro.Usage
		want  float64
	}{
		{"at the threshold", anthro.Usage{InputTokens: 1000, CacheReadInputTokens: 99_000, OutputTokens: 10},
			(1000*1.0 + 99_000*0.1 + 10*5.0) / 1e6},
		{"crossed only by cached tokens", anthro.Usage{InputTokens: 1000, CacheReadInputTokens: 98_000, CacheCreationInputTokens: 1001, OutputTokens: 10},
			(1000*5.0 + 98_000*0.5 + 1001*10.0 + 10*25.0) / 1e6},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newDualEnvWithTable(t, tieredModelJSON, openaiChatResponse(), anthropicUsage(tc.usage), 100)
			auth, _ := insertTestKey(t, env.store, nil)
			rec := doDual(env, auth, `{"model":"tiered","messages":[{"role":"user","content":"hi"}]}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
			}
			spend, err := env.store.MonthSpend(t.Context(), accounting.Month(env.now))
			if err != nil {
				t.Fatal(err)
			}
			if math.Abs(spend-tc.want) > 1e-12 {
				t.Errorf("spend = %v, want %v", spend, tc.want)
			}
		})
	}
}

func TestAdmitReservesAtTheTopTier(t *testing.T) {
	s := newTestStore(t)
	clk := &clock{t: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)}
	acct := accounting.New(s, 100)
	g := NewGuards(s, acct, tableFrom(t, tieredModelJSON), 60, 4, clk.now)
	gen := insertKey(t, s, clk.t, nil)

	adm, refusal := admitBearer(g, bearer(gen.Full), "tiered", accounting.Demand{InputTokens: 1000, OutputTokens: 2000})
	if refusal != nil {
		t.Fatalf("Admit refused: %+v", refusal)
	}
	defer adm.Release()
	want := (1000*10.0 + 2000*25.0) / 1e6
	if got := acct.Reserved(); math.Abs(got-want) > 1e-12 {
		t.Errorf("reserved = %v, want %v (a 1000-token prompt still reserves at the tier it cannot be proven to stay under)", got, want)
	}
}

func TestCutStreamBooksAnEstimatedPromptAtTheTopTier(t *testing.T) {
	const body = `{"model":"tiered","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	text := strings.Repeat("a", 40)
	tests := []struct {
		name   string
		events string
		want   float64
	}{
		{"no usage reported", "",
			(float64((len(body)+3)/4)*5.0 + 10*25.0) / 1e6},
		{"input reported before the cut",
			"data: {\"type\":\"message_start\",\"message\":{\"type\":\"message\",\"role\":\"assistant\",\"usage\":{\"input_tokens\":100,\"output_tokens\":1}}}\n\n",
			(100*1.0 + 10*5.0) / 1e6},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cut := func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tc.events)
				fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n")
				fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", text)
			}
			env := newDualEnvWithTable(t, tieredModelJSON, openaiChatResponse(), cut, 100)
			auth, _ := insertTestKey(t, env.store, nil)
			doDual(env, auth, body)
			spend, err := env.store.MonthSpend(t.Context(), accounting.Month(env.now))
			if err != nil {
				t.Fatal(err)
			}
			if math.Abs(spend-tc.want) > 1e-12 {
				t.Errorf("spend = %v, want %v", spend, tc.want)
			}
		})
	}
}
