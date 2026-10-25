//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/SnowballSH/modelgate/internal/config"
)

var searchTable = fmt.Sprintf(`{"models":{%q:{"provider_model":%q,
	"web_search":{"tool":"web_search_20250305","usd_per_search":0.01,"max_uses":3,"input_tokens_per_search":25000},
	"input_usd_per_mtok":3,"output_usd_per_mtok":15,
	"cache_read_usd_per_mtok":0.3,"cache_write_usd_per_mtok":3.75}}}`, testModel, testModel)

func adminSetWebSearch(t *testing.T, adminAddr, keyID string, enabled bool) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+adminAddr+"/api/keys/"+keyID+"/web-search",
		strings.NewReader(fmt.Sprintf(`{"web_search":%t}`, enabled)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	asAdmin(req)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		Key struct {
			WebSearch bool `json:"web_search"`
		} `json:"key"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil || res.StatusCode != http.StatusOK || out.Key.WebSearch != enabled {
		t.Fatalf("set web search: status %d, %+v, %v", res.StatusCode, out, err)
	}
}

// TestWebSearchSwitchesEndToEnd runs the whole gateway: the environment
// switch, the admin API's per-key switch and the table's block together
// decide whether the Anthropic upstream is offered the server search tool.
func TestWebSearchSwitchesEndToEnd(t *testing.T) {
	gw := startGatewayWith(t, 10, searchTable, func(cfg *config.Config) { cfg.WebSearchEnabled = true })
	key := adminCreateKey(t, gw.AdminAddr, "chat")
	keyID := strings.SplitN(strings.TrimPrefix(key, "mg_"), "_", 2)[0]
	body := fmt.Sprintf(`{"model":%q,"web_search_options":{},"messages":[{"role":"user","content":"news?"}]}`, testModel)

	send := func() (*http.Response, []byte) {
		res := chatCompletion(t, gw.PublicAddr, key, body)
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			payload, _ := io.ReadAll(res.Body)
			t.Fatalf("status %d: %s", res.StatusCode, payload)
		}
		seen := gw.Anthropic.seen()
		return res, seen[len(seen)-1].Body
	}

	res, upstream := send()
	if got := res.Header.Get("X-Modelgate-Dropped-Params"); got != "web_search_options" {
		t.Errorf("a key without web search: dropped %q", got)
	}
	if bytes.Contains(upstream, []byte("web_search")) {
		t.Errorf("a key without web search reached the upstream with search: %s", upstream)
	}

	adminSetWebSearch(t, gw.AdminAddr, keyID, true)
	res, upstream = send()
	if got := res.Header.Get("X-Modelgate-Dropped-Params"); got != "" {
		t.Errorf("a searching request dropped %q", got)
	}
	if !bytes.Contains(upstream, []byte(`{"type":"web_search_20250305","name":"web_search"`)) || bytes.Contains(upstream, []byte("web_search_options")) {
		t.Errorf("upstream request = %s, want the server tool and no raw options", upstream)
	}
}
