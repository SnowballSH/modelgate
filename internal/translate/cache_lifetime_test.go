package translate

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/SnowballSH/modelgate/internal/anthro"
)

func cacheControls(req anthro.MessagesRequest) []*anthro.CacheControl {
	controls := []*anthro.CacheControl{req.CacheControl}
	for _, block := range req.System {
		controls = append(controls, block.CacheControl)
	}
	for _, tool := range req.Tools {
		controls = append(controls, tool.CacheControl)
	}
	var set []*anthro.CacheControl
	for _, control := range controls {
		if control != nil {
			set = append(set, control)
		}
	}
	return set
}

func TestOneHourRowCarriesTheLifetimeOnEveryBreakpoint(t *testing.T) {
	toolsOnly := `{"tools":` + weatherTool + `,"messages":[{"role":"user","content":"hi"}]}`
	for name, tc := range map[string]struct {
		body        string
		breakpoints int
	}{
		"system and conversation": {agentTurn, 2},
		"tools and conversation":  {toolsOnly, 2},
		"conversation alone":      {`{"messages":[{"role":"user","content":"hi"}]}`, 1},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ToAnthropicWith(chatRequest(t, tc.body), AnthropicOptions{ProviderModel: "claude-real", DefaultMaxTokens: 1024, CacheTTL: "1h"})
			if err != nil {
				t.Fatal(err)
			}
			controls := cacheControls(got)
			if len(controls) != tc.breakpoints {
				t.Fatalf("breakpoints = %d, want %d", len(controls), tc.breakpoints)
			}
			for _, control := range controls {
				raw, err := json.Marshal(control)
				if err != nil {
					t.Fatal(err)
				}
				if string(raw) != `{"type":"ephemeral","ttl":"1h"}` {
					t.Errorf("breakpoint = %s, want the one-hour lifetime", raw)
				}
			}
		})
	}
}

func TestFiveMinuteRowCarriesNoLifetime(t *testing.T) {
	got, err := ToAnthropicWith(chatRequest(t, agentTurn), AnthropicOptions{ProviderModel: "claude-real", DefaultMaxTokens: 1024})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"ttl"`)) {
		t.Errorf("a five-minute row sent a ttl: %s", raw)
	}
	if n := len(cacheControls(got)); n != 2 {
		t.Errorf("breakpoints = %d, want 2", n)
	}
}

func TestCachingOffSendsNoLifetime(t *testing.T) {
	got, err := ToAnthropicWith(chatRequest(t, agentTurn), AnthropicOptions{ProviderModel: "claude-real", DefaultMaxTokens: 1024, CacheTTL: "1h", DisablePromptCaching: true})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(cacheControls(got)); n != 0 {
		t.Errorf("breakpoints = %d, want none with caching off", n)
	}
}

func TestNamespacedCacheKey(t *testing.T) {
	first := NamespacedCacheKey("key-a", "thread-1")
	if len(first) != 32 || first == "thread-1" {
		t.Fatalf("namespaced key = %q, want 32 hex characters that are not the client's value", first)
	}
	if again := NamespacedCacheKey("key-a", "thread-1"); again != first {
		t.Errorf("the same key and value gave %q then %q", first, again)
	}
	if other := NamespacedCacheKey("key-b", "thread-1"); other == first {
		t.Error("two gateway keys share a cache bucket")
	}
	if other := NamespacedCacheKey("key-a", "thread-2"); other == first {
		t.Error("two client values share a cache bucket")
	}
	if shifted := NamespacedCacheKey("key-", "athread-1"); shifted == first {
		t.Error("the key id and the client value are not delimited")
	}
	if got := NamespacedCacheKey("key-a", ""); got != "" {
		t.Errorf("no client value must send no key, got %q", got)
	}
}

func TestToResponsesForwardsTheNamespacedCacheKey(t *testing.T) {
	req := chatRequest(t, `{"messages":[{"role":"user","content":"hi"}]}`)
	key := NamespacedCacheKey("key-a", "thread-1")
	got, err := ToResponsesWith(req, ResponsesOptions{ProviderModel: "gpt-real", DefaultMaxTokens: 1024, DropSampling: true, PromptCacheKey: key})
	if err != nil {
		t.Fatal(err)
	}
	if wire := asMap(t, got)["prompt_cache_key"]; wire != key {
		t.Errorf("prompt_cache_key on the wire = %v, want %q", wire, key)
	}

	plain, err := ToResponses(req, "gpt-real", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := asMap(t, plain)["prompt_cache_key"]; present {
		t.Error("prompt_cache_key must be omitted when the client sent none")
	}
}
