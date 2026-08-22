package server

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SnowballSH/modelgate/internal/accounting"
	"github.com/SnowballSH/modelgate/internal/keys"
	"github.com/SnowballSH/modelgate/internal/provider"
	"github.com/SnowballSH/modelgate/internal/store"
)

const identityHeader = "Remote-User"

type adminEnv struct {
	handler http.Handler
	store   *store.Store
	acct    *accounting.Accountant
	now     time.Time
}

func newAdminEnv(t *testing.T) *adminEnv {
	t.Helper()
	s := testStore(t)
	table := testTable(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	acct := accounting.New(s, 100)
	m := NewMetrics(100)
	handler := NewAdminHandler(s, acct, table, m, identityHeader, 100, func() time.Time { return now }, rand.Reader, nil)
	return &adminEnv{handler: handler, store: s, acct: acct, now: now}
}

func doAdmin(env *adminEnv, method, path, identity, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if identity != "" {
		req.Header.Set(identityHeader, identity)
	}
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	return rec
}

type keyJSON struct {
	ID            string   `json:"id"`
	Prefix        string   `json:"prefix"`
	Label         string   `json:"label"`
	Models        []string `json:"models"`
	QuotaUSD      *float64 `json:"quota_usd"`
	ExpiresAt     *string  `json:"expires_at"`
	RevokedAt     *string  `json:"revoked_at"`
	LastUsedAt    *string  `json:"last_used_at"`
	CreatedAt     string   `json:"created_at"`
	CreatedBy     string   `json:"created_by"`
	MonthSpendUSD float64  `json:"month_spend_usd"`
}

func createKey(t *testing.T, env *adminEnv, body string) (keyJSON, string) {
	t.Helper()
	rec := doAdmin(env, http.MethodPost, "/api/keys", "alice", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create key: status %d body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Key     keyJSON `json:"key"`
		FullKey string  `json:"full_key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Key, resp.FullKey
}

func TestAdminMissingIdentity(t *testing.T) {
	env := newAdminEnv(t)
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/keys"},
		{http.MethodPost, "/api/keys"},
		{http.MethodPost, "/api/keys/abc/revoke"},
		{http.MethodGet, "/api/usage"},
		{http.MethodGet, "/"},
	} {
		rec := doAdmin(env, route.method, route.path, "", "{}")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without identity: status %d", route.method, route.path, rec.Code)
		}
	}
}

func TestAdminCreateAndList(t *testing.T) {
	env := newAdminEnv(t)
	created, fullKey := createKey(t, env, `{"label":"ci bot"}`)

	id, secret, ok := keys.ParseBearer("Bearer " + fullKey)
	if !ok {
		t.Fatalf("full_key %q not parseable", fullKey)
	}
	if id != created.ID {
		t.Errorf("parsed id %q != created id %q", id, created.ID)
	}
	if created.CreatedBy != "alice" {
		t.Errorf("created_by = %q", created.CreatedBy)
	}

	rec := doAdmin(env, http.MethodGet, "/api/keys", "alice", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Error("list response contains the key secret")
	}
	var list struct {
		Keys []keyJSON `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Keys) != 1 || list.Keys[0].ID != created.ID || list.Keys[0].Label != "ci bot" {
		t.Errorf("list = %+v", list.Keys)
	}
}

func TestAdminCreateValidation(t *testing.T) {
	env := newAdminEnv(t)
	for name, body := range map[string]string{
		"empty label":    `{"label":""}`,
		"unknown model":  `{"label":"x","models":["gpt-9"]}`,
		"negative quota": `{"label":"x","quota_usd":-5}`,
	} {
		rec := doAdmin(env, http.MethodPost, "/api/keys", "alice", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d body %s", name, rec.Code, rec.Body.String())
		}
	}
}

func TestAdminRevoke(t *testing.T) {
	env := newAdminEnv(t)
	created, _ := createKey(t, env, `{"label":"to revoke"}`)

	rec := doAdmin(env, http.MethodPost, "/api/keys/"+created.ID+"/revoke", "alice", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: status %d body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Key keyJSON `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Key.RevokedAt == nil {
		t.Fatal("revoked_at not set")
	}
	first := *resp.Key.RevokedAt

	rec = doAdmin(env, http.MethodPost, "/api/keys/"+created.ID+"/revoke", "alice", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("second revoke: status %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Key.RevokedAt == nil || *resp.Key.RevokedAt != first {
		t.Errorf("second revoke changed timestamp: %v != %s", resp.Key.RevokedAt, first)
	}

	rec = doAdmin(env, http.MethodPost, "/api/keys/nosuchid/revoke", "alice", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown id revoke: status %d", rec.Code)
	}
}

func TestAdminUsage(t *testing.T) {
	env := newAdminEnv(t)
	created, _ := createKey(t, env, `{"label":"spender"}`)

	usage := store.Usage{InputTokens: 1000, OutputTokens: 500}
	pricing, _ := testTable(t).Resolve("claude-sonnet-5")
	if err := env.acct.Record(t.Context(), env.now, created.ID, "claude-sonnet-5", usage, pricing.Pricing); err != nil {
		t.Fatal(err)
	}
	wantSpend := 1000*3/1e6 + 500*15/1e6

	rec := doAdmin(env, http.MethodGet, "/api/usage", "alice", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("usage: status %d", rec.Code)
	}
	var resp struct {
		Month     string  `json:"month"`
		BudgetUSD float64 `json:"budget_usd"`
		SpendUSD  float64 `json:"spend_usd"`
		Keys      []struct {
			ID       string   `json:"id"`
			Label    string   `json:"label"`
			SpendUSD float64  `json:"spend_usd"`
			QuotaUSD *float64 `json:"quota_usd"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Month != "2026-08" || resp.BudgetUSD != 100 {
		t.Errorf("month %q budget %v", resp.Month, resp.BudgetUSD)
	}
	if math.Abs(resp.SpendUSD-wantSpend) > 1e-12 {
		t.Errorf("spend = %v, want %v", resp.SpendUSD, wantSpend)
	}
	if len(resp.Keys) != 1 || resp.Keys[0].ID != created.ID || math.Abs(resp.Keys[0].SpendUSD-wantSpend) > 1e-12 {
		t.Errorf("keys = %+v", resp.Keys)
	}
	if resp.Keys[0].QuotaUSD != nil {
		t.Errorf("quota = %v, want null", *resp.Keys[0].QuotaUSD)
	}
}

func TestAdminPublicFullFlow(t *testing.T) {
	upstream := httptest.NewServer(fullResponseHandler())
	t.Cleanup(upstream.Close)

	s := testStore(t)
	table := testTable(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return now }
	acct := accounting.New(s, 100)
	m := NewMetrics(100)
	admin := NewAdminHandler(s, acct, table, m, identityHeader, 100, nowFn, rand.Reader, nil)
	guards := NewGuards(s, acct, table, 1000, 8, 1<<20, nowFn)
	client := provider.NewClient(upstream.URL, "k", upstream.Client())
	breaker := provider.NewBreaker(100, time.Minute, nowFn)
	public := NewPublicHandler(guards, table, acct, s, Upstreams{Anthropic: client, AnthropicBreaker: breaker}, m, 4096, 1<<20, 5*time.Second, nowFn)
	adminE := &adminEnv{handler: admin, store: s, acct: acct, now: now}
	publicE := &publicEnv{handler: public, store: s, acct: acct, now: now}

	created, fullKey := createKey(t, adminE, `{"label":"e2e"}`)
	auth := "Bearer " + fullKey

	rec := doPublic(publicE, http.MethodPost, "/v1/chat/completions", auth, chatBody(false))
	if rec.Code != http.StatusOK {
		t.Fatalf("chat with created key: status %d body %s", rec.Code, rec.Body.String())
	}

	rec = doAdmin(adminE, http.MethodPost, fmt.Sprintf("/api/keys/%s/revoke", created.ID), "alice", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: status %d", rec.Code)
	}

	rec = doPublic(publicE, http.MethodPost, "/v1/chat/completions", auth, chatBody(false))
	if rec.Code != http.StatusUnauthorized || decodeErrorCode(t, rec) != CodeInvalidAPIKey {
		t.Errorf("chat after revoke: status %d code %s", rec.Code, decodeErrorCode(t, rec))
	}
}
