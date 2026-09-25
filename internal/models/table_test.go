package models

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeTable(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadTableValid(t *testing.T) {
	table, err := LoadTable(filepath.Join("testdata", "models.json"))
	if err != nil {
		t.Fatalf("LoadTable: %v", err)
	}

	tests := []struct {
		publicID string
		want     Model
	}{
		{"claude-opus-5", Model{
			Provider:      ProviderAnthropic,
			ProviderModel: "claude-opus-5",
			Pricing: Pricing{
				InputUSDPerMTok:      5.0,
				OutputUSDPerMTok:     25.0,
				CacheReadUSDPerMTok:  0.50,
				CacheWriteUSDPerMTok: 6.25,
			},
		}},
		{"claude-sonnet-5", Model{
			Provider:      ProviderAnthropic,
			ProviderModel: "claude-sonnet-5",
			Pricing: Pricing{
				InputUSDPerMTok:      3.0,
				OutputUSDPerMTok:     15.0,
				CacheReadUSDPerMTok:  0.30,
				CacheWriteUSDPerMTok: 3.75,
			},
		}},
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

	if _, ok := table.Resolve("gpt-5"); ok {
		t.Error("Resolve(unknown): ok=true, want false")
	}
}

func TestIDsSorted(t *testing.T) {
	table, err := LoadTable(filepath.Join("testdata", "models.json"))
	if err != nil {
		t.Fatalf("LoadTable: %v", err)
	}
	got := table.IDs()
	want := []string{"claude-opus-5", "claude-sonnet-5"}
	if len(got) != len(want) {
		t.Fatalf("IDs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("IDs() = %v, want %v", got, want)
		}
	}
}

func TestLoadTableErrors(t *testing.T) {
	tests := []struct {
		name     string
		contents string
	}{
		{"missing price field", `{"models": {"m": {"provider_model": "m",
			"input_usd_per_mtok": 1.0, "output_usd_per_mtok": 2.0,
			"cache_read_usd_per_mtok": 0.1}}}`},
		{"explicit zero price", `{"models": {"m": {"provider_model": "m",
			"input_usd_per_mtok": 0.0, "output_usd_per_mtok": 2.0,
			"cache_read_usd_per_mtok": 0.1, "cache_write_usd_per_mtok": 1.25}}}`},
		{"negative price", `{"models": {"m": {"provider_model": "m",
			"input_usd_per_mtok": 1.0, "output_usd_per_mtok": -2.0,
			"cache_read_usd_per_mtok": 0.1, "cache_write_usd_per_mtok": 1.25}}}`},
		{"empty provider_model", `{"models": {"m": {"provider_model": "",
			"input_usd_per_mtok": 1.0, "output_usd_per_mtok": 2.0,
			"cache_read_usd_per_mtok": 0.1, "cache_write_usd_per_mtok": 1.25}}}`},
		{"empty models map", `{"models": {}}`},
		{"invalid JSON", `{"models": `},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			table, err := LoadTable(writeTable(t, tc.contents))
			if err == nil {
				t.Fatal("LoadTable: err=nil, want error")
			}
			if table != nil {
				t.Fatalf("LoadTable returned partial table %+v with error", table)
			}
		})
	}
}

func TestLoadTableNonexistentPath(t *testing.T) {
	table, err := LoadTable(filepath.Join(t.TempDir(), "absent.json"))
	if err == nil {
		t.Fatal("LoadTable: err=nil, want error")
	}
	if table != nil {
		t.Fatal("LoadTable returned table with error")
	}
}

func TestLoadTableProviders(t *testing.T) {
	path := writeTable(t, `{"models":{
		"claude-sonnet-5":{"provider_model":"claude-sonnet-5","input_usd_per_mtok":3,"output_usd_per_mtok":15,"cache_read_usd_per_mtok":0.3,"cache_write_usd_per_mtok":3.75},
		"gpt-5":{"provider":"openai","provider_model":"gpt-5","input_usd_per_mtok":1.25,"output_usd_per_mtok":10,"cache_read_usd_per_mtok":0.125,"cache_write_usd_per_mtok":1.25}}}`)
	table, err := LoadTable(path)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := table.Resolve("gpt-5")
	if !ok || m.Provider != ProviderOpenAI {
		t.Fatalf("gpt-5 = %+v, ok=%v; want openai provider", m, ok)
	}
	got := table.Providers()
	want := []string{ProviderAnthropic, ProviderOpenAI}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Providers() = %v, want %v", got, want)
	}
}

func TestLoadTableRejectsUnknownProvider(t *testing.T) {
	path := writeTable(t, `{"models":{"m":{"provider":"azure","provider_model":"m","input_usd_per_mtok":1,"output_usd_per_mtok":1,"cache_read_usd_per_mtok":1,"cache_write_usd_per_mtok":1}}}`)
	if _, err := LoadTable(path); err == nil {
		t.Fatal("unknown provider accepted")
	}
}

