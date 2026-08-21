package models

import (
	"os"
	"path/filepath"
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
			ProviderModel: "claude-opus-5",
			Pricing: Pricing{
				InputUSDPerMTok:      5.0,
				OutputUSDPerMTok:     25.0,
				CacheReadUSDPerMTok:  0.50,
				CacheWriteUSDPerMTok: 6.25,
			},
		}},
		{"claude-sonnet-5", Model{
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
		if got != tc.want {
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
