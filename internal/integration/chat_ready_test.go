//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/SnowballSH/modelgate/internal/anthro"
)

const chatReadyTable = `{"models":{"claude-chat":{"provider_model":"claude-chat",
	"sampling":false,"image_input":true,"default_max_tokens":16000,"cache_ttl":"1h",
	"input_usd_per_mtok":2,"output_usd_per_mtok":10,
	"cache_read_usd_per_mtok":0.1,"cache_write_usd_per_mtok":4}}}`

const chatClientTurn = `{"model":"claude-chat","temperature":0.7,"seed":7,"frequency_penalty":0.2,"messages":[
	{"role":"system","content":"Be brief."},
	{"role":"user","content":"hello"},
	{"role":"assistant","content":""},
	{"role":"user","content":[
		{"type":"text","text":"what is this?"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="}}]}]}`

func TestAChatClientsDefaultTurnReachesAnthropic(t *testing.T) {
	gw := startGateway(t, 20, chatReadyTable)
	key := adminCreateKey(t, gw.AdminAddr, "chat")

	res := chatCompletion(t, gw.PublicAddr, key, chatClientTurn)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, code %s", res.StatusCode, decodeErrorCode(t, res))
	}
	if got := res.Header.Get("X-Modelgate-Dropped-Params"); got != "temperature, frequency_penalty, seed" {
		t.Errorf("X-Modelgate-Dropped-Params = %q", got)
	}

	seen := gw.Anthropic.seen()
	if len(seen) != 1 {
		t.Fatalf("anthropic upstream requests = %d, want 1", len(seen))
	}
	var upstream anthro.MessagesRequest
	if err := json.Unmarshal(seen[0].Body, &upstream); err != nil {
		t.Fatal(err)
	}
	if upstream.MaxTokens != 16_000 || upstream.Temperature != nil {
		t.Errorf("max_tokens %d, temperature %v; want the row's 16000 and no temperature", upstream.MaxTokens, upstream.Temperature)
	}
	if upstream.CacheControl == nil || upstream.CacheControl.TTL != "1h" {
		t.Errorf("request cache_control = %+v, want the one-hour lifetime", upstream.CacheControl)
	}
	if len(upstream.Messages) != 2 {
		t.Fatalf("messages = %d, want the two user turns with the empty assistant turn skipped", len(upstream.Messages))
	}
	last := upstream.Messages[1].Content
	if len(last) != 2 || last[1].Type != "image" || last[1].Source == nil || last[1].Source.MediaType != "image/png" {
		t.Errorf("last turn = %+v, want text then a png image", last)
	}
}
