package models

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const tierFields = `"price_tiers":[{"above_input_tokens":100000,"input_usd_per_mtok":5,"output_usd_per_mtok":25,"cache_read_usd_per_mtok":0.5,"cache_write_usd_per_mtok":10}]`

var (
	unitPricing = Pricing{InputUSDPerMTok: 1, OutputUSDPerMTok: 1, CacheReadUSDPerMTok: 1, CacheWriteUSDPerMTok: 1}
	tierPricing = Pricing{InputUSDPerMTok: 5, OutputUSDPerMTok: 25, CacheReadUSDPerMTok: 0.5, CacheWriteUSDPerMTok: 10}
)

func TestLoadTableChatFields(t *testing.T) {
	path := writeTable(t, `{"models":{
		"claude-chat":{"provider_model":"claude-chat","default_max_tokens":32000,"sampling":false,"image_input":true,"cache_ttl":"1h",`+tierFields+`,`+pricedFields+`},
		"claude-short":{"provider_model":"claude-short","cache_ttl":"5m","sampling":true,"image_input":false,"price_tiers":null,`+pricedFields+`},
		"gpt-resp":{"provider":"openai","upstream_api":"responses","provider_model":"gpt-resp",`+pricedFields+`},
		"gpt-resp-sampling":{"provider":"openai","upstream_api":"responses","provider_model":"gpt-resp-sampling","sampling":true,`+pricedFields+`},
		"gpt-pass":{"provider":"openai","provider_model":"gpt-pass",`+pricedFields+`}}}`)
	table, err := LoadTable(path)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		publicID string
		want     Model
	}{
		{"claude-chat", Model{
			Provider: ProviderAnthropic, ProviderModel: "claude-chat", Pricing: unitPricing,
			PriceTiers:       []PriceTier{{AboveInputTokens: 100_000, Pricing: tierPricing}},
			DefaultMaxTokens: 32_000, DropsSampling: true, ImageInput: true, CacheTTL: CacheTTLOneHour,
		}},
		{"claude-short", Model{Provider: ProviderAnthropic, ProviderModel: "claude-short", Pricing: unitPricing}},
		{"gpt-resp", Model{Provider: ProviderOpenAI, UpstreamAPI: UpstreamResponses, ProviderModel: "gpt-resp", Pricing: unitPricing, DropsSampling: true}},
		{"gpt-resp-sampling", Model{Provider: ProviderOpenAI, UpstreamAPI: UpstreamResponses, ProviderModel: "gpt-resp-sampling", Pricing: unitPricing}},
		{"gpt-pass", Model{Provider: ProviderOpenAI, UpstreamAPI: UpstreamChatCompletions, ProviderModel: "gpt-pass", Pricing: unitPricing}},
	}
	for _, tc := range tests {
		got, ok := table.Resolve(tc.publicID)
		if !ok {
			t.Fatalf("Resolve(%q): ok=false", tc.publicID)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Resolve(%q) = %+v, want %+v", tc.publicID, got, tc.want)
		}
	}
}

