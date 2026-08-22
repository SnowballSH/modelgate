package store

import (
	"context"
	"math"
	"testing"
	"time"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

func ptr[T any](v T) *T { return &v }

func TestPing(t *testing.T) {
	s, _ := openTestStore(t)
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestInsertKeyRoundTripAllFieldsSet(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	k := KeyRecord{
		ID:           "key1",
		Prefix:       "mg_abc",
		SecretSHA256: []byte{0x01, 0x02, 0x03},
		Label:        "test key",
		Models:       []string{"claude-sonnet", "gpt-5"},
		QuotaUSD:     ptr(12.5),
		ExpiresAt:    ptr(now.Add(24 * time.Hour)),
		RevokedAt:    ptr(now.Add(time.Hour)),
		LastUsedAt:   ptr(now.Add(time.Minute)),
		CreatedAt:    now,
		CreatedBy:    "admin",
	}
	if err := s.InsertKey(ctx, k); err != nil {
		t.Fatalf("InsertKey: %v", err)
	}
	got, ok, err := s.KeyByID(ctx, "key1")
	if err != nil || !ok {
		t.Fatalf("KeyByID: ok=%v err=%v", ok, err)
	}
	if got.ID != k.ID || got.Prefix != k.Prefix || got.Label != k.Label || got.CreatedBy != k.CreatedBy {
		t.Errorf("string fields mismatch: %+v", got)
	}
	if string(got.SecretSHA256) != string(k.SecretSHA256) {
		t.Errorf("SecretSHA256 = %v, want %v", got.SecretSHA256, k.SecretSHA256)
	}
	if len(got.Models) != 2 || got.Models[0] != "claude-sonnet" || got.Models[1] != "gpt-5" {
		t.Errorf("Models = %v", got.Models)
	}
	if got.QuotaUSD == nil || *got.QuotaUSD != 12.5 {
		t.Errorf("QuotaUSD = %v", got.QuotaUSD)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(*k.ExpiresAt) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, k.ExpiresAt)
	}
	if got.RevokedAt == nil || !got.RevokedAt.Equal(*k.RevokedAt) {
		t.Errorf("RevokedAt = %v, want %v", got.RevokedAt, k.RevokedAt)
	}
	if got.LastUsedAt == nil || !got.LastUsedAt.Equal(*k.LastUsedAt) {
		t.Errorf("LastUsedAt = %v, want %v", got.LastUsedAt, k.LastUsedAt)
	}
	if !got.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, now)
	}
}

func TestInsertKeyRoundTripNilOptionals(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	k := KeyRecord{
		ID:           "key-nil",
		Prefix:       "mg_nil",
		SecretSHA256: []byte{0xff},
		Label:        "nils",
		CreatedAt:    time.Now().UTC(),
		CreatedBy:    "admin",
	}
	if err := s.InsertKey(ctx, k); err != nil {
		t.Fatalf("InsertKey: %v", err)
	}
	got, ok, err := s.KeyByID(ctx, "key-nil")
	if err != nil || !ok {
		t.Fatalf("KeyByID: ok=%v err=%v", ok, err)
	}
	if got.Models != nil {
		t.Errorf("Models = %v, want nil", got.Models)
	}
	if got.QuotaUSD != nil || got.ExpiresAt != nil || got.RevokedAt != nil || got.LastUsedAt != nil {
		t.Errorf("optional fields not nil: %+v", got)
	}
}

func TestInsertKeyEmptyModelsRoundTripsEmpty(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	k := KeyRecord{
		ID:           "key-empty-models",
		Prefix:       "mg_em",
		SecretSHA256: []byte{0x00},
		Label:        "empty models",
		Models:       []string{},
		CreatedAt:    time.Now().UTC(),
		CreatedBy:    "admin",
	}
	if err := s.InsertKey(ctx, k); err != nil {
		t.Fatalf("InsertKey: %v", err)
	}
	got, _, err := s.KeyByID(ctx, "key-empty-models")
	if err != nil {
		t.Fatalf("KeyByID: %v", err)
	}
	if got.Models == nil || len(got.Models) != 0 {
		t.Errorf("Models = %v, want non-nil empty slice", got.Models)
	}
}

