package oai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// WebSearchContextSizes is the search_context_size vocabulary, in ascending
// order.
var WebSearchContextSizes = []string{"low", "medium", "high"}

// WebSearchOptions is the Chat Completions web_search_options object. Its
// presence, even empty, asks for provider-native web search.
type WebSearchOptions struct {
	SearchContextSize string        `json:"search_context_size,omitempty"`
	UserLocation      *UserLocation `json:"user_location,omitempty"`
}

type UserLocation struct {
	Type        string              `json:"type"`
	Approximate ApproximateLocation `json:"approximate"`
}

type ApproximateLocation struct {
	City     string `json:"city,omitempty"`
	Region   string `json:"region,omitempty"`
	Country  string `json:"country,omitempty"`
	Timezone string `json:"timezone,omitempty"`
}

func (l ApproximateLocation) IsZero() bool {
	return l == ApproximateLocation{}
}

// ParseWebSearchOptions reads a request's web_search_options strictly: nil
// when the field is absent or null, otherwise an object with only the keys
// OpenAI documents, each within its vocabulary.
func ParseWebSearchOptions(raw json.RawMessage) (*WebSearchOptions, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	if trimmed[0] != '{' {
		return nil, errors.New("web_search_options must be an object")
	}
	var wire struct {
		SearchContextSize *string `json:"search_context_size"`
		UserLocation      *struct {
			Type        string               `json:"type"`
			Approximate *ApproximateLocation `json:"approximate"`
		} `json:"user_location"`
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return nil, fmt.Errorf("invalid web_search_options: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid web_search_options: trailing data")
	}
	opts := &WebSearchOptions{}
	if size := wire.SearchContextSize; size != nil {
		if !slices.Contains(WebSearchContextSizes, *size) {
			return nil, fmt.Errorf("web_search_options.search_context_size %q is not one of %s", *size, strings.Join(WebSearchContextSizes, ", "))
		}
		opts.SearchContextSize = *size
	}
	if loc := wire.UserLocation; loc != nil {
		switch {
		case loc.Type != "approximate":
			return nil, errors.New(`web_search_options.user_location.type must be "approximate"`)
		case loc.Approximate == nil:
			return nil, errors.New("web_search_options.user_location.approximate is required")
		case loc.Approximate.Country != "" && !isAlpha2(loc.Approximate.Country):
			return nil, fmt.Errorf("web_search_options.user_location.approximate.country %q is not a two-letter ISO 3166-1 code", loc.Approximate.Country)
		}
		opts.UserLocation = &UserLocation{Type: loc.Type, Approximate: *loc.Approximate}
	}
	return opts, nil
}

func isAlpha2(code string) bool {
	return len(code) == 2 && strings.IndexFunc(code, func(r rune) bool {
		return (r < 'A' || r > 'Z') && (r < 'a' || r > 'z')
	}) < 0
}

// Annotation is a Chat Completions url_citation annotation: one cited web
// source.
type Annotation struct {
	Type        string       `json:"type"`
	URLCitation *URLCitation `json:"url_citation,omitempty"`
}

type URLCitation struct {
	URL   string `json:"url"`
	Title string `json:"title"`
}

func URLCitationAnnotation(url, title string) Annotation {
	return Annotation{Type: "url_citation", URLCitation: &URLCitation{URL: url, Title: title}}
}
