package models

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadTableMaxInputTokens(t *testing.T) {
	path := writeTable(t, `{"models":{
		"capped":{"provider_model":"capped","max_input_tokens":1000000,`+pricedFields+`},
		"capped-tiered":{"provider_model":"capped-tiered","max_input_tokens":100001,`+tierFields+`,`+pricedFields+`},
		"one-token":{"provider_model":"one-token","max_input_tokens":1,`+pricedFields+`},
		"null":{"provider_model":"null","max_input_tokens":null,`+pricedFields+`},
		"absent":{"provider_model":"absent",`+pricedFields+`}}}`)
	table, err := LoadTable(path)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]int64{"capped": 1_000_000, "capped-tiered": 100_001, "one-token": 1, "null": 0, "absent": 0} {
		m, ok := table.Resolve(id)
		if !ok {
			t.Fatalf("Resolve(%q): ok=false", id)
		}
		if m.MaxInputTokens != want {
			t.Errorf("%s: MaxInputTokens = %d, want %d", id, m.MaxInputTokens, want)
		}
	}
}

func TestLoadTableRejectsUnusableMaxInputTokens(t *testing.T) {
	twoTiers := `"price_tiers":[{"above_input_tokens":100000,` + pricedFields + `},{"above_input_tokens":200000,` + pricedFields + `}]`
	tests := []struct {
		name    string
		fields  string
		message string
	}{
		{"zero", `"max_input_tokens":0`, "max_input_tokens must be positive, got 0"},
		{"negative", `"max_input_tokens":-1`, "max_input_tokens must be positive, got -1"},
		{"a fraction", `"max_input_tokens":1000000.5`, "max_input_tokens"},
		{"a string", `"max_input_tokens":"1000000"`, "max_input_tokens"},
		{"a tier threshold equal to the cap", `"max_input_tokens":100000,` + tierFields,
			"price_tiers[0]: above_input_tokens 100000 can never apply: no prompt exceeds max_input_tokens 100000"},
		{"a tier threshold above the cap", `"max_input_tokens":99999,` + tierFields,
			"price_tiers[0]: above_input_tokens 100000 can never apply: no prompt exceeds max_input_tokens 99999"},
		{"only the second tier out of reach", `"max_input_tokens":150000,` + twoTiers,
			"price_tiers[1]: above_input_tokens 200000 can never apply: no prompt exceeds max_input_tokens 150000"},
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

func TestShippedTablesDeclareNoMaxInputTokens(t *testing.T) {
	for _, fixture := range []string{"models.json", "models_v5.json", "models_v6.json"} {
		table, err := LoadTable(filepath.Join("testdata", fixture))
		if err != nil {
			t.Fatalf("%s: %v", fixture, err)
		}
		for _, id := range table.IDs() {
			if m, _ := table.Resolve(id); m.MaxInputTokens != 0 {
				t.Errorf("%s: %s gained a prompt cap of %d it never declared", fixture, id, m.MaxInputTokens)
			}
		}
	}
}