func TestListKeysOrdering(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	base := time.Now().UTC()
	for _, k := range []KeyRecord{
		{ID: "b", Prefix: "p", SecretSHA256: []byte{1}, Label: "l", CreatedAt: base, CreatedBy: "a"},
		{ID: "a", Prefix: "p", SecretSHA256: []byte{1}, Label: "l", CreatedAt: base, CreatedBy: "a"},
		{ID: "c", Prefix: "p", SecretSHA256: []byte{1}, Label: "l", CreatedAt: base.Add(-time.Hour), CreatedBy: "a"},
	} {
		if err := s.InsertKey(ctx, k); err != nil {
			t.Fatalf("InsertKey %s: %v", k.ID, err)
		}
	}
	keys, err := s.ListKeys(ctx)
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("len = %d, want 3", len(keys))
	}
	if keys[0].ID != "c" || keys[1].ID != "a" || keys[2].ID != "b" {
		t.Errorf("order = %s,%s,%s, want c,a,b", keys[0].ID, keys[1].ID, keys[2].ID)
	}
}

func TestRevokeKeyKeepsFirstTimestamp(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	k := KeyRecord{ID: "rk", Prefix: "p", SecretSHA256: []byte{1}, Label: "l", CreatedAt: time.Now().UTC(), CreatedBy: "a"}
	if err := s.InsertKey(ctx, k); err != nil {
		t.Fatalf("InsertKey: %v", err)
	}
	first := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.RevokeKey(ctx, "rk", first, "alice"); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	if err := s.RevokeKey(ctx, "rk", first.Add(time.Hour), "bob"); err != nil {
		t.Fatalf("second RevokeKey: %v", err)
	}
	got, _, err := s.KeyByID(ctx, "rk")
	if err != nil {
		t.Fatalf("KeyByID: %v", err)
	}
	if got.RevokedAt == nil || !got.RevokedAt.Equal(first) {
		t.Errorf("RevokedAt = %v, want %v", got.RevokedAt, first)
	}
	if got.RevokedBy == nil || *got.RevokedBy != "alice" {
		t.Errorf("RevokedBy = %v, want alice (first revoker wins)", got.RevokedBy)
	}
}

func TestRevokeKeyUnknownIDErrors(t *testing.T) {
	s, _ := openTestStore(t)
	if err := s.RevokeKey(context.Background(), "nope", time.Now().UTC(), "alice"); err == nil {
		t.Fatal("RevokeKey on unknown id: want error, got nil")
	}
}