func TestTableEntries(t *testing.T) {
	path := writeTable(t, `{"models":{
		"gpt-5":{"provider":"openai","provider_model":"gpt-5","input_usd_per_mtok":1.25,"output_usd_per_mtok":10,"cache_read_usd_per_mtok":0.125,"cache_write_usd_per_mtok":1.25},
		"claude-sonnet-5":{"provider_model":"claude-sonnet-5","input_usd_per_mtok":3,"output_usd_per_mtok":15,"cache_read_usd_per_mtok":0.3,"cache_write_usd_per_mtok":3.75}}}`)
	table, err := LoadTable(path)
	if err != nil {
		t.Fatal(err)
	}
	got := table.Entries()
	want := []Entry{
		{ID: "claude-sonnet-5", Provider: ProviderAnthropic},
		{ID: "gpt-5", Provider: ProviderOpenAI},
	}
	if len(got) != len(want) {
		t.Fatalf("Entries() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Entries()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestLoadTableUpstreamAPI(t *testing.T) {
	path := writeTable(t, `{"models":{
		"gpt-x":{"provider":"openai","upstream_api":"responses","provider_model":"gpt-x","input_usd_per_mtok":1,"output_usd_per_mtok":1,"cache_read_usd_per_mtok":1,"cache_write_usd_per_mtok":1},
		"gpt-y":{"provider":"openai","provider_model":"gpt-y","input_usd_per_mtok":1,"output_usd_per_mtok":1,"cache_read_usd_per_mtok":1,"cache_write_usd_per_mtok":1}}}`)
	table, err := LoadTable(path)
	if err != nil {
		t.Fatal(err)
	}
	x, _ := table.Resolve("gpt-x")
	y, _ := table.Resolve("gpt-y")
	if x.UpstreamAPI != UpstreamResponses || y.UpstreamAPI != UpstreamChatCompletions {
		t.Fatalf("upstream_api: x=%q y=%q", x.UpstreamAPI, y.UpstreamAPI)
	}
}

func TestLoadTableRejectsUpstreamAPIOutsideOpenAI(t *testing.T) {
	path := writeTable(t, `{"models":{"claude-x":{"upstream_api":"responses","provider_model":"claude-x","input_usd_per_mtok":1,"output_usd_per_mtok":1,"cache_read_usd_per_mtok":1,"cache_write_usd_per_mtok":1}}}`)
	if _, err := LoadTable(path); err == nil {
		t.Fatal("expected an error: upstream_api is an openai-only field")
	}
}

func TestLoadTableRejectsUnknownUpstreamAPI(t *testing.T) {
	path := writeTable(t, `{"models":{"gpt-x":{"provider":"openai","upstream_api":"grpc","provider_model":"gpt-x","input_usd_per_mtok":1,"output_usd_per_mtok":1,"cache_read_usd_per_mtok":1,"cache_write_usd_per_mtok":1}}}`)
	if _, err := LoadTable(path); err == nil {
		t.Fatal("expected an error for an unknown upstream_api")
	}
}

const pricedFields = `"input_usd_per_mtok":1,"output_usd_per_mtok":1,"cache_read_usd_per_mtok":1,"cache_write_usd_per_mtok":1`

func TestLoadTableLimits(t *testing.T) {
	path := writeTable(t, `{"models":{
		"claude-opus-5-5":{"provider_model":"claude-opus-5-5","forced_tool_choice":false,`+pricedFields+`},
		"gpt-6-luna":{"provider":"openai","upstream_api":"responses","provider_model":"gpt-6-luna","reasoning_efforts":["none","low","medium","high","xhigh","max"],`+pricedFields+`},
		"claude-sonnet-5":{"provider_model":"claude-sonnet-5","forced_tool_choice":true,`+pricedFields+`},
		"gpt-5.6-terra":{"provider":"openai","provider_model":"gpt-5.6-terra","reasoning_efforts":null,`+pricedFields+`}}}`)
	table, err := LoadTable(path)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		publicID string
		want     Limits
	}{
		{"claude-opus-5-5", Limits{NoForcedToolChoice: true}},
		{"gpt-6-luna", Limits{ReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"}}},
		{"claude-sonnet-5", Limits{}},
		{"gpt-5.6-terra", Limits{}},
	}
	for _, tc := range tests {
		got, ok := table.Resolve(tc.publicID)
		if !ok {
			t.Fatalf("Resolve(%q): ok=false", tc.publicID)
		}
		if !reflect.DeepEqual(got.Limits, tc.want) {
			t.Errorf("Resolve(%q).Limits = %+v, want %+v", tc.publicID, got.Limits, tc.want)
		}
	}
}

func TestLoadTableRejectsUnusableLimits(t *testing.T) {
	tests := []struct {
		name   string
		fields string
	}{
		{"empty reasoning_efforts", `"reasoning_efforts":[]`},
		{"unknown effort", `"reasoning_efforts":["low","extreme"]`},
		{"effort not in canonical case", `"reasoning_efforts":["Low"]`},
		{"forced_tool_choice not a boolean", `"forced_tool_choice":"no"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTable(t, `{"models":{"m":{"provider_model":"m",`+tc.fields+`,`+pricedFields+`}}}`)
			if table, err := LoadTable(path); err == nil || table != nil {
				t.Fatalf("LoadTable = %v, %v; want no table and an error", table, err)
			}
		})
	}
}

func TestLoadTableIgnoresUnknownFields(t *testing.T) {
	path := writeTable(t, `{"schema":9,"models":{"m":{"provider_model":"m","a_future_limit":{"x":1},`+pricedFields+`}}}`)
	table, err := LoadTable(path)
	if err != nil {
		t.Fatalf("a table carrying fields this build does not know must still load: %v", err)
	}
	if m, ok := table.Resolve("m"); !ok || !reflect.DeepEqual(m.Limits, Limits{}) {
		t.Fatalf("Resolve(m) = %+v, %v; want a model with no limits", m, ok)
	}
}

func TestLimitsAllowEffort(t *testing.T) {
	unrestricted := Limits{}
	listed := Limits{ReasoningEfforts: []string{"low", "high"}}
	tests := []struct {
		limits Limits
		level  string
		want   bool
	}{
		{unrestricted, "minimal", true},
		{unrestricted, "", true},
		{listed, "high", true},
		{listed, "minimal", false},
		{listed, "", false},
	}
	for _, tc := range tests {
		if got := tc.limits.AllowsEffort(tc.level); got != tc.want {
			t.Errorf("%+v.AllowsEffort(%q) = %v, want %v", tc.limits, tc.level, got, tc.want)
		}
	}
}
