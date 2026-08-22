package models

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
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

type Model struct {
	Provider      string
	ProviderModel string
	Pricing       Pricing
}

type Table struct {
	models map[string]Model
}

type modelFile struct {
	Models map[string]modelEntry `json:"models"`
}

type modelEntry struct {
	Provider             string   `json:"provider"`
	ProviderModel        string   `json:"provider_model"`
	InputUSDPerMTok      *float64 `json:"input_usd_per_mtok"`
	OutputUSDPerMTok     *float64 `json:"output_usd_per_mtok"`
	CacheReadUSDPerMTok  *float64 `json:"cache_read_usd_per_mtok"`
	CacheWriteUSDPerMTok *float64 `json:"cache_write_usd_per_mtok"`
}

// LoadTable reads the model pricing table at path. Cost accounting fails
// closed by construction — nothing unpriced can run — so any unreadable
// file, invalid JSON, empty table, empty provider_model, unknown provider,
// or absent, zero, or negative price field yields an error and no table.
// An omitted provider means anthropic.
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
	return Model{
		Provider:      provider,
		ProviderModel: e.ProviderModel,
		Pricing: Pricing{
			InputUSDPerMTok:      *e.InputUSDPerMTok,
			OutputUSDPerMTok:     *e.OutputUSDPerMTok,
			CacheReadUSDPerMTok:  *e.CacheReadUSDPerMTok,
			CacheWriteUSDPerMTok: *e.CacheWriteUSDPerMTok,
		},
	}, nil
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
	providers := make([]string, 0, len(seen))
	for p := range seen {
		providers = append(providers, p)
	}
	slices.Sort(providers)
	return providers
}

func (t *Table) IDs() []string {
	ids := make([]string, 0, len(t.models))
	for id := range t.models {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}
