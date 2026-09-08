package translate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SnowballSH/modelgate/internal/oai"
)

func cmpMaps(got, want map[string]any) string {
	if reflect.DeepEqual(got, want) {
		return ""
	}
	g, _ := json.MarshalIndent(got, "", "  ")
	w, _ := json.MarshalIndent(want, "", "  ")
	return "got:\n" + string(g) + "\nwant:\n" + string(w)
}

func TestToResponsesGolden(t *testing.T) {
	for _, name := range []string{
		"responses_plain",
		"responses_tool_roundtrip",
		"responses_assistant_text_and_tool",
		"responses_parallel_tool_results",
	} {
		t.Run(name, func(t *testing.T) {
			req := loadChatRequest(t, filepath.Join("testdata", name+".input.json"))
			got, err := ToResponses(req, "gpt-real", 1024)
			if err != nil {
				t.Fatalf("ToResponses: %v", err)
			}
			raw, err := os.ReadFile(filepath.Join("testdata", name+".expected.json"))
			if err != nil {
				t.Fatal(err)
			}
			var want map[string]any
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			if diff := cmpMaps(asMap(t, got), want); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestToResponsesEmptyToolOutput(t *testing.T) {
	req := loadChatRequest(t, filepath.Join("testdata", "responses_empty_tool_output.input.json"))
	got, err := ToResponses(req, "gpt-real", 1024)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"output":""`)) {
		t.Fatalf("empty tool result lost its output key: %s", raw)
	}
}

func TestToResponsesNeverEchoesItemIDs(t *testing.T) {
	req := loadChatRequest(t, filepath.Join("testdata", "responses_tool_roundtrip.input.json"))
	got, err := ToResponses(req, "gpt-real", 1024)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range got.Input {
		if item.ID != "" {
			t.Fatalf("input item %q carries an id; a function_call with an id but no paired reasoning item is refused upstream", item.Type)
		}
	}
}

func TestToResponsesRejects(t *testing.T) {
	base := loadChatRequest(t, filepath.Join("testdata", "responses_plain.input.json"))
	n := 2
	half := 0.5
	seed := int64(7)
	for name, mutate := range map[string]func(*oai.ChatRequest){
		"n":                 func(r *oai.ChatRequest) { r.N = &n },
		"logprobs":          func(r *oai.ChatRequest) { v := true; r.Logprobs = &v },
		"effort":            func(r *oai.ChatRequest) { r.ReasoningEffort = "ultra" },
		"stop":              func(r *oai.ChatRequest) { r.Stop = json.RawMessage(`"END"`) },
		"stop_list":         func(r *oai.ChatRequest) { r.Stop = json.RawMessage(`["END","STOP"]`) },
		"stop_not_a_string": func(r *oai.ChatRequest) { r.Stop = json.RawMessage(`42`) },
		"frequency_penalty": func(r *oai.ChatRequest) { r.FrequencyPenalty = &half },
		"presence_penalty":  func(r *oai.ChatRequest) { r.PresencePenalty = &half },
		"seed":              func(r *oai.ChatRequest) { r.Seed = &seed },
		"tool_role_without_id": func(r *oai.ChatRequest) {
			r.Messages = append(r.Messages, oai.Message{Role: "tool", Content: json.RawMessage(`"x"`)})
		},
		"unknown_role": func(r *oai.ChatRequest) {
			r.Messages = append(r.Messages, oai.Message{Role: "function", Content: json.RawMessage(`"x"`)})
		},
		"empty_user_content": func(r *oai.ChatRequest) {
			r.Messages = append(r.Messages, oai.Message{Role: "user", Content: json.RawMessage(`""`)})
		},
		"assistant_without_content_or_tool_calls": func(r *oai.ChatRequest) {
			r.Messages = append(r.Messages, oai.Message{Role: "assistant", Content: json.RawMessage(`null`)})
		},
		"invalid_tool_arguments": func(r *oai.ChatRequest) {
			r.Messages = append(r.Messages, oai.Message{Role: "assistant", ToolCalls: []oai.ToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: oai.FunctionCall{Name: "f", Arguments: "{oops"},
			}}})
		},
		"only_instructions": func(r *oai.ChatRequest) {
			r.Messages = []oai.Message{{Role: "developer", Content: json.RawMessage(`"Be brief."`)}}
		},
		"tool_type": func(r *oai.ChatRequest) {
			r.Tools = []oai.Tool{{Type: "custom"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := base
			r.Messages = append([]oai.Message(nil), base.Messages...)
			mutate(&r)
			if _, err := ToResponses(r, "gpt-real", 1024); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestToResponsesAcceptsMinimalAndDefaultSampling(t *testing.T) {
	req := loadChatRequest(t, filepath.Join("testdata", "responses_plain.input.json"))
	req.ReasoningEffort = "minimal"
	one := 1.0
	req.Temperature, req.TopP = &one, &one
	got, err := ToResponses(req, "gpt-real", 1024)
	if err != nil {
		t.Fatalf("minimal + default sampling must be accepted: %v", err)
	}
	if got.Reasoning == nil || got.Reasoning.Effort != "minimal" {
		t.Fatalf("effort = %+v, want minimal", got.Reasoning)
	}
}

func TestToResponsesNormalisesNonDefaultSampling(t *testing.T) {
	req := loadChatRequest(t, filepath.Join("testdata", "responses_plain.input.json"))
	half := 0.5
	req.Temperature, req.TopP = &half, &half
	got, err := ToResponses(req, "gpt-real", 1024)
	if err != nil {
		t.Fatalf("non-default sampling must be normalised, not refused: %v", err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"temperature", "top_p"} {
		if bytes.Contains(raw, []byte(`"`+key+`"`)) {
			t.Fatalf("%s reached the responses upstream: %s", key, raw)
		}
	}
}

// A stop field that names no sequence is unset, not an unsupported parameter:
// the Anthropic path reads it the same way through the same parseStop.
func TestToResponsesAcceptsEmptyStop(t *testing.T) {
	base := loadChatRequest(t, filepath.Join("testdata", "responses_plain.input.json"))
	for name, raw := range map[string]string{
		"null":                  `null`,
		"empty string":          `""`,
		"empty list":            `[]`,
		"list of empty strings": `["",""]`,
	} {
		t.Run(name, func(t *testing.T) {
			req := base
			req.Stop = json.RawMessage(raw)
			if _, err := ToResponses(req, "gpt-real", 1024); err != nil {
				t.Fatalf("stop %s must be read as unset: %v", raw, err)
			}
		})
	}
}

func TestToResponsesToolChoice(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		want  string
		valid bool
	}{
		{name: "auto", raw: `"auto"`, want: `"auto"`, valid: true},
		{name: "none", raw: `"none"`, want: `"none"`, valid: true},
		{name: "required", raw: `"required"`, want: `"required"`, valid: true},
		{name: "function", raw: `{"type":"function","function":{"name":"f"}}`, want: `{"type":"function","name":"f"}`, valid: true},
		{name: "unknown string", raw: `"any"`},
		{name: "function without a name", raw: `{"type":"function","function":{}}`},
		{name: "unknown object", raw: `{"type":"allowed_tools"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := loadChatRequest(t, filepath.Join("testdata", "responses_plain.input.json"))
			req.ToolChoice = json.RawMessage(tc.raw)
			got, err := ToResponses(req, "gpt-real", 1024)
			if !tc.valid {
				if err == nil {
					t.Fatalf("tool_choice %s was accepted", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got.ToolChoice) != tc.want {
				t.Errorf("tool_choice = %s, want %s", got.ToolChoice, tc.want)
			}
		})
	}
}

func TestToResponsesTextFormat(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		want  string
		valid bool
	}{
		{name: "text", raw: `{"type":"text"}`, want: `{"type":"text"}`, valid: true},
		{name: "json object", raw: `{"type":"json_object"}`, want: `{"type":"json_object"}`, valid: true},
		{
			name:  "json schema",
			raw:   `{"type":"json_schema","json_schema":{"name":"reply","schema":{"type":"object"},"strict":true}}`,
			want:  `{"type":"json_schema","name":"reply","schema":{"type":"object"},"strict":true}`,
			valid: true,
		},
		{name: "json schema without a name", raw: `{"type":"json_schema","json_schema":{"schema":{"type":"object"}}}`},
		{name: "json schema without a schema", raw: `{"type":"json_schema","json_schema":{"name":"reply"}}`},
		{name: "unknown type", raw: `{"type":"grammar"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := loadChatRequest(t, filepath.Join("testdata", "responses_plain.input.json"))
			req.ResponseFormat = json.RawMessage(tc.raw)
			got, err := ToResponses(req, "gpt-real", 1024)
			if !tc.valid {
				if err == nil {
					t.Fatalf("response_format %s was accepted", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Text == nil {
				t.Fatal("text config is missing")
			}
			if string(got.Text.Format) != tc.want {
				t.Errorf("format = %s, want %s", got.Text.Format, tc.want)
			}
		})
	}
}

func TestToResponsesUnknownEffortNamesTheLevels(t *testing.T) {
	req := loadChatRequest(t, filepath.Join("testdata", "responses_plain.input.json"))
	req.ReasoningEffort = "ultra"
	_, err := ToResponses(req, "gpt-real", 1024)
	if err == nil {
		t.Fatal("expected a reasoning_effort error, got nil")
	}
	for _, want := range []string{"reasoning_effort", "ultra", "is not one of"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestToResponsesCarriesStream(t *testing.T) {
	req := loadChatRequest(t, filepath.Join("testdata", "responses_plain.input.json"))
	req.Stream = true
	got, err := ToResponses(req, "gpt-real", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Stream {
		t.Error("stream was not carried through")
	}
}
