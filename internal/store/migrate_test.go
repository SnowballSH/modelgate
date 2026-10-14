package store

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"net/url"
	"path/filepath"
	"testing"
	"time"
)

// v050Schema is the schema modelgate v0.5.0 created, verbatim: no
// user_version, tables created with IF NOT EXISTS on every start.
const v050Schema = `
CREATE TABLE IF NOT EXISTS keys (
	id TEXT PRIMARY KEY,
	prefix TEXT NOT NULL,
	secret_sha256 BLOB NOT NULL,
	label TEXT NOT NULL,
	models TEXT,
	quota_usd REAL,
	expires_at TEXT,
	revoked_at TEXT,
	revoked_by TEXT,
	last_used_at TEXT,
	created_at TEXT NOT NULL,
	created_by TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS usage (
	month TEXT NOT NULL,
	key_id TEXT NOT NULL,
	model TEXT NOT NULL,
	requests INTEGER NOT NULL DEFAULT 0,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens INTEGER NOT NULL DEFAULT 0,
	cache_write_tokens INTEGER NOT NULL DEFAULT 0,
	cost_usd REAL NOT NULL DEFAULT 0,
	PRIMARY KEY (month, key_id, model)
);
`

func rawDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(filepath.Join(dir, "modelgate.db")))
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestOpenStampsAV050DatabaseWithoutDataLoss(t *testing.T) {
	dir := t.TempDir()
	legacy := rawDB(t, dir)
	if _, err := legacy.Exec(v050Schema); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	if _, err := legacy.Exec(
		`INSERT INTO keys (id, prefix, secret_sha256, label, models, quota_usd, created_at, created_by)
		 VALUES ('abcdefgh', 'mg_abcdefgh', x'0102', 'legacy', '["claude-sonnet-5"]', 5, ?, 'alice')`, created); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(
		`INSERT INTO usage (month, key_id, model, requests, input_tokens, output_tokens, cost_usd)
		 VALUES ('2026-08', 'abcdefgh', 'claude-sonnet-5', 3, 300, 90, 0.75)`); err != nil {
		t.Fatal(err)
	}
	if got := userVersion(t, legacy); got != 0 {
		t.Fatalf("a v0.5.0 database reports user_version %d, want 0", got)
	}
	legacy.Close()

	s, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open on a v0.5.0 database: %v", err)
	}
	ctx := context.Background()
	key, found, err := s.KeyByID(ctx, "abcdefgh")
	if err != nil || !found || key.Label != "legacy" || key.QuotaUSD == nil || *key.QuotaUSD != 5 || len(key.Models) != 1 {
		t.Fatalf("legacy key after migration = %+v found=%v err=%v", key, found, err)
	}
	spend, err := s.MonthSpend(ctx, "2026-08")
	if err != nil || spend != 0.75 {
		t.Fatalf("legacy spend after migration = %v err=%v, want 0.75", spend, err)
	}
	s.Close()

	check := rawDB(t, dir)
	defer check.Close()
	if got := userVersion(t, check); got != SchemaVersion {
		t.Fatalf("user_version after Open = %d, want %d", got, SchemaVersion)
	}
}

func TestOpenIsIdempotentAtTheCurrentVersion(t *testing.T) {
	dir := t.TempDir()
	for i := range 3 {
		s, err := Open(dir, nil)
		if err != nil {
			t.Fatalf("Open #%d: %v", i, err)
		}
		s.Close()
	}
	check := rawDB(t, dir)
	defer check.Close()
	if got := userVersion(t, check); got != SchemaVersion {
		t.Fatalf("user_version = %d, want %d", got, SchemaVersion)
	}
}

