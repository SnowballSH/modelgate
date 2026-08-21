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
		"tools_auto",
		"tools_forced",
		"tool_roundtrip",
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
