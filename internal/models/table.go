package models

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
)

type Pricing struct {
	InputUSDPerMTok      float64
	OutputUSDPerMTok     float64
	CacheReadUSDPerMTok  float64
	CacheWriteUSDPerMTok float64
}

const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
)

const (
	UpstreamChatCompletions = "chat_completions"
	UpstreamResponses       = "responses"
)

// EffortLevels is the reasoning_effort vocabulary modelgate accepts from
// clients, in ascending order.
var EffortLevels = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// Limits are the request features a model's provider refuses, declared in the
// table so modelgate can refuse them itself before any upstream call. The zero
// value refuses nothing, which is what a table without the fields means.
type Limits struct {
	NoForcedToolChoice bool
	// ReasoningEfforts lists the levels the model accepts; nil accepts every
	// level in EffortLevels.
	ReasoningEfforts []string
}

func (l Limits) AllowsEffort(level string) bool {
	return l.ReasoningEfforts == nil || slices.Contains(l.ReasoningEfforts, level)
}

type Model struct {
	Provider      string
	UpstreamAPI   string
	ProviderModel string
	Pricing       Pricing
	Limits        Limits
}

type Table struct {
	models map[string]Model
}

type modelFile struct {
	Models map[string]modelEntry `json:"models"`
}

type modelEntry struct {
	Provider             string   `json:"provider"`
	UpstreamAPI          string   `json:"upstream_api"`
	ProviderModel        string   `json:"provider_model"`
	InputUSDPerMTok      *float64 `json:"input_usd_per_mtok"`
	OutputUSDPerMTok     *float64 `json:"output_usd_per_mtok"`
	CacheReadUSDPerMTok  *float64 `json:"cache_read_usd_per_mtok"`
	CacheWriteUSDPerMTok *float64 `json:"cache_write_usd_per_mtok"`
	ForcedToolChoice     *bool    `json:"forced_tool_choice"`
	ReasoningEfforts     []string `json:"reasoning_efforts"`
}

// LoadTable reads the model pricing table at path. Cost accounting fails
// closed by construction — nothing unpriced can run — so any unreadable
// file, invalid JSON, empty table, empty provider_model, unknown provider,
// unusable upstream_api, absent, zero, or negative price field, or empty or
// unknown reasoning_efforts yields an error and no table. An omitted provider
// means anthropic, an omitted upstream_api means chat_completions on openai
// models and nothing on anthropic ones, and omitted limits refuse nothing.
// Fields this build does not know are ignored, so a newer table still loads.
func LoadTable(path string) (*Table, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("model table: %w", err)
	}
	var file modelFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("model table %s: %w", path, err)
	}
	if len(file.Models) == 0 {
		return nil, fmt.Errorf("model table %s: no models", path)
	}
	table := &Table{models: make(map[string]Model, len(file.Models))}
	for id, entry := range file.Models {
		model, err := entry.validate()
		if err != nil {
			return nil, fmt.Errorf("model table %s: model %q: %w", path, id, err)
		}
		table.models[id] = model
	}
	return table, nil
}

func (e modelEntry) validate() (Model, error) {
	if e.ProviderModel == "" {
		return Model{}, fmt.Errorf("empty provider_model")
	}
	provider := e.Provider
	if provider == "" {
		provider = ProviderAnthropic
	}
	if provider != ProviderAnthropic && provider != ProviderOpenAI {
		return Model{}, fmt.Errorf("unknown provider %q", provider)
	}
	upstream := e.UpstreamAPI
	switch {
	case upstream == "" && provider == ProviderOpenAI:
		upstream = UpstreamChatCompletions
	case upstream == "":
	case provider != ProviderOpenAI:
		return Model{}, fmt.Errorf("upstream_api is only valid for openai models")
	case upstream != UpstreamChatCompletions && upstream != UpstreamResponses:
		return Model{}, fmt.Errorf("unknown upstream_api %q", upstream)
	}
	prices := []struct {
		field string
		value *float64
	}{
		{"input_usd_per_mtok", e.InputUSDPerMTok},
		{"output_usd_per_mtok", e.OutputUSDPerMTok},
		{"cache_read_usd_per_mtok", e.CacheReadUSDPerMTok},
		{"cache_write_usd_per_mtok", e.CacheWriteUSDPerMTok},
	}
	for _, p := range prices {
		if p.value == nil {
			return Model{}, fmt.Errorf("%s absent", p.field)
		}
		if *p.value <= 0 {
			return Model{}, fmt.Errorf("%s must be positive, got %v", p.field, *p.value)
		}
	}
	limits, err := e.limits()
	if err != nil {
		return Model{}, err
	}
	return Model{
		Provider:      provider,
		UpstreamAPI:   upstream,
		ProviderModel: e.ProviderModel,
		Limits:        limits,
		Pricing: Pricing{
			InputUSDPerMTok:      *e.InputUSDPerMTok,
			OutputUSDPerMTok:     *e.OutputUSDPerMTok,
			CacheReadUSDPerMTok:  *e.CacheReadUSDPerMTok,
			CacheWriteUSDPerMTok: *e.CacheWriteUSDPerMTok,
		},
	}, nil
}

func (e modelEntry) limits() (Limits, error) {
	limits := Limits{NoForcedToolChoice: e.ForcedToolChoice != nil && !*e.ForcedToolChoice}
	if e.ReasoningEfforts == nil {
		return limits, nil
	}
	if len(e.ReasoningEfforts) == 0 {
		return Limits{}, errors.New("reasoning_efforts is empty; omit it to accept every level")
	}
	for _, level := range e.ReasoningEfforts {
		if !slices.Contains(EffortLevels, level) {
			return Limits{}, fmt.Errorf("reasoning_efforts: %q is not one of %s", level, strings.Join(EffortLevels, ", "))
		}
	}
	limits.ReasoningEfforts = slices.Clone(e.ReasoningEfforts)
	return limits, nil
}

func (t *Table) Resolve(publicID string) (Model, bool) {
	model, ok := t.models[publicID]
	return model, ok
}

// Providers reports which providers the table references, so startup can
// require exactly the credentials the configuration will use.
func (t *Table) Providers() []string {
	seen := map[string]bool{}
	for _, m := range t.models {
		seen[m.Provider] = true
	}
	return slices.Sorted(maps.Keys(seen))
}

func (t *Table) IDs() []string {
	return slices.Sorted(maps.Keys(t.models))
}

type Entry struct {
	ID       string
	Provider string
}

func (t *Table) Entries() []Entry {
	entries := make([]Entry, 0, len(t.models))
	for _, id := range t.IDs() {
		entries = append(entries, Entry{ID: id, Provider: t.models[id].Provider})
	}
	return entries
}
