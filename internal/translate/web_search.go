package translate

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/SnowballSH/modelgate/internal/anthro"
	"github.com/SnowballSH/modelgate/internal/oai"
)

// WebSearchTool is a request's provider-native web search as the model table
// and the client settle it: the row's tool and per-request search limit, and
// the client's options.
type WebSearchTool struct {
	Tool    string
	MaxUses int
	Options oai.WebSearchOptions
}

const droppedSearchContextSize = "web_search_options.search_context_size"

// anthropicTool renders the server tool. Anthropic has no search context
// size, so the translator names that option as dropped.
func (w WebSearchTool) anthropicTool() anthro.Tool {
	tool := anthro.Tool{Type: w.Tool, Name: "web_search", MaxUses: w.MaxUses}
	if loc := w.Options.UserLocation; loc != nil && !loc.Approximate.IsZero() {
		tool.UserLocation = &anthro.UserLocation{
			Type:     "approximate",
			City:     loc.Approximate.City,
			Region:   loc.Approximate.Region,
			Country:  loc.Approximate.Country,
			Timezone: loc.Approximate.Timezone,
		}
	}
	return tool
}

// source is one web page an answer cites.
type source struct {
	url, title string
}

func webSource(rawURL, title string) (source, bool) {
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return source{}, false
	}
	if strings.TrimSpace(title) == "" {
		title = rawURL
	}
	return source{url: rawURL, title: title}, true
}

func anthropicSources(citations []anthro.Citation) []source {
	var sources []source
	for _, c := range citations {
		if s, ok := webSource(c.URL, c.Title); ok {
			sources = append(sources, s)
		}
	}
	return sources
}

// citationIndex numbers an answer's web sources by first use, so a marker
// [n] in the text names the n-th url_citation annotation the client received.
type citationIndex struct {
	numbers map[string]int
	sources []source
}

// cite numbers a cited span's sources and returns the markers to append
// after the span, one per distinct source in citation order, and the
// annotations for the sources this span cites first.
func (c *citationIndex) cite(sources []source) (string, []oai.Annotation) {
	if c.numbers == nil {
		c.numbers = map[string]int{}
	}
	var markers strings.Builder
	var added []oai.Annotation
	marked := map[int]bool{}
	for _, s := range sources {
		n, known := c.numbers[s.url]
		if !known {
			c.sources = append(c.sources, s)
			n = len(c.sources)
			c.numbers[s.url] = n
			added = append(added, oai.URLCitationAnnotation(s.url, s.title))
		}
		if marked[n] {
			continue
		}
		marked[n] = true
		if markers.Len() == 0 {
			markers.WriteByte(' ')
		}
		fmt.Fprintf(&markers, "[%d]", n)
	}
	return markers.String(), added
}

func (c *citationIndex) annotations() []oai.Annotation {
	out := make([]oai.Annotation, len(c.sources))
	for i, s := range c.sources {
		out[i] = oai.URLCitationAnnotation(s.url, s.title)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
