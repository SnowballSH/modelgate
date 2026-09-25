package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

func Open(dir string) (*Store, error) {
	path := filepath.Join(dir, "modelgate.db")
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)",
		url.PathEscape(path),
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := migrate(context.Background(), db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// ProbeWrite commits one real write, so a read-only mount, a revoked
// permission or a full disk fails readiness instead of the next booking.
func (s *Store) ProbeWrite(ctx context.Context, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO readiness (id, probed_at) VALUES (1, ?)
ON CONFLICT (id) DO UPDATE SET probed_at = excluded.probed_at`, encodeTime(at))
	if err != nil {
		return fmt.Errorf("probe write: %w", err)
	}
	return nil
}

func encodeTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func encodeTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	v := encodeTime(*t)
	return &v
}

func decodeTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse time %q: %w", s, err)
	}
	return t.UTC(), nil
}

func decodeTimePtr(s *string) (*time.Time, error) {
	if s == nil {
		return nil, nil
	}
	t, err := decodeTime(*s)
	if err != nil {
		return nil, err
	}
	return &t, nil
}