func TestTouchLastUsed(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	k := KeyRecord{ID: "tl", Prefix: "p", SecretSHA256: []byte{1}, Label: "l", CreatedAt: time.Now().UTC(), CreatedBy: "a"}
	if err := s.InsertKey(ctx, k); err != nil {
		t.Fatalf("InsertKey: %v", err)
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.TouchLastUsed(ctx, "tl", at); err != nil {
		t.Fatalf("TouchLastUsed: %v", err)
	}
	got, _, err := s.KeyByID(ctx, "tl")
	if err != nil {
		t.Fatalf("KeyByID: %v", err)
	}
	if got.LastUsedAt == nil || !got.LastUsedAt.Equal(at) {
		t.Errorf("LastUsedAt = %v, want %v", got.LastUsedAt, at)
	}
}

func TestKeyByIDUnknown(t *testing.T) {
	s, _ := openTestStore(t)
	got, ok, err := s.KeyByID(context.Background(), "missing")
	if err != nil {
		t.Fatalf("KeyByID: %v", err)
	}
	if ok {
		t.Error("ok = true, want false")
	}
	if got.ID != "" {
		t.Errorf("got = %+v, want zero", got)
	}
}

func TestAddUsageAccumulates(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	u := Usage{Requests: 1, InputTokens: 10, OutputTokens: 20, CacheReadTokens: 5, CacheWriteTokens: 3, CostUSD: 0.25}
	if err := s.AddUsage(ctx, "2026-08", "k1", "m1", u); err != nil {
		t.Fatalf("AddUsage: %v", err)
	}
	if err := s.AddUsage(ctx, "2026-08", "k1", "m1", u); err != nil {
		t.Fatalf("second AddUsage: %v", err)
	}
	got, err := s.MonthUsage(ctx, "2026-08")
	if err != nil {
		t.Fatalf("MonthUsage: %v", err)
	}
	want := Usage{Requests: 2, InputTokens: 20, OutputTokens: 40, CacheReadTokens: 10, CacheWriteTokens: 6, CostUSD: 0.5}
	if got["k1"] != want {
		t.Errorf("usage = %+v, want %+v", got["k1"], want)
	}
}

func TestMonthSpend(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	if err := s.AddUsage(ctx, "2026-08", "k1", "m1", Usage{CostUSD: 1.5}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUsage(ctx, "2026-08", "k1", "m2", Usage{CostUSD: 2.5}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUsage(ctx, "2026-08", "k2", "m1", Usage{CostUSD: 4}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUsage(ctx, "2026-07", "k1", "m1", Usage{CostUSD: 100}); err != nil {
		t.Fatal(err)
	}
	total, err := s.MonthSpend(ctx, "2026-08")
	if err != nil {
		t.Fatalf("MonthSpend: %v", err)
	}
	if math.Abs(total-8) > 1e-9 {
		t.Errorf("MonthSpend = %v, want 8", total)
	}
	empty, err := s.MonthSpend(ctx, "2020-01")
	if err != nil {
		t.Fatalf("MonthSpend empty: %v", err)
	}
	if empty != 0 {
		t.Errorf("empty month spend = %v, want 0", empty)
	}
}

func TestMonthSpendByKey(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	if err := s.AddUsage(ctx, "2026-08", "k1", "m1", Usage{CostUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUsage(ctx, "2026-08", "k1", "m2", Usage{CostUSD: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUsage(ctx, "2026-08", "k2", "m1", Usage{CostUSD: 10}); err != nil {
		t.Fatal(err)
	}
	got, err := s.MonthSpendByKey(ctx, "2026-08", "k1")
	if err != nil {
		t.Fatalf("MonthSpendByKey: %v", err)
	}
	if math.Abs(got-3) > 1e-9 {
		t.Errorf("MonthSpendByKey = %v, want 3", got)
	}
}

func TestMonthUsageAggregatesModels(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	if err := s.AddUsage(ctx, "2026-08", "k1", "m1", Usage{Requests: 1, InputTokens: 100, CostUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUsage(ctx, "2026-08", "k1", "m2", Usage{Requests: 2, OutputTokens: 50, CostUSD: 2}); err != nil {
		t.Fatal(err)
	}
	got, err := s.MonthUsage(ctx, "2026-08")
	if err != nil {
		t.Fatalf("MonthUsage: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	want := Usage{Requests: 3, InputTokens: 100, OutputTokens: 50, CostUSD: 3}
	if got["k1"] != want {
		t.Errorf("usage = %+v, want %+v", got["k1"], want)
	}
}

func TestReopenPersists(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	k := KeyRecord{ID: "persist", Prefix: "p", SecretSHA256: []byte{9}, Label: "l", CreatedAt: time.Now().UTC(), CreatedBy: "a"}
	if err := s.InsertKey(ctx, k); err != nil {
		t.Fatalf("InsertKey: %v", err)
	}
	if err := s.AddUsage(ctx, "2026-08", "persist", "m", Usage{Requests: 7, CostUSD: 1.25}); err != nil {
		t.Fatalf("AddUsage: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	_, ok, err := s2.KeyByID(ctx, "persist")
	if err != nil || !ok {
		t.Fatalf("KeyByID after reopen: ok=%v err=%v", ok, err)
	}
	spend, err := s2.MonthSpend(ctx, "2026-08")
	if err != nil {
		t.Fatalf("MonthSpend after reopen: %v", err)
	}
	if math.Abs(spend-1.25) > 1e-9 {
		t.Errorf("spend = %v, want 1.25", spend)
	}
}
