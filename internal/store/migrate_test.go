package store

import (
	"context"
	"database/sql"
	"fmt"
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

	s, err := Open(dir)
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
		s, err := Open(dir)
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
	if s, err := Open(dir); err == nil {
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
