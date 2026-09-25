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

var smallDemand = accounting.Demand{InputTokens: 10, OutputTokens: 10}

func admitBearer(g *Guards, auth, model string, demand accounting.Demand) (Admission, *Refusal) {
	key, ok, err := g.Authenticate(context.Background(), auth)
	switch {
	case err != nil:
		return Admission{}, &Refusal{Code: CodeInternal}
	case !ok:
		return Admission{}, &Refusal{Code: CodeInvalidAPIKey}
	}
	resolved, refusal := g.ResolveModel(key, model)
	if refusal != nil {
		return Admission{}, refusal
	}
	return g.Admit(context.Background(), key, resolved, demand)
}

func mustDeny(t *testing.T, g *Guards, auth, model, wantCode string) *Refusal {
	t.Helper()
	_, refusal := admitBearer(g, auth, model, smallDemand)
	if refusal == nil {
		t.Fatalf("Admit unexpectedly succeeded, want code %q", wantCode)
	}
	if refusal.Code != wantCode {
		t.Fatalf("Admit code = %q, want %q", refusal.Code, wantCode)
	}
	return refusal
}

func mustAdmit(t *testing.T, g *Guards, auth, model string) Admission {
	t.Helper()
	adm, refusal := admitBearer(g, auth, model, smallDemand)
	if refusal != nil {
		t.Fatalf("Admit failed with code %q, want success", refusal.Code)
	}
	return adm
}

func TestAdmitGuardOrder(t *testing.T) {
	s := newTestStore(t)
	table := newTestTable(t)
	clk := &clock{t: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)}
	acct := accounting.New(s, 100)
	g := NewGuards(s, acct, table, 1, 1, clk.now)

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

	mustDeny(t, g, "garbage", "no-such-model", CodeInvalidAPIKey)
	mustDeny(t, g, bearer(unknown.Full), "no-such-model", CodeInvalidAPIKey)
	mustDeny(t, g, bearer(valid.Prefix+"_wrongsecret"), "no-such-model", CodeInvalidAPIKey)
	mustDeny(t, g, bearer(revoked.Full), "no-such-model", CodeInvalidAPIKey)
	mustDeny(t, g, bearer(expired.Full), "no-such-model", CodeInvalidAPIKey)

	adm := mustAdmit(t, g, bearer(valid.Full), sonnet)
	adm.Release()
	mustDeny(t, g, bearer(valid.Full), sonnet, CodeRateLimited)
	clk.advance(61 * time.Second)

	mustDeny(t, g, bearer(valid.Full), "no-such-model", CodeModelNotFound)
	mustDeny(t, g, bearer(restricted.Full), sonnet, CodeModelNotFound)

	clk.advance(61 * time.Second)
	held := mustAdmit(t, g, bearer(valid.Full), sonnet)
	mustDeny(t, g, bearer(fresh.Full), sonnet, CodeRateLimited)
	held.Release()

	clk.advance(61 * time.Second)
	mustDeny(t, g, bearer(quotaed.Full), sonnet, CodeQuotaExhausted)

	usage := store.Usage{InputTokens: 100_000}
	pricing, _ := table.Resolve(sonnet)
	if err := acct.Record(context.Background(), clk.t, valid.ID, sonnet, usage, pricing.Pricing); err != nil {
		t.Fatal(err)
	}
	tight := NewGuards(s, accounting.New(s, 0.01), table, 100, 1, clk.now)
	mustDeny(t, tight, bearer(fresh.Full), sonnet, CodeBudgetExhausted)

	adm = mustAdmit(t, g, bearer(fresh.Full), sonnet)
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
	g := NewGuards(s, accounting.New(s, 100), newTestTable(t), 1, 4, clk.now)
	key := insertKey(t, s, clk.t, nil)

	mustAdmit(t, g, bearer(key.Full), sonnet).Release()
	mustDeny(t, g, bearer(key.Full), sonnet, CodeRateLimited)
	clk.advance(61 * time.Second)
	mustAdmit(t, g, bearer(key.Full), sonnet).Release()
}

func TestAdmitConcurrency(t *testing.T) {
	s := newTestStore(t)
	clk := &clock{t: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)}
	g := NewGuards(s, accounting.New(s, 100), newTestTable(t), 1000, 1, clk.now)
	a := insertKey(t, s, clk.t, nil)
	b := insertKey(t, s, clk.t, nil)

	held := mustAdmit(t, g, bearer(a.Full), sonnet)
	if refusal := mustDeny(t, g, bearer(b.Full), sonnet, CodeRateLimited); refusal.RetryAfter != slotRetryAfter {
		t.Errorf("slot refusal RetryAfter = %v, want %v", refusal.RetryAfter, slotRetryAfter)
	}
	held.Release()
	mustAdmit(t, g, bearer(b.Full), sonnet).Release()
}

func TestReleaseIdempotent(t *testing.T) {
	s := newTestStore(t)
	clk := &clock{t: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)}
	g := NewGuards(s, accounting.New(s, 100), newTestTable(t), 1000, 1, clk.now)
	a := insertKey(t, s, clk.t, nil)
	b := insertKey(t, s, clk.t, nil)
	c := insertKey(t, s, clk.t, nil)

	adm := mustAdmit(t, g, bearer(a.Full), sonnet)
	adm.Release()
	adm.Release()
	third := mustAdmit(t, g, bearer(b.Full), sonnet)
	mustDeny(t, g, bearer(c.Full), sonnet, CodeRateLimited)
	third.Release()
}

func TestRevokedOverQuotaReportsInvalidKey(t *testing.T) {
	s := newTestStore(t)
	clk := &clock{t: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)}
	g := NewGuards(s, accounting.New(s, 100), newTestTable(t), 1000, 4, clk.now)
	revokedAt := clk.t
	zeroQuota := 0.0
	key := insertKey(t, s, clk.t, func(r *store.KeyRecord) {
		r.RevokedAt = &revokedAt
		r.QuotaUSD = &zeroQuota
	})
	mustDeny(t, g, bearer(key.Full), sonnet, CodeInvalidAPIKey)
}

func TestStoreErrorReportsInternal(t *testing.T) {
	s := newTestStore(t)
	clk := &clock{t: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)}
	g := NewGuards(s, accounting.New(s, 100), newTestTable(t), 1000, 4, clk.now)
	key := insertKey(t, s, clk.t, nil)
	s.Close()
	mustDeny(t, g, bearer(key.Full), sonnet, CodeInternal)
}