func TestLoadTableRejectsUnusableChatFields(t *testing.T) {
	tests := []struct {
		name    string
		fields  string
		message string
	}{
		{"default_max_tokens zero", `"default_max_tokens":0`, "default_max_tokens must be between 1 and 128000, got 0"},
		{"default_max_tokens above the range", `"default_max_tokens":128001`, "default_max_tokens must be between 1 and 128000, got 128001"},
		{"default_max_tokens not an integer", `"default_max_tokens":"many"`, "default_max_tokens"},
		{"sampling not a boolean", `"sampling":"no"`, "sampling"},
		{"image_input not a boolean", `"image_input":1`, "image_input"},
		{"unknown cache_ttl", `"cache_ttl":"24h"`, `cache_ttl "24h" is not one of 5m, 1h`},
		{"cache_ttl on an openai row", `"provider":"openai","cache_ttl":"1h"`, "cache_ttl is only valid for anthropic models"},
		{"the default cache_ttl on an openai row", `"provider":"openai","cache_ttl":"5m"`, "cache_ttl is only valid for anthropic models"},
		{"empty price_tiers", `"price_tiers":[]`, "price_tiers is empty; omit it for one price"},
		{"tier without a threshold", `"price_tiers":[{` + pricedFields + `}]`, "price_tiers[0]: above_input_tokens absent"},
		{"zero threshold", `"price_tiers":[{"above_input_tokens":0,` + pricedFields + `}]`, "price_tiers[0]: above_input_tokens must be positive, got 0"},
		{"thresholds not ascending", `"price_tiers":[{"above_input_tokens":200000,` + pricedFields + `},{"above_input_tokens":200000,` + pricedFields + `}]`,
			"price_tiers[1]: above_input_tokens 200000 must exceed the previous tier's 200000"},
		{"tier missing a price", `"price_tiers":[{"above_input_tokens":1,"input_usd_per_mtok":1,"output_usd_per_mtok":1,"cache_read_usd_per_mtok":1}]`,
			"price_tiers[0]: cache_write_usd_per_mtok absent"},
		{"tier with a zero price", `"price_tiers":[{"above_input_tokens":1,"input_usd_per_mtok":1,"output_usd_per_mtok":0,"cache_read_usd_per_mtok":1,"cache_write_usd_per_mtok":1}]`,
			"price_tiers[0]: output_usd_per_mtok must be positive, got 0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTable(t, `{"models":{"m":{"provider_model":"m",`+tc.fields+`,`+pricedFields+`}}}`)
			table, err := LoadTable(path)
			if err == nil || table != nil {
				t.Fatalf("LoadTable = %v, %v; want no table and an error", table, err)
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Errorf("error %q does not carry %q", err, tc.message)
			}
		})
	}
}

func TestTableV5LoadsWithEveryNewFieldAtItsV060Meaning(t *testing.T) {
	table, err := LoadTable(filepath.Join("testdata", "models_v5.json"))
	if err != nil {
		t.Fatalf("the production v5 table must load unchanged: %v", err)
	}
	wantIDs := []string{"claude-haiku-4-5", "claude-opus-5-5", "claude-sonnet-5", "gpt-5.6-terra", "gpt-6-luna", "gpt-6-sol"}
	if got := table.IDs(); !reflect.DeepEqual(got, wantIDs) {
		t.Fatalf("IDs() = %v, want %v", got, wantIDs)
	}
	for _, id := range wantIDs {
		m, _ := table.Resolve(id)
		if m.PriceTiers != nil || m.DefaultMaxTokens != 0 || m.ImageInput || m.CacheTTL != "" {
			t.Errorf("%s gained a v0.7.0 capability it never declared: %+v", id, m)
		}
		if m.DropsSampling != (m.UpstreamAPI == UpstreamResponses) {
			t.Errorf("%s: DropsSampling = %v; v0.6.0 dropped sampling on responses rows only", id, m.DropsSampling)
		}
		if got := m.OutputCapDefault(4096); got != 4096 {
			t.Errorf("%s: output default = %d, want the gateway's 4096", id, got)
		}
		if got := m.PricingFor(900_000); got != m.Pricing {
			t.Errorf("%s: PricingFor(900000) = %+v, want the one price %+v", id, got, m.Pricing)
		}
		if got := m.WorstCasePricing(); got != m.Pricing {
			t.Errorf("%s: WorstCasePricing() = %+v, want %+v", id, got, m.Pricing)
		}
	}
	opus, _ := table.Resolve("claude-opus-5-5")
	luna, _ := table.Resolve("gpt-6-luna")
	if !opus.Limits.NoForcedToolChoice || !reflect.DeepEqual(luna.Limits.ReasoningEfforts, []string{"none", "low", "medium", "high", "xhigh", "max"}) {
		t.Errorf("v0.6.0 limits changed: opus %+v, luna %+v", opus.Limits, luna.Limits)
	}
}

