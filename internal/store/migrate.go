package store

import (
	"context"
	"database/sql"
	"fmt"
)

// migrations[i] takes the schema from version i to version i+1, recorded in
// PRAGMA user_version. Version 1 is the v0.5.0 schema, written with IF NOT
// EXISTS so a v0.5.0 database, which never set user_version, is stamped
// without being altered. Version 3 keys usage by the provider that served
// it, attributing older rows through temp.upgrade_attribution. Append new
// steps; never edit an existing one.
var migrations = []string{
	`CREATE TABLE IF NOT EXISTS keys (
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
);`,
	`CREATE TABLE readiness (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	probed_at TEXT NOT NULL
);`,
	`CREATE TABLE usage_by_provider (
	month TEXT NOT NULL,
	key_id TEXT NOT NULL,
	model TEXT NOT NULL,
	provider TEXT NOT NULL,
	requests INTEGER NOT NULL DEFAULT 0,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens INTEGER NOT NULL DEFAULT 0,
	cache_write_tokens INTEGER NOT NULL DEFAULT 0,
	cost_usd REAL NOT NULL DEFAULT 0,
	PRIMARY KEY (month, key_id, model, provider)
);
INSERT INTO usage_by_provider (month, key_id, model, provider, requests, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd)
SELECT u.month, u.key_id, u.model, COALESCE(a.provider, ''), u.requests, u.input_tokens, u.output_tokens, u.cache_read_tokens, u.cache_write_tokens, u.cost_usd
FROM usage u LEFT JOIN temp.upgrade_attribution a ON a.model = u.model;
DROP TABLE usage;
ALTER TABLE usage_by_provider RENAME TO usage;`,
}

const SchemaVersion = 3

// migrate brings the schema to SchemaVersion. modelProviders names the
// provider serving each model now, which is the best record of who served
// the usage rows written before usage carried a provider.
func migrate(ctx context.Context, db *sql.DB, modelProviders map[string]string) error {
	if len(migrations) != SchemaVersion {
		return fmt.Errorf("schema version %d does not match %d migrations", SchemaVersion, len(migrations))
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := stageAttribution(ctx, conn, modelProviders); err != nil {
		return err
	}
	defer func() { _, _ = conn.ExecContext(context.WithoutCancel(ctx), "DROP TABLE temp.upgrade_attribution") }()
	for {
		done, err := step(ctx, conn)
		if err != nil || done {
			return err
		}
	}
}

func stageAttribution(ctx context.Context, conn *sql.Conn, modelProviders map[string]string) error {
	if _, err := conn.ExecContext(ctx, "CREATE TEMP TABLE upgrade_attribution (model TEXT PRIMARY KEY, provider TEXT NOT NULL)"); err != nil {
		return fmt.Errorf("stage attribution: %w", err)
	}
	for model, provider := range modelProviders {
		if _, err := conn.ExecContext(ctx, "INSERT INTO temp.upgrade_attribution (model, provider) VALUES (?, ?)", model, provider); err != nil {
			return fmt.Errorf("stage attribution of %s: %w", model, err)
		}
	}
	return nil
}

// step applies the next migration inside one immediate transaction, so the
// version it reads is the version it moves from.
func step(ctx context.Context, conn *sql.Conn) (done bool, err error) {
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if err != nil {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()
	var current int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return false, fmt.Errorf("read user_version: %w", err)
	}
	switch {
	case current > SchemaVersion:
		return false, fmt.Errorf("the database is at schema version %d, newer than this release's %d", current, SchemaVersion)
	case current == SchemaVersion:
		_, err := conn.ExecContext(ctx, "COMMIT")
		return true, err
	}
	next := current + 1
	if _, err := conn.ExecContext(ctx, migrations[current]); err != nil {
		return false, fmt.Errorf("schema version %d: %w", next, err)
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", next)); err != nil {
		return false, fmt.Errorf("stamp schema version %d: %w", next, err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return false, fmt.Errorf("commit schema version %d: %w", next, err)
	}
	return false, nil
}
