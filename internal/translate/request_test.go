package translate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SnowballSH/modelgate/internal/oai"
)

func loadChatRequest(t *testing.T, path string) oai.ChatRequest {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var req oai.ChatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return req
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestToAnthropicGolden(t *testing.T) {
	cases := []string{
		"plain_chat",
		"sampling_stop_string",
		"sampling_stop_array",
		"stop_null",
		"stop_empty_string",
		"tools_auto",
		"tools_forced",
		"tool_roundtrip",
		"reasoning_low",
		"reasoning_minimal",
		"reasoning_none",
		"reasoning_xhigh",
		"reasoning_drops_sampling",
		"developer_role",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			req := loadChatRequest(t, filepath.Join("testdata", name+".input.json"))
			got, err := ToAnthropic(req, "claude-real", 1024)
			if err != nil {
				t.Fatalf("ToAnthropic: %v", err)
			}
			expectedRaw, err := os.ReadFile(filepath.Join("testdata", name+".expected.json"))
			if err != nil {
				t.Fatal(err)
			}
			var want map[string]any
			if err := json.Unmarshal(expectedRaw, &want); err != nil {
				t.Fatal(err)
			}
			gotMap := asMap(t, got)
			if !reflect.DeepEqual(gotMap, want) {
				gotJSON, _ := json.MarshalIndent(gotMap, "", "  ")
				wantJSON, _ := json.MarshalIndent(want, "", "  ")
				t.Errorf("mismatch\ngot:\n%s\nwant:\n%s", gotJSON, wantJSON)
			}
		})
	}
}

func TestParseStop(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "absent"},
		{name: "null", raw: `null`},
		{name: "empty string", raw: `""`},
		{name: "empty list", raw: `[]`},
		{name: "list of empty strings", raw: `["",""]`},
		{name: "string", raw: `"END"`, want: []string{"END"}},
		{name: "list", raw: `["END","STOP"]`, want: []string{"END", "STOP"}},
		{name: "list with an empty entry", raw: `["","END"]`, want: []string{"END"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseStop(json.RawMessage(tc.raw))
			if err != nil {
				t.Fatalf("parseStop(%s): %v", tc.raw, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseStop(%s) = %#v, want %#v", tc.raw, got, tc.want)
			}
		})
	}
	if _, err := parseStop(json.RawMessage(`42`)); err == nil {
		t.Error("parseStop(42): expected an error")
	}
}

func TestEffortForAnthropic(t *testing.T) {
	cases := []struct {
		name  string
		level string
		want  string
	}{
		{name: "unset", level: "", want: ""},
		{name: "none", level: "none", want: "low"},
		{name: "minimal", level: "minimal", want: "low"},
		{name: "low", level: "low", want: "low"},
		{name: "medium", level: "medium", want: "medium"},
		{name: "high", level: "high", want: "high"},
		{name: "xhigh", level: "xhigh", want: "xhigh"},
		{name: "max", level: "max", want: "max"},
		{name: "mixed case", level: "XHigh", want: "xhigh"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EffortForAnthropic(tc.level)
			if err != nil {
				t.Fatalf("EffortForAnthropic(%q): %v", tc.level, err)
			}
			if got != tc.want {
				t.Errorf("EffortForAnthropic(%q) = %q, want %q", tc.level, got, tc.want)
			}
		})
	}
}

func TestKnownEffort(t *testing.T) {
	cases := []struct {
		name  string
		level string
		want  string
		ok    bool
	}{
		{name: "canonical", level: "medium", want: "medium", ok: true},
		{name: "mixed case", level: "XHigh", want: "xhigh", ok: true},
		{name: "unset", level: ""},
		{name: "unknown", level: "adaptive"},
		{name: "prefixed", level: "high evict=" + strings.Repeat("x", 64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := KnownEffort(tc.level)
			if got != tc.want || ok != tc.ok {
				t.Errorf("KnownEffort(%.24q) = %q, %v; want %q, %v", tc.level, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestToAnthropicRejectsUnknownEffort(t *testing.T) {
	req := loadChatRequest(t, filepath.Join("testdata", "reasoning_low.input.json"))
	req.ReasoningEffort = "adaptive"
	_, err := ToAnthropic(req, "claude-real", 1024)
	if err == nil {
		t.Fatal("expected a reasoning_effort error, got nil")
	}
	for _, want := range []string{"reasoning_effort", "adaptive", "is not one of"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestToAnthropicOmitsEffortWhenUnset(t *testing.T) {
	req := loadChatRequest(t, filepath.Join("testdata", "plain_chat.input.json"))
	got, err := ToAnthropic(req, "claude-real", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := asMap(t, got)["output_config"]; present {
		t.Fatal("output_config must be omitted when reasoning_effort is unset")
	}
}

func TestToAnthropicErrors(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "image part",
			body:    `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://x"}}]}]}`,
			wantErr: "unsupported content part: image_url",
		},
		{
			name:    "bad tool_choice",
			body:    `{"messages":[{"role":"user","content":"hi"}],"tool_choice":"sometimes"}`,
			wantErr: "tool_choice",
		},
		{
			name:    "unknown role",
			body:    `{"messages":[{"role":"critic","content":"hi"}]}`,
			wantErr: "role",
		},
		{
			name:    "invalid tool args",
			body:    `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{oops"}}]}]}`,
			wantErr: "arguments",
		},
		{
			name:    "no messages",
			body:    `{"messages":[{"role":"system","content":"only system"}]}`,
			wantErr: "no messages",
		},
		{
			name:    "bad stop",
			body:    `{"messages":[{"role":"user","content":"hi"}],"stop":42}`,
			wantErr: "stop",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req oai.ChatRequest
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatal(err)
			}
			_, err := ToAnthropic(req, "claude-real", 1024)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}
