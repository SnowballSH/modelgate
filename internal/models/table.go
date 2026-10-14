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

const (
	CacheTTLFiveMinutes = "5m"
	CacheTTLOneHour     = "1h"
)

const MaxDefaultMaxTokens = 128_000

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
	Provider         string
	UpstreamAPI      string
	ProviderModel    string
	Pricing          Pricing
	PriceTiers       []PriceTier
	Limits           Limits
	DefaultMaxTokens int
	DropsSampling    bool
	ImageInput       bool
	CacheTTL         string
}

func (m Model) OutputCapDefault(gatewayDefault int) int {
	if m.DefaultMaxTokens > 0 {
		return m.DefaultMaxTokens
	}
	return gatewayDefault
}

func (m Model) Translated() bool {
	return m.Provider == ProviderAnthropic || m.UpstreamAPI == UpstreamResponses
}

type Table struct {
	models map[string]Model
}

type modelFile struct {
	Models map[string]modelEntry `json:"models"`
}

type priceEntry struct {
	InputUSDPerMTok      *float64 `json:"input_usd_per_mtok"`
	OutputUSDPerMTok     *float64 `json:"output_usd_per_mtok"`
	CacheReadUSDPerMTok  *float64 `json:"cache_read_usd_per_mtok"`
	CacheWriteUSDPerMTok *float64 `json:"cache_write_usd_per_mtok"`
}

type tierEntry struct {
	AboveInputTokens *int64 `json:"above_input_tokens"`
	priceEntry
}

type modelEntry struct {
	Provider      string `json:"provider"`
	UpstreamAPI   string `json:"upstream_api"`
	ProviderModel string `json:"provider_model"`
	priceEntry
	ForcedToolChoice *bool       `json:"forced_tool_choice"`
	ReasoningEfforts []string    `json:"reasoning_efforts"`
	DefaultMaxTokens *int        `json:"default_max_tokens"`
	Sampling         *bool       `json:"sampling"`
	ImageInput       *bool       `json:"image_input"`
	PriceTiers       []tierEntry `json:"price_tiers"`
	CacheTTL         *string     `json:"cache_ttl"`
}

// LoadTable reads the model pricing table at path. Cost accounting fails
// closed by construction — nothing unpriced can run — so any unreadable
// file, invalid JSON, empty table, empty provider_model, unknown provider,
// unusable upstream_api, absent, zero, or negative price field, or empty or
// unknown reasoning_efforts, or an out-of-range default_max_tokens, unusable
// price_tiers or cache_ttl yields an error and no table. An omitted provider
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
	pricing, err := e.pricing()
	if err != nil {
		return Model{}, err
	}
	limits, err := e.limits()
	if err != nil {
		return Model{}, err
	}
	tiers, err := e.tiers()
	if err != nil {
		return Model{}, err
	}
	defaultMaxTokens, err := e.defaultMaxTokens()
	if err != nil {
		return Model{}, err
	}
	cacheTTL, err := e.cacheTTL(provider)
	if err != nil {
		return Model{}, err
	}
	return Model{
		Provider:         provider,
		UpstreamAPI:      upstream,
		ProviderModel:    e.ProviderModel,
		Pricing:          pricing,
		PriceTiers:       tiers,
		Limits:           limits,
		DefaultMaxTokens: defaultMaxTokens,
		DropsSampling:    dropsSampling(e.Sampling, upstream),
		ImageInput:       e.ImageInput != nil && *e.ImageInput,
		CacheTTL:         cacheTTL,
	}, nil
}

func (e priceEntry) pricing() (Pricing, error) {
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
			return Pricing{}, fmt.Errorf("%s absent", p.field)
		}
		if *p.value <= 0 {
			return Pricing{}, fmt.Errorf("%s must be positive, got %v", p.field, *p.value)
		}
	}
	return Pricing{
		InputUSDPerMTok:      *e.InputUSDPerMTok,
		OutputUSDPerMTok:     *e.OutputUSDPerMTok,
		CacheReadUSDPerMTok:  *e.CacheReadUSDPerMTok,
		CacheWriteUSDPerMTok: *e.CacheWriteUSDPerMTok,
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

func (e modelEntry) tiers() ([]PriceTier, error) {
	if e.PriceTiers == nil {
		return nil, nil
	}
	if len(e.PriceTiers) == 0 {
		return nil, errors.New("price_tiers is empty; omit it for one price")
	}
	tiers := make([]PriceTier, len(e.PriceTiers))
	var previous int64
	for i, entry := range e.PriceTiers {
		switch {
		case entry.AboveInputTokens == nil:
			return nil, fmt.Errorf("price_tiers[%d]: above_input_tokens absent", i)
		case *entry.AboveInputTokens <= 0:
			return nil, fmt.Errorf("price_tiers[%d]: above_input_tokens must be positive, got %d", i, *entry.AboveInputTokens)
		case *entry.AboveInputTokens <= previous:
			return nil, fmt.Errorf("price_tiers[%d]: above_input_tokens %d must exceed the previous tier's %d", i, *entry.AboveInputTokens, previous)
		}
		pricing, err := entry.pricing()
		if err != nil {
			return nil, fmt.Errorf("price_tiers[%d]: %w", i, err)
		}
		previous = *entry.AboveInputTokens
		tiers[i] = PriceTier{AboveInputTokens: previous, Pricing: pricing}
	}
	return tiers, nil
}

func (e modelEntry) defaultMaxTokens() (int, error) {
	if e.DefaultMaxTokens == nil {
		return 0, nil
	}
	if *e.DefaultMaxTokens < 1 || *e.DefaultMaxTokens > MaxDefaultMaxTokens {
		return 0, fmt.Errorf("default_max_tokens must be between 1 and %d, got %d", MaxDefaultMaxTokens, *e.DefaultMaxTokens)
	}
	return *e.DefaultMaxTokens, nil
}

func (e modelEntry) cacheTTL(provider string) (string, error) {
	if e.CacheTTL == nil {
		return "", nil
	}
	if provider != ProviderAnthropic {
		return "", errors.New("cache_ttl is only valid for anthropic models")
	}
	switch *e.CacheTTL {
	case CacheTTLFiveMinutes:
		return "", nil
	case CacheTTLOneHour:
		return CacheTTLOneHour, nil
	}
	return "", fmt.Errorf("cache_ttl %q is not one of %s, %s", *e.CacheTTL, CacheTTLFiveMinutes, CacheTTLOneHour)
}

func dropsSampling(sampling *bool, upstream string) bool {
	if sampling != nil {
		return !*sampling
	}
	return upstream == UpstreamResponses
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
