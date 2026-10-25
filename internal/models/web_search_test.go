package models

import (
	"path/filepath"
	"strings"
	"testing"
)

const anthropicSearch = `"web_search":{"tool":"web_search_20250305","usd_per_search":0.01,"max_uses":3,"input_tokens_per_search":25000}`
const openaiSearch = `"web_search":{"tool":"web_search","usd_per_search":0.01,"max_uses":3,"input_tokens_per_search":25000}`

func TestLoadTableWebSearch(t *testing.T) {
	path := writeTable(t, `{"models":{
		"claude":{"provider_model":"claude",`+anthropicSearch+`,`+tierFields+`,`+pricedFields+`},
		"gpt":{"provider":"openai","upstream_api":"responses","provider_model":"gpt",`+openaiSearch+`,`+pricedFields+`},
		"plain":{"provider_model":"plain",`+pricedFields+`},
		"null":{"provider_model":"null","web_search":null,`+pricedFields+`}}}`)
	table, err := LoadTable(path)
	if err != nil {
		t.Fatal(err)
	}
	claude, _ := table.Resolve("claude")
	want := WebSearch{Tool: "web_search_20250305", USDPerSearch: 0.01, MaxUses: 3, InputTokensPerSearch: 25_000}
	if claude.WebSearch == nil || *claude.WebSearch != want {
		t.Fatalf("claude.WebSearch = %+v, want %+v", claude.WebSearch, want)
	}
	if claude.Pricing.USDPerSearch != 0.01 || claude.PriceTiers[0].Pricing.USDPerSearch != 0.01 || claude.WorstCasePricing().USDPerSearch != 0.01 {
		t.Errorf("the search fee must ride every tier: base %+v, tier %+v", claude.Pricing, claude.PriceTiers[0].Pricing)
	}
	gpt, _ := table.Resolve("gpt")
	if gpt.WebSearch == nil || gpt.WebSearch.Tool != "web_search" {
		t.Errorf("gpt.WebSearch = %+v", gpt.WebSearch)
	}
	for _, id := range []string{"plain", "null"} {
		if m, _ := table.Resolve(id); m.WebSearch != nil || m.Pricing.USDPerSearch != 0 {
			t.Errorf("%s: WebSearch = %+v, fee %v; want none", id, m.WebSearch, m.Pricing.USDPerSearch)
		}
	}
}

func TestLoadTableRejectsUnusableWebSearch(t *testing.T) {
	block := func(fields string) string { return `"web_search":{` + fields + `}` }
	const rest = `"usd_per_search":0.01,"max_uses":3,"input_tokens_per_search":25000`
	tests := []struct {
		name    string
		row     string
		message string
	}{
		{"an openai tool on an anthropic row", `"provider_model":"m",` + block(`"tool":"web_search",`+rest),
			`web_search.tool "web_search" is not web_search_20250305`},
		{"a newer anthropic tool", `"provider_model":"m",` + block(`"tool":"web_search_20260209",`+rest),
			`web_search.tool "web_search_20260209" is not web_search_20250305`},
		{"an anthropic tool on a responses row", `"provider":"openai","upstream_api":"responses","provider_model":"m",` + block(`"tool":"web_search_20250305",`+rest),
			`web_search.tool "web_search_20250305" is not web_search`},
		{"a passthrough row", `"provider":"openai","provider_model":"m",` + block(`"tool":"web_search",`+rest),
			"web_search is only valid for anthropic models and openai responses models"},
		{"no tool", `"provider_model":"m",` + block(rest), "web_search.tool absent"},
		{"no fee", `"provider_model":"m",` + block(`"tool":"web_search_20250305","max_uses":3,"input_tokens_per_search":25000`), "web_search.usd_per_search absent"},
		{"a zero fee", `"provider_model":"m",` + block(`"tool":"web_search_20250305","usd_per_search":0,"max_uses":3,"input_tokens_per_search":25000`), "web_search.usd_per_search must be positive"},
		{"no max_uses", `"provider_model":"m",` + block(`"tool":"web_search_20250305","usd_per_search":0.01,"input_tokens_per_search":25000`), "web_search.max_uses absent"},
		{"zero max_uses", `"provider_model":"m",` + block(`"tool":"web_search_20250305","usd_per_search":0.01,"max_uses":0,"input_tokens_per_search":25000`), "web_search.max_uses must be between 1 and 10, got 0"},
		{"eleven max_uses", `"provider_model":"m",` + block(`"tool":"web_search_20250305","usd_per_search":0.01,"max_uses":11,"input_tokens_per_search":25000`), "web_search.max_uses must be between 1 and 10, got 11"},
		{"no allowance", `"provider_model":"m",` + block(`"tool":"web_search_20250305","usd_per_search":0.01,"max_uses":3`), "web_search.input_tokens_per_search absent"},
		{"a zero allowance", `"provider_model":"m",` + block(`"tool":"web_search_20250305","usd_per_search":0.01,"max_uses":3,"input_tokens_per_search":0`), "web_search.input_tokens_per_search must be between 1 and 10000000, got 0"},
		{"an absurd allowance", `"provider_model":"m",` + block(`"tool":"web_search_20250305","usd_per_search":0.01,"max_uses":3,"input_tokens_per_search":10000001`), "web_search.input_tokens_per_search must be between 1 and 10000000, got 10000001"},
		{"not an object", `"provider_model":"m","web_search":true`, "web_search"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			table, err := LoadTable(writeTable(t, `{"models":{"m":{`+tc.row+`,`+pricedFields+`}}}`))
			if err == nil || table != nil {
				t.Fatalf("LoadTable = %v, %v; want no table and an error", table, err)
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Errorf("error %q does not carry %q", err, tc.message)
			}
		})
	}
}

func TestTableV7LoadsWithoutWebSearch(t *testing.T) {
	table, err := LoadTable(filepath.Join("testdata", "models_v7.json"))
	if err != nil {
		t.Fatalf("the production v7 table must load unchanged: %v", err)
	}
	for _, id := range table.IDs() {
		m, _ := table.Resolve(id)
		if m.WebSearch != nil || m.Pricing.USDPerSearch != 0 || m.WorstCasePricing().USDPerSearch != 0 {
			t.Errorf("%s: a v7 row carries no search, got %+v", id, m.WebSearch)
		}
	}
}
