package translate

import (
	"testing"

	"github.com/SnowballSH/modelgate/internal/anthro"
)

const agentTurn = `{"tools":` + weatherTool + `,"messages":[
	{"role":"system","content":"You are an agent."},
	{"role":"user","content":"weather?"},
	{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
	{"role":"tool","tool_call_id":"c1","content":"18C"}]}`

func breakpoints(req anthro.MessagesRequest) int {
	n := 0
	if req.CacheControl != nil {
		n++
	}
	for _, block := range req.System {
		if block.CacheControl != nil {
			n++
		}
	}
	for _, tool := range req.Tools {
		if tool.CacheControl != nil {
			n++
		}
	}
	return n
}

func TestPromptCachingMarksSystemAndConversation(t *testing.T) {
	got, err := ToAnthropic(chatRequest(t, agentTurn), "claude-real", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if got.CacheControl == nil || got.CacheControl.Type != "ephemeral" {
		t.Errorf("request cache_control = %+v, want the automatic ephemeral breakpoint", got.CacheControl)
	}
	if len(got.System) != 1 || got.System[0].CacheControl == nil || got.System[0].CacheControl.Type != "ephemeral" {
		t.Errorf("system = %+v, want one block carrying the breakpoint", got.System)
	}
	if got.Tools[0].CacheControl != nil {
		t.Error("the system breakpoint already covers the tools; a second marker spends a slot")
	}
	if n := breakpoints(got); n != 2 {
		t.Errorf("breakpoints = %d, want 2 of the 4 allowed", n)
	}
	wire := asMap(t, got)
	if control, _ := wire["cache_control"].(map[string]any); control["type"] != "ephemeral" {
		t.Errorf("wire cache_control = %v", wire["cache_control"])
	}
}

func TestPromptCachingMarksLastToolWithoutSystem(t *testing.T) {
	req := chatRequest(t, `{"tools":[
		{"type":"function","function":{"name":"a"}},
		{"type":"function","function":{"name":"b"}}],
		"messages":[{"role":"user","content":"hi"}]}`)
	got, err := ToAnthropic(req, "claude-real", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tools[0].CacheControl != nil || got.Tools[1].CacheControl == nil {
		t.Errorf("tools = %+v, want the breakpoint on the last tool only", got.Tools)
	}
	if got.CacheControl == nil {
		t.Error("request cache_control missing")
	}
}

func TestPromptCachingJoinsSystemMessagesIntoOneBlock(t *testing.T) {
	req := chatRequest(t, `{"messages":[
		{"role":"system","content":"First."},
		{"role":"developer","content":"Second."},
		{"role":"user","content":"hi"}]}`)
	got, err := ToAnthropic(req, "claude-real", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.System) != 1 || got.System[0].Text != "First.\n\nSecond." || got.System[0].Type != "text" {
		t.Errorf("system = %+v", got.System)
	}
}

func TestPromptCachingWithoutSystemOrTools(t *testing.T) {
	got, err := ToAnthropic(chatRequest(t, `{"messages":[{"role":"user","content":"hi"}]}`), "claude-real", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if got.System != nil {
		t.Errorf("system = %+v, want omitted", got.System)
	}
	if n := breakpoints(got); n != 1 || got.CacheControl == nil {
		t.Errorf("breakpoints = %d, want only the automatic one", n)
	}
}

func TestPromptCachingOffSwitch(t *testing.T) {
	got, err := ToAnthropicWith(chatRequest(t, agentTurn), AnthropicOptions{
		ProviderModel:        "claude-real",
		DefaultMaxTokens:     1024,
		DisablePromptCaching: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := breakpoints(got); n != 0 {
		t.Errorf("breakpoints = %d, want none with caching off", n)
	}
	wire := asMap(t, got)
	if _, present := wire["cache_control"]; present {
		t.Error("cache_control must be omitted with caching off")
	}
	if got.Model != "claude-real" || got.MaxTokens != 1024 {
		t.Errorf("options not applied: model %q max_tokens %d", got.Model, got.MaxTokens)
	}
}

func TestCacheUsageFlowsThroughUnchanged(t *testing.T) {
	usage := anthro.Usage{InputTokens: 40, OutputTokens: 7, CacheCreationInputTokens: 300, CacheReadInputTokens: 1200}
	stored := ToStoreUsage(usage)
	if stored.CacheWriteTokens != 300 || stored.CacheReadTokens != 1200 || stored.InputTokens != 40 {
		t.Errorf("store usage = %+v", stored)
	}
	st := NewStreamTranslator("m", 1, "id")
	drive(t, st, []anthro.StreamEvent{
		{Type: "message_start", Message: &anthro.MessagesResponse{Usage: anthro.Usage{InputTokens: 40, CacheCreationInputTokens: 300, CacheReadInputTokens: 1200}}},
		{Type: "message_delta", Delta: &anthro.StreamDelta{StopReason: "end_turn"}, Usage: &anthro.Usage{OutputTokens: 7}},
		{Type: "message_stop"},
	})
	if got := st.Usage(); got != usage {
		t.Errorf("streamed usage = %+v, want %+v", got, usage)
	}
}
