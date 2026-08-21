package server

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SnowballSH/modelgate/internal/accounting"
	"github.com/SnowballSH/modelgate/internal/keys"
	"github.com/SnowballSH/modelgate/internal/models"
	"github.com/SnowballSH/modelgate/internal/store"
)

const sonnet = "claude-sonnet-5"

const tableJSON = `{
	"models": {
		"claude-sonnet-5": {
			"provider_model": "claude-sonnet-5-20250929",
			"input_usd_per_mtok": 3,
			"output_usd_per_mtok": 15,
			"cache_read_usd_per_mtok": 0.30,
			"cache_write_usd_per_mtok": 3.75
		}
	}
}`

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestTable(t *testing.T) *models.Table {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(tableJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	table, err := models.LoadTable(path)
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func insertKey(t *testing.T, s *store.Store, at time.Time, mutate func(*store.KeyRecord)) keys.Generated {
	t.Helper()
	gen, err := keys.Generate(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rec := store.KeyRecord{
		ID:           gen.ID,
		Prefix:       gen.Prefix,
		SecretSHA256: gen.SecretSHA256[:],
		Label:        "test",
		CreatedAt:    at,
		CreatedBy:    "guards_test",
	}
	if mutate != nil {
		mutate(&rec)
	}
	if err := s.InsertKey(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	return gen
}

func bearer(full string) string { return "Bearer " + full }

func mustDeny(t *testing.T, g *Guards, auth string, bodyLen int64, model, wantCode string) {
	t.Helper()
	_, code, ok := g.Admit(context.Background(), auth, bodyLen, model)
	if ok {
		t.Fatalf("Admit unexpectedly succeeded, want code %q", wantCode)
	}
	if code != wantCode {
		t.Fatalf("Admit code = %q, want %q", code, wantCode)
	}
}

func mustAdmit(t *testing.T, g *Guards, auth string, bodyLen int64, model string) Admission {
	t.Helper()
	adm, code, ok := g.Admit(context.Background(), auth, bodyLen, model)
	if !ok {
		t.Fatalf("Admit failed with code %q, want success", code)
	}
	return adm
}

func TestAdmitGuardOrder(t *testing.T) {
	s := newTestStore(t)
	table := newTestTable(t)
	clk := &clock{t: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)}
	acct := accounting.New(s, 100)
	g := NewGuards(s, acct, table, 1, 1, 1024, clk.now)

	valid := insertKey(t, s, clk.t, nil)
	unknown, err := keys.Generate(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	revokedAt := clk.t.Add(-time.Hour)
	revoked := insertKey(t, s, clk.t, func(r *store.KeyRecord) { r.RevokedAt = &revokedAt })
	expiresAt := clk.t.Add(-time.Minute)
	expired := insertKey(t, s, clk.t, func(r *store.KeyRecord) { r.ExpiresAt = &expiresAt })
	restricted := insertKey(t, s, clk.t, func(r *store.KeyRecord) { r.Models = []string{"other-model"} })
	zeroQuota := 0.0
	quotaed := insertKey(t, s, clk.t, func(r *store.KeyRecord) { r.QuotaUSD = &zeroQuota })
	fresh := insertKey(t, s, clk.t, nil)

	mustDeny(t, g, "garbage", 2048, "no-such-model", CodeRequestTooLarge)
	mustDeny(t, g, "garbage", 10, "no-such-model", CodeInvalidAPIKey)
	mustDeny(t, g, bearer(unknown.Full), 10, "no-such-model", CodeInvalidAPIKey)
	mustDeny(t, g, bearer(valid.Prefix+"_wrongsecret"), 10, "no-such-model", CodeInvalidAPIKey)
	mustDeny(t, g, bearer(revoked.Full), 10, "no-such-model", CodeInvalidAPIKey)
	mustDeny(t, g, bearer(expired.Full), 10, "no-such-model", CodeInvalidAPIKey)

	adm := mustAdmit(t, g, bearer(valid.Full), 10, sonnet)
	adm.Release()
	mustDeny(t, g, bearer(valid.Full), 10, sonnet, CodeRateLimited)
	clk.advance(61 * time.Second)

	mustDeny(t, g, bearer(valid.Full), 10, "no-such-model", CodeModelNotFound)
	mustDeny(t, g, bearer(restricted.Full), 10, sonnet, CodeModelNotFound)

	clk.advance(61 * time.Second)
	held := mustAdmit(t, g, bearer(valid.Full), 10, sonnet)
	mustDeny(t, g, bearer(fresh.Full), 10, sonnet, CodeRateLimited)
	held.Release()

	clk.advance(61 * time.Second)
	mustDeny(t, g, bearer(quotaed.Full), 10, sonnet, CodeQuotaExhausted)

	usage := store.Usage{InputTokens: 100_000}
	pricing, _ := table.Resolve(sonnet)
	if err := acct.Record(context.Background(), clk.t, valid.ID, sonnet, usage, pricing.Pricing); err != nil {
		t.Fatal(err)
	}
	tight := NewGuards(s, accounting.New(s, 0.01), table, 100, 1, 1024, clk.now)
	mustDeny(t, tight, bearer(fresh.Full), 10, sonnet, CodeBudgetExhausted)

	adm = mustAdmit(t, g, bearer(fresh.Full), 10, sonnet)
	defer adm.Release()
	if adm.Key.ID != fresh.ID {
		t.Errorf("admitted key = %q, want %q", adm.Key.ID, fresh.ID)
	}
	if adm.Model.ProviderModel != "claude-sonnet-5-20250929" {
		t.Errorf("admitted model = %q", adm.Model.ProviderModel)
	}
}

func TestAdmitRateLimitRefill(t *testing.T) {
	s := newTestStore(t)
	clk := &clock{t: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)}
	g := NewGuards(s, accounting.New(s, 100), newTestTable(t), 1, 4, 1024, clk.now)
	key := insertKey(t, s, clk.t, nil)

	mustAdmit(t, g, bearer(key.Full), 10, sonnet).Release()
	mustDeny(t, g, bearer(key.Full), 10, sonnet, CodeRateLimited)
	clk.advance(61 * time.Second)
	mustAdmit(t, g, bearer(key.Full), 10, sonnet).Release()
}

func TestAdmitConcurrency(t *testing.T) {
	s := newTestStore(t)
	clk := &clock{t: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)}
	g := NewGuards(s, accounting.New(s, 100), newTestTable(t), 1000, 1, 1024, clk.now)
	a := insertKey(t, s, clk.t, nil)
	b := insertKey(t, s, clk.t, nil)

	held := mustAdmit(t, g, bearer(a.Full), 10, sonnet)
	mustDeny(t, g, bearer(b.Full), 10, sonnet, CodeRateLimited)
	held.Release()
	mustAdmit(t, g, bearer(b.Full), 10, sonnet).Release()
}

func TestReleaseIdempotent(t *testing.T) {
	s := newTestStore(t)
	clk := &clock{t: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)}
	g := NewGuards(s, accounting.New(s, 100), newTestTable(t), 1000, 1, 1024, clk.now)
	a := insertKey(t, s, clk.t, nil)
	b := insertKey(t, s, clk.t, nil)
	c := insertKey(t, s, clk.t, nil)

	adm := mustAdmit(t, g, bearer(a.Full), 10, sonnet)
	adm.Release()
	adm.Release()
	third := mustAdmit(t, g, bearer(b.Full), 10, sonnet)
	mustDeny(t, g, bearer(c.Full), 10, sonnet, CodeRateLimited)
	third.Release()
}

func TestRevokedOverQuotaReportsInvalidKey(t *testing.T) {
	s := newTestStore(t)
	clk := &clock{t: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)}
	g := NewGuards(s, accounting.New(s, 100), newTestTable(t), 1000, 4, 1024, clk.now)
	revokedAt := clk.t
	zeroQuota := 0.0
	key := insertKey(t, s, clk.t, func(r *store.KeyRecord) {
		r.RevokedAt = &revokedAt
		r.QuotaUSD = &zeroQuota
	})
	mustDeny(t, g, bearer(key.Full), 10, sonnet, CodeInvalidAPIKey)
}

func TestStoreErrorReportsInternal(t *testing.T) {
	s := newTestStore(t)
	clk := &clock{t: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)}
	g := NewGuards(s, accounting.New(s, 100), newTestTable(t), 1000, 4, 1024, clk.now)
	key := insertKey(t, s, clk.t, nil)
	s.Close()
	mustDeny(t, g, bearer(key.Full), 10, sonnet, CodeInternal)
}