func TestTableV6Loads(t *testing.T) {
	table, err := LoadTable(filepath.Join("testdata", "models_v6.json"))
	if err != nil {
		t.Fatalf("the spec's v6 rows must load: %v", err)
	}
	wantIDs := []string{"claude-fable-5-1", "claude-haiku-5-5", "claude-opus-5-5", "claude-sonnet-5-5", "gpt-6-astra", "gpt-6-luna", "gpt-6.1-sol"}
	if got := table.IDs(); !reflect.DeepEqual(got, wantIDs) {
		t.Fatalf("IDs() = %v, want %v", got, wantIDs)
	}
	tests := []struct {
		id               string
		defaultMaxTokens int
		dropsSampling    bool
		cacheTTL         string
		tierAbove        int64
		tierInput        float64
	}{
		{"claude-fable-5-1", 32_000, true, CacheTTLOneHour, 0, 0},
		{"claude-opus-5-5", 32_000, true, CacheTTLOneHour, 0, 0},
		{"claude-sonnet-5-5", 16_000, true, CacheTTLOneHour, 0, 0},
		{"claude-haiku-5-5", 16_000, true, CacheTTLOneHour, 100_000, 0.5},
		{"gpt-6-astra", 32_000, true, "", 272_000, 20},
		{"gpt-6.1-sol", 16_000, true, "", 272_000, 4},
		{"gpt-6-luna", 16_000, true, "", 272_000, 0.2},
	}
	for _, tc := range tests {
		m, _ := table.Resolve(tc.id)
		if m.DefaultMaxTokens != tc.defaultMaxTokens || m.DropsSampling != tc.dropsSampling || m.CacheTTL != tc.cacheTTL || !m.ImageInput {
			t.Errorf("%s = %+v", tc.id, m)
		}
		if tc.tierAbove == 0 {
			if m.PriceTiers != nil {
				t.Errorf("%s: unexpected tiers %+v", tc.id, m.PriceTiers)
			}
			continue
		}
		if len(m.PriceTiers) != 1 || m.PriceTiers[0].AboveInputTokens != tc.tierAbove || m.PriceTiers[0].Pricing.InputUSDPerMTok != tc.tierInput {
			t.Errorf("%s: tiers = %+v", tc.id, m.PriceTiers)
		}
	}
}

func TestTierIsChosenByTheWholePrompt(t *testing.T) {
	second := Pricing{InputUSDPerMTok: 9, OutputUSDPerMTok: 9, CacheReadUSDPerMTok: 9, CacheWriteUSDPerMTok: 9}
	m := Model{Pricing: unitPricing, PriceTiers: []PriceTier{
		{AboveInputTokens: 100_000, Pricing: tierPricing},
		{AboveInputTokens: 500_000, Pricing: second},
	}}
	tests := []struct {
		promptTokens int64
		want         Pricing
	}{
		{0, unitPricing},
		{100_000, unitPricing},
		{100_001, tierPricing},
		{500_000, tierPricing},
		{500_001, second},
	}
	for _, tc := range tests {
		if got := m.PricingFor(tc.promptTokens); got != tc.want {
			t.Errorf("PricingFor(%d) = %+v, want %+v", tc.promptTokens, got, tc.want)
		}
	}
}

func TestWorstCasePricingBoundsEveryTier(t *testing.T) {
	cheaperOutput := Pricing{InputUSDPerMTok: 5, OutputUSDPerMTok: 0.5, CacheReadUSDPerMTok: 0.5, CacheWriteUSDPerMTok: 10}
	m := Model{Pricing: unitPricing, PriceTiers: []PriceTier{{AboveInputTokens: 100_000, Pricing: cheaperOutput}}}
	want := Pricing{InputUSDPerMTok: 5, OutputUSDPerMTok: 1, CacheReadUSDPerMTok: 1, CacheWriteUSDPerMTok: 10}
	if got := m.WorstCasePricing(); got != want {
		t.Errorf("WorstCasePricing() = %+v, want %+v", got, want)
	}
}

func TestOutputCapDefaultAndTranslated(t *testing.T) {
	if got := (Model{DefaultMaxTokens: 16_000}).OutputCapDefault(4096); got != 16_000 {
		t.Errorf("row default = %d, want 16000", got)
	}
	for _, tc := range []struct {
		model Model
		want  bool
	}{
		{Model{Provider: ProviderAnthropic}, true},
		{Model{Provider: ProviderOpenAI, UpstreamAPI: UpstreamResponses}, true},
		{Model{Provider: ProviderOpenAI, UpstreamAPI: UpstreamChatCompletions}, false},
	} {
		if got := tc.model.Translated(); got != tc.want {
			t.Errorf("%+v.Translated() = %v, want %v", tc.model, got, tc.want)
		}
	}
}
