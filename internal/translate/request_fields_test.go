package translate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/SnowballSH/modelgate/internal/anthro"
	"github.com/SnowballSH/modelgate/internal/oai"
)

func chatRequest(t *testing.T, body string) oai.ChatRequest {
	t.Helper()
	var req oai.ChatRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	return req
}

const weatherTool = `[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]`

func TestToAnthropicMaxTokens(t *testing.T) {
	cases := []struct {
		name   string
		fields string
		want   int
	}{
		{name: "neither", want: 4096},
		{name: "max_tokens", fields: `"max_tokens":9000,`, want: 9000},
		{name: "max_completion_tokens", fields: `"max_completion_tokens":64,`, want: 64},
		{name: "both prefers max_completion_tokens", fields: `"max_tokens":9000,"max_completion_tokens":64,`, want: 64},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := chatRequest(t, `{`+tc.fields+`"messages":[{"role":"user","content":"hi"}]}`)
			got, err := ToAnthropic(req, "claude-real", 4096)
			if err != nil {
				t.Fatal(err)
			}
			if got.MaxTokens != tc.want {
				t.Errorf("max_tokens = %d, want %d", got.MaxTokens, tc.want)
			}
		})
	}
}

func TestToAnthropicParallelToolCalls(t *testing.T) {
	cases := []struct {
		name   string
		fields string
		want   *anthro.ToolChoice
	}{
		{name: "unset keeps the default", fields: `"tools":` + weatherTool + `,`},
		{name: "true keeps the default", fields: `"tools":` + weatherTool + `,"parallel_tool_calls":true,`},
		{name: "false without tool_choice", fields: `"tools":` + weatherTool + `,"parallel_tool_calls":false,`,
			want: &anthro.ToolChoice{Type: "auto", DisableParallelToolUse: true}},
		{name: "false with auto", fields: `"tools":` + weatherTool + `,"tool_choice":"auto","parallel_tool_calls":false,`,
			want: &anthro.ToolChoice{Type: "auto", DisableParallelToolUse: true}},
		{name: "false with required", fields: `"tools":` + weatherTool + `,"tool_choice":"required","parallel_tool_calls":false,`,
			want: &anthro.ToolChoice{Type: "any", DisableParallelToolUse: true}},
		{name: "false with a named function", fields: `"tools":` + weatherTool + `,"tool_choice":{"type":"function","function":{"name":"get_weather"}},"parallel_tool_calls":false,`,
			want: &anthro.ToolChoice{Type: "tool", Name: "get_weather", DisableParallelToolUse: true}},
		{name: "false with none", fields: `"tools":` + weatherTool + `,"tool_choice":"none","parallel_tool_calls":false,`,
			want: &anthro.ToolChoice{Type: "none"}},
		{name: "false without tools", fields: `"parallel_tool_calls":false,`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := chatRequest(t, `{`+tc.fields+`"messages":[{"role":"user","content":"hi"}]}`)
			got, err := ToAnthropic(req, "claude-real", 1024)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.ToolChoice, tc.want) {
				t.Errorf("tool_choice = %+v, want %+v", got.ToolChoice, tc.want)
			}
		})
	}
}

func TestToAnthropicWireCarriesDisableParallelToolUse(t *testing.T) {
	req := chatRequest(t, `{"tools":`+weatherTool+`,"parallel_tool_calls":false,"messages":[{"role":"user","content":"hi"}]}`)
	got, err := ToAnthropic(req, "claude-real", 1024)
	if err != nil {
		t.Fatal(err)
	}
	choice, _ := asMap(t, got)["tool_choice"].(map[string]any)
	if choice["disable_parallel_tool_use"] != true || choice["type"] != "auto" {
		t.Errorf("tool_choice on the wire = %v", choice)
	}
}

func TestToAnthropicUserBecomesOpaqueMetadata(t *testing.T) {
	longUser := strings.Repeat("u", 600)
	for name, tc := range map[string]struct {
		user string
		want string
	}{
		"short id":       {user: "worker-7", want: "8aca0dd300d2436b96f9d412b7b1b00d8e2454988fc9ada5607c2436a5bfbfa1"},
		"email address":  {user: "someone@example.com", want: digestOf("someone@example.com")},
		"over the limit": {user: longUser, want: digestOf(longUser)},
	} {
		t.Run(name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"user":     tc.user,
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			got, err := ToAnthropic(chatRequest(t, string(body)), "claude-real", 1024)
			if err != nil {
				t.Fatal(err)
			}
			if got.Metadata == nil || got.Metadata.UserID != tc.want {
				t.Errorf("metadata = %+v, want user_id %s", got.Metadata, tc.want)
			}
		})
	}

	got, err := ToAnthropic(chatRequest(t, `{"messages":[{"role":"user","content":"hi"}]}`), "claude-real", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := asMap(t, got)["metadata"]; present {
		t.Error("metadata must be omitted without a user")
	}
}

func digestOf(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func TestToAnthropicRefusesUnsupportedFields(t *testing.T) {
	for field, fragment := range map[string]string{
		"response_format":   `"response_format":{"type":"json_object"}`,
		"n other than 1":    `"n":2`,
		"frequency_penalty": `"frequency_penalty":0.5`,
		"presence_penalty":  `"presence_penalty":0.5`,
		"seed":              `"seed":7`,
		"logprobs":          `"logprobs":true`,
		"top_logprobs":      `"top_logprobs":3`,
	} {
		t.Run(field, func(t *testing.T) {
			req := chatRequest(t, `{`+fragment+`,"messages":[{"role":"user","content":"hi"}]}`)
			_, err := ToAnthropic(req, "claude-real", 1024)
			if err == nil || !strings.Contains(err.Error(), field+" is not supported") {
				t.Fatalf("err = %v, want %q refused", err, field)
			}
		})
	}
}

func TestToAnthropicAcceptsNeutralValues(t *testing.T) {
	req := chatRequest(t, `{"n":1,"logprobs":false,"top_logprobs":0,"parallel_tool_calls":true,"messages":[{"role":"user","content":"hi"}]}`)
	if _, err := ToAnthropic(req, "claude-real", 1024); err != nil {
		t.Fatalf("ToAnthropic: %v", err)
	}
}

func TestToResponsesCarriesUserAndRefusesTopLogprobs(t *testing.T) {
	req := chatRequest(t, `{"user":"worker-7","messages":[{"role":"user","content":"hi"}]}`)
	got, err := ToResponses(req, "gpt-real", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if got.User != "worker-7" {
		t.Errorf("user = %q, want worker-7", got.User)
	}

	req = chatRequest(t, `{"top_logprobs":2,"messages":[{"role":"user","content":"hi"}]}`)
	if _, err := ToResponses(req, "gpt-real", 1024); err == nil || !strings.Contains(err.Error(), "top_logprobs") {
		t.Fatalf("err = %v, want top_logprobs refused", err)
	}
}