func TestOpenRefusesADatabaseFromANewerRelease(t *testing.T) {
	dir := t.TempDir()
	future := rawDB(t, dir)
	if _, err := future.Exec(fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	future.Close()
	if s, err := Open(dir, nil); err == nil {
		s.Close()
		t.Fatal("Open accepted a database stamped by a newer release")
	}
}

func TestProbeWriteFailsOnAReadOnlyDatabase(t *testing.T) {
	s, dir := openTestStore(t)
	now := time.Now()
	if err := s.ProbeWrite(context.Background(), now); err != nil {
		t.Fatalf("ProbeWrite on a writable store: %v", err)
	}
	if err := s.ProbeWrite(context.Background(), now.Add(time.Second)); err != nil {
		t.Fatalf("second ProbeWrite: %v", err)
	}

	ro, err := sql.Open("sqlite", "file:"+url.PathEscape(filepath.Join(dir, "modelgate.db"))+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	readOnly := &Store{db: ro}
	defer readOnly.Close()
	if err := readOnly.Ping(context.Background()); err != nil {
		t.Fatalf("the read-only store must still ping: %v", err)
	}
	if err := readOnly.ProbeWrite(context.Background(), now); err == nil {
		t.Fatal("ProbeWrite succeeded on a read-only database")
	}
}

// v071Schema is the schema modelgate v0.6.0 through v0.7.1 left behind:
// the v0.5.0 tables, the readiness table, and user_version 2.
const v071Schema = v050Schema + `
CREATE TABLE readiness (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	probed_at TEXT NOT NULL
);
PRAGMA user_version = 2;
`

type legacyRow struct {
	month, key, model string
	requests, input   int64
	cost              float64
}

func writeV071Database(t *testing.T, dir string, rows []legacyRow) {
	t.Helper()
	legacy := rawDB(t, dir)
	defer legacy.Close()
	if _, err := legacy.Exec(v071Schema); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if _, err := legacy.Exec(
			`INSERT INTO usage (month, key_id, model, requests, input_tokens, cost_usd) VALUES (?, ?, ?, ?, ?, ?)`,
			r.month, r.key, r.model, r.requests, r.input, r.cost); err != nil {
			t.Fatal(err)
		}
	}
}

type spendSnapshot struct {
	total, keyA, keyB float64
	perProvider       map[string]float64
	usage             map[string]Usage
}

func snapshot(t *testing.T, s *Store, month string) spendSnapshot {
	t.Helper()
	ctx := context.Background()
	var snap spendSnapshot
	var err error
	if snap.total, err = s.MonthSpend(ctx, month); err != nil {
		t.Fatal(err)
	}
	if snap.keyA, err = s.MonthSpendByKey(ctx, month, "keyA"); err != nil {
		t.Fatal(err)
	}
	if snap.keyB, err = s.MonthSpendByKey(ctx, month, "keyB"); err != nil {
		t.Fatal(err)
	}
	if snap.perProvider, err = s.MonthSpendPerProvider(ctx, month); err != nil {
		t.Fatal(err)
	}
	if snap.usage, err = s.MonthUsage(ctx, month); err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestOpenUpgradesAV071DatabaseAttributingSpendByModel(t *testing.T) {
	dir := t.TempDir()
	writeV071Database(t, dir, []legacyRow{
		{"2026-10", "keyA", "claude-sonnet-5", 3, 300, 1.5},
		{"2026-10", "keyA", "gpt-5", 2, 200, 2},
		{"2026-10", "keyB", "retired-model", 1, 100, 0.25},
		{"2026-09", "keyA", "claude-sonnet-5", 9, 900, 10},
	})

	s, err := Open(dir, map[string]string{"claude-sonnet-5": "anthropic", "gpt-5": "openai"})
	if err != nil {
		t.Fatalf("Open on a v0.7.1 database: %v", err)
	}
	upgraded := snapshot(t, s, "2026-10")
	if upgraded.total != 3.75 || upgraded.keyA != 3.5 || upgraded.keyB != 0.25 {
		t.Errorf("spend after the upgrade = total %v keyA %v keyB %v, want 3.75, 3.5, 0.25", upgraded.total, upgraded.keyA, upgraded.keyB)
	}
	wantPerProvider := map[string]float64{"anthropic": 1.5, "openai": 2, Unattributed: 0.25}
	if !maps.Equal(upgraded.perProvider, wantPerProvider) {
		t.Errorf("per-provider spend after the upgrade = %v, want %v", upgraded.perProvider, wantPerProvider)
	}
	if got := upgraded.usage["keyA"]; got.Requests != 5 || got.InputTokens != 500 {
		t.Errorf("keyA usage after the upgrade = %+v, want 5 requests and 500 input tokens", got)
	}
	if september, err := s.MonthSpendByProvider(context.Background(), "2026-09", "anthropic"); err != nil || september != 10 {
		t.Errorf("September anthropic spend = %v, %v; want 10", september, err)
	}
	s.Close()

	again, err := Open(dir, map[string]string{"claude-sonnet-5": "openai", "retired-model": "openai"})
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer again.Close()
	if reopened := snapshot(t, again, "2026-10"); !maps.Equal(reopened.perProvider, wantPerProvider) || reopened.total != upgraded.total {
		t.Errorf("a second Open with a different table re-attributed spend: %v (total %v), want %v (total %v)",
			reopened.perProvider, reopened.total, wantPerProvider, upgraded.total)
	}
	check := rawDB(t, dir)
	defer check.Close()
	if got := userVersion(t, check); got != SchemaVersion {
		t.Fatalf("user_version = %d, want %d", got, SchemaVersion)
	}
}

func TestUpgradedRowsMergeWithNewBookingsOfTheSameProvider(t *testing.T) {
	dir := t.TempDir()
	writeV071Database(t, dir, []legacyRow{{"2026-10", "keyA", "claude-sonnet-5", 3, 300, 1.5}})
	s, err := Open(dir, map[string]string{"claude-sonnet-5": "anthropic"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.AddUsage(ctx, "2026-10", "keyA", "claude-sonnet-5", "anthropic", Usage{Requests: 1, CostUSD: 0.5}); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM usage").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if spend, err := s.MonthSpendByProvider(ctx, "2026-10", "anthropic"); err != nil || spend != 2 || rows != 1 {
		t.Errorf("anthropic spend %v (err %v) over %d rows, want 2 over one row", spend, err, rows)
	}
}
