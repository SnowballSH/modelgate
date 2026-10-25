package store

import (
	"context"
	"maps"
	"testing"
	"time"
)

// writeV080Database builds the database v0.8.0 left behind by running its
// three migrations, as it did, with one key and two usage rows.
func writeV080Database(t *testing.T, dir string) {
	t.Helper()
	legacy := rawDB(t, dir)
	defer legacy.Close()
	if _, err := legacy.Exec("CREATE TEMP TABLE upgrade_attribution (model TEXT PRIMARY KEY, provider TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	for _, step := range migrations[:3] {
		if _, err := legacy.Exec(step); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := legacy.Exec("PRAGMA user_version = 3"); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	if _, err := legacy.Exec(
		`INSERT INTO keys (id, prefix, secret_sha256, label, models, quota_usd, created_at, created_by)
		 VALUES ('keyA', 'mg_keyA', x'0102', 'kami', '["gpt-6-luna"]', 5, ?, 'alice')`, created); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		model, provider string
		cost            float64
	}{{"gpt-6-luna", "openai", 1.25}, {"claude-opus-5-5", "anthropic", 2}} {
		if _, err := legacy.Exec(
			`INSERT INTO usage (month, key_id, model, provider, requests, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd)
			 VALUES ('2026-10', 'keyA', ?, ?, 4, 400, 40, 4, 1, ?)`, row.model, row.provider, row.cost); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenUpgradesAV080DatabaseLosslessly(t *testing.T) {
	dir := t.TempDir()
	writeV080Database(t, dir)

	s, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open on a v0.8.0 database: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	key, found, err := s.KeyByID(ctx, "keyA")
	if err != nil || !found || key.Label != "kami" || key.WebSearch {
		t.Fatalf("key after the upgrade = %+v found=%v err=%v; want the v0.8.0 key with web search off", key, found, err)
	}
	usage, err := s.MonthUsage(ctx, "2026-10")
	if err != nil {
		t.Fatal(err)
	}
	want := Usage{Requests: 8, InputTokens: 800, OutputTokens: 80, CacheReadTokens: 8, CacheWriteTokens: 2, CostUSD: 3.25}
	if got := usage["keyA"]; got != want {
		t.Errorf("usage after the upgrade = %+v, want %+v", got, want)
	}
	perProvider, err := s.MonthSpendPerProvider(ctx, "2026-10")
	if err != nil || !maps.Equal(perProvider, map[string]float64{"openai": 1.25, "anthropic": 2}) {
		t.Errorf("per-provider spend after the upgrade = %v, %v", perProvider, err)
	}

	check := rawDB(t, dir)
	defer check.Close()
	if got := userVersion(t, check); got != SchemaVersion || SchemaVersion != 4 {
		t.Fatalf("user_version = %d, SchemaVersion %d; want 4", got, SchemaVersion)
	}
}

func TestKeyWebSearchRoundTrip(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	for _, k := range []KeyRecord{
		{ID: "on", Prefix: "mg_on", SecretSHA256: []byte{1}, Label: "chat", WebSearch: true, CreatedAt: now, CreatedBy: "alice"},
		{ID: "off", Prefix: "mg_off", SecretSHA256: []byte{2}, Label: "agent", CreatedAt: now, CreatedBy: "alice"},
	} {
		if err := s.InsertKey(ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[string]bool{"on": true, "off": false} {
		if k, _, err := s.KeyByID(ctx, id); err != nil || k.WebSearch != want {
			t.Errorf("KeyByID(%s).WebSearch = %v, %v; want %v", id, k.WebSearch, err, want)
		}
	}

	if err := s.SetKeyWebSearch(ctx, "off", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetKeyWebSearch(ctx, "on", false); err != nil {
		t.Fatal(err)
	}
	listed, err := s.ListKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, k := range listed {
		got[k.ID] = k.WebSearch
	}
	if !maps.Equal(got, map[string]bool{"on": false, "off": true}) {
		t.Errorf("after toggling, web search = %v", got)
	}
	if err := s.SetKeyWebSearch(ctx, "missing", true); err == nil {
		t.Error("SetKeyWebSearch on an unknown key succeeded")
	}
}

func TestAddUsageAccumulatesWebSearchRequests(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	for _, n := range []int64{2, 3} {
		if err := s.AddUsage(ctx, "2026-10", "k", "m", "anthropic", Usage{Requests: 1, InputTokens: 10, WebSearchRequests: n, CostUSD: 0.1}); err != nil {
			t.Fatal(err)
		}
	}
	usage, err := s.MonthUsage(ctx, "2026-10")
	if err != nil {
		t.Fatal(err)
	}
	if got := usage["k"]; got.WebSearchRequests != 5 || got.Requests != 2 {
		t.Errorf("usage = %+v, want 5 web search requests over 2 requests", got)
	}
}

func TestUsageBillable(t *testing.T) {
	cases := []struct {
		u    Usage
		want bool
	}{
		{Usage{}, false},
		{Usage{InputTokens: 1}, true},
		{Usage{WebSearchRequests: 1}, true},
	}
	for _, tc := range cases {
		if got := tc.u.Billable(); got != tc.want {
			t.Errorf("%+v.Billable() = %v, want %v", tc.u, got, tc.want)
		}
	}
}
