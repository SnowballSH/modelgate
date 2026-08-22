package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type KeyRecord struct {
	ID, Prefix   string
	SecretSHA256 []byte
	Label        string
	Models       []string
	QuotaUSD     *float64
	ExpiresAt    *time.Time
	RevokedAt    *time.Time
	RevokedBy    *string
	LastUsedAt   *time.Time
	CreatedAt    time.Time
	CreatedBy    string
}

const keyColumns = "id, prefix, secret_sha256, label, models, quota_usd, expires_at, revoked_at, revoked_by, last_used_at, created_at, created_by"

func encodeModels(models []string) (*string, error) {
	if models == nil {
		return nil, nil
	}
	b, err := json.Marshal(models)
	if err != nil {
		return nil, fmt.Errorf("encode models: %w", err)
	}
	v := string(b)
	return &v, nil
}

func decodeModels(s *string) ([]string, error) {
	if s == nil {
		return nil, nil
	}
	var models []string
	if err := json.Unmarshal([]byte(*s), &models); err != nil {
		return nil, fmt.Errorf("decode models %q: %w", *s, err)
	}
	if models == nil {
		models = []string{}
	}
	return models, nil
}

func (s *Store) InsertKey(ctx context.Context, k KeyRecord) error {
	models, err := encodeModels(k.Models)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		"INSERT INTO keys ("+keyColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		k.ID, k.Prefix, k.SecretSHA256, k.Label, models, k.QuotaUSD,
		encodeTimePtr(k.ExpiresAt), encodeTimePtr(k.RevokedAt), k.RevokedBy,
		encodeTimePtr(k.LastUsedAt), encodeTime(k.CreatedAt), k.CreatedBy,
	)
	if err != nil {
		return fmt.Errorf("insert key %s: %w", k.ID, err)
	}
	return nil
}

type keyScanner interface {
	Scan(dest ...any) error
}

func scanKey(row keyScanner) (KeyRecord, error) {
	var (
		k                                        KeyRecord
		models, expiresAt, revokedAt, lastUsedAt *string
		createdAt                                string
	)
	if err := row.Scan(
		&k.ID, &k.Prefix, &k.SecretSHA256, &k.Label, &models, &k.QuotaUSD,
		&expiresAt, &revokedAt, &k.RevokedBy, &lastUsedAt, &createdAt, &k.CreatedBy,
	); err != nil {
		return KeyRecord{}, err
	}
	var err error
	if k.Models, err = decodeModels(models); err != nil {
		return KeyRecord{}, err
	}
	if k.ExpiresAt, err = decodeTimePtr(expiresAt); err != nil {
		return KeyRecord{}, err
	}
	if k.RevokedAt, err = decodeTimePtr(revokedAt); err != nil {
		return KeyRecord{}, err
	}
	if k.LastUsedAt, err = decodeTimePtr(lastUsedAt); err != nil {
		return KeyRecord{}, err
	}
	if k.CreatedAt, err = decodeTime(createdAt); err != nil {
		return KeyRecord{}, err
	}
	return k, nil
}

func (s *Store) KeyByID(ctx context.Context, id string) (KeyRecord, bool, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+keyColumns+" FROM keys WHERE id = ?", id)
	k, err := scanKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return KeyRecord{}, false, nil
	}
	if err != nil {
		return KeyRecord{}, false, fmt.Errorf("key %s: %w", id, err)
	}
	return k, true, nil
}

func (s *Store) ListKeys(ctx context.Context) ([]KeyRecord, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+keyColumns+" FROM keys ORDER BY created_at, id")
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	defer rows.Close()
	var keys []KeyRecord
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, fmt.Errorf("list keys: %w", err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	return keys, nil
}

func (s *Store) RevokeKey(ctx context.Context, id string, at time.Time, by string) error {
	res, err := s.db.ExecContext(ctx,
		"UPDATE keys SET revoked_at = COALESCE(revoked_at, ?), revoked_by = COALESCE(revoked_by, ?) WHERE id = ?",
		encodeTime(at), by, id,
	)
	if err != nil {
		return fmt.Errorf("revoke key %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("revoke key %s: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("revoke key %s: key not found", id)
	}
	return nil
}

func (s *Store) TouchLastUsed(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE keys SET last_used_at = ? WHERE id = ?",
		encodeTime(at), id,
	)
	if err != nil {
		return fmt.Errorf("touch key %s: %w", id, err)
	}
	return nil
}
