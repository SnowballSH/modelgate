package oai

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestParseWebSearchOptions(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want *WebSearchOptions
	}{
		{"absent", ``, nil},
		{"null", `null`, nil},
		{"empty object turns search on", `{}`, &WebSearchOptions{}},
		{"context size", `{"search_context_size":"high"}`, &WebSearchOptions{SearchContextSize: "high"}},
		{"null location", `{"user_location":null}`, &WebSearchOptions{}},
		{"full location", `{"search_context_size":"low","user_location":{"type":"approximate","approximate":{"city":"Boston","region":"Massachusetts","country":"US","timezone":"America/New_York"}}}`,
			&WebSearchOptions{SearchContextSize: "low", UserLocation: &UserLocation{Type: "approximate", Approximate: ApproximateLocation{
				City: "Boston", Region: "Massachusetts", Country: "US", Timezone: "America/New_York"}}}},
		{"empty approximate", `{"user_location":{"type":"approximate","approximate":{}}}`,
			&WebSearchOptions{UserLocation: &UserLocation{Type: "approximate"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseWebSearchOptions(json.RawMessage(tc.raw))
			if err != nil {
				t.Fatalf("ParseWebSearchOptions(%s): %v", tc.raw, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseWebSearchOptions(%s) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestParseWebSearchOptionsIsStrict(t *testing.T) {
	tests := []struct {
		name, raw, message string
	}{
		{"a boolean", `true`, "web_search_options must be an object"},
		{"an array", `[]`, "web_search_options must be an object"},
		{"a string", `"on"`, "web_search_options must be an object"},
		{"an unknown key", `{"max_uses":9}`, `unknown field "max_uses"`},
		{"a bad context size", `{"search_context_size":"huge"}`, `search_context_size "huge" is not one of low, medium, high`},
		{"a non-string context size", `{"search_context_size":3}`, "web_search_options"},
		{"a location without a type", `{"user_location":{"approximate":{"city":"Boston"}}}`, `user_location.type must be "approximate"`},
		{"an exact location", `{"user_location":{"type":"exact","approximate":{}}}`, `user_location.type must be "approximate"`},
		{"a location without approximate", `{"user_location":{"type":"approximate"}}`, "user_location.approximate is required"},
		{"an unknown location key", `{"user_location":{"type":"approximate","approximate":{"zip":"02139"}}}`, `unknown field "zip"`},
		{"a three-letter country", `{"user_location":{"type":"approximate","approximate":{"country":"USA"}}}`, "two-letter ISO 3166-1 code"},
		{"a non-letter country", `{"user_location":{"type":"approximate","approximate":{"country":"1A"}}}`, "two-letter ISO 3166-1 code"},
		{"trailing data", `{} {}`, "web_search_options"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseWebSearchOptions(json.RawMessage(tc.raw))
			if err == nil {
				t.Fatalf("ParseWebSearchOptions(%s) = %+v, want an error", tc.raw, got)
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Errorf("error %q does not carry %q", err, tc.message)
			}
		})
	}
}

func TestChatRequestKeepsWebSearchOptionsRaw(t *testing.T) {
	var req ChatRequest
	if err := json.Unmarshal([]byte(`{"model":"m","web_search_options":{"search_context_size":"low"}}`), &req); err != nil {
		t.Fatal(err)
	}
	if string(req.WebSearchOptions) != `{"search_context_size":"low"}` {
		t.Errorf("WebSearchOptions = %s", req.WebSearchOptions)
	}
	out, err := json.Marshal(ChatRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "web_search_options") {
		t.Errorf("a request without the field marshals it: %s", out)
	}
}

func TestAnnotationWireShape(t *testing.T) {
	delta, err := json.Marshal(Delta{Annotations: []Annotation{URLCitationAnnotation("https://example.com/a", "Example")}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"annotations":[{"type":"url_citation","url_citation":{"url":"https://example.com/a","title":"Example"}}]}`; string(delta) != want {
		t.Errorf("delta = %s, want %s", delta, want)
	}
	plain, _ := json.Marshal(Delta{Content: "x"})
	message, _ := json.Marshal(ResponseMessage{Role: "assistant"})
	if strings.Contains(string(plain), "annotations") || strings.Contains(string(message), "annotations") {
		t.Errorf("an answer without citations carries annotations: %s %s", plain, message)
	}
}
