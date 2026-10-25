package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type searchKeyJSON struct {
	ID        string `json:"id"`
	WebSearch *bool  `json:"web_search"`
}

func decodeSearchKey(t *testing.T, body []byte) searchKeyJSON {
	t.Helper()
	var resp struct {
		Key searchKeyJSON `json:"key"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Key
}

func TestAdminCreateKeyWebSearch(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{"label":"chat","web_search":true}`, true},
		{`{"label":"agent","web_search":false}`, false},
		{`{"label":"default"}`, false},
	} {
		env := newAdminEnv(t)
		rec := doAdmin(env, http.MethodPost, "/api/keys", "alice", tc.body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", tc.body, rec.Code, rec.Body.String())
		}
		created := decodeSearchKey(t, rec.Body.Bytes())
		if created.WebSearch == nil || *created.WebSearch != tc.want {
			t.Errorf("create %s: web_search = %v, want %v", tc.body, created.WebSearch, tc.want)
		}
		stored, _, err := env.store.KeyByID(t.Context(), created.ID)
		if err != nil || stored.WebSearch != tc.want {
			t.Errorf("stored web_search = %v, %v; want %v", stored.WebSearch, err, tc.want)
		}
	}
}

func TestAdminCreateKeyRejectsANonBooleanWebSearch(t *testing.T) {
	env := newAdminEnv(t)
	rec := doAdmin(env, http.MethodPost, "/api/keys", "alice", `{"label":"x","web_search":"yes"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
}

func TestAdminSetKeyWebSearch(t *testing.T) {
	env := newAdminEnv(t)
	created, _ := createKey(t, env, `{"label":"chat"}`)
	path := "/api/keys/" + created.ID + "/web-search"

	rec := doAdmin(env, http.MethodPost, path, "bob", `{"web_search":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body.String())
	}
	if got := decodeSearchKey(t, rec.Body.Bytes()); got.WebSearch == nil || !*got.WebSearch {
		t.Errorf("enable answered %+v", got)
	}
	list := doAdmin(env, http.MethodGet, "/api/keys", "alice", "")
	if !strings.Contains(list.Body.String(), `"web_search":true`) {
		t.Errorf("the key list does not show the switch: %s", list.Body.String())
	}

	rec = doAdmin(env, http.MethodPost, path, "bob", `{"web_search":false}`)
	if got := decodeSearchKey(t, rec.Body.Bytes()); rec.Code != http.StatusOK || got.WebSearch == nil || *got.WebSearch {
		t.Errorf("disable: %d %+v", rec.Code, got)
	}
	if stored, _, _ := env.store.KeyByID(t.Context(), created.ID); stored.WebSearch {
		t.Error("the store still has web search on")
	}

	for _, tc := range []struct {
		name, path, body string
		want             int
	}{
		{"unknown key", "/api/keys/nosuchid/web-search", `{"web_search":true}`, http.StatusNotFound},
		{"missing field", path, `{}`, http.StatusBadRequest},
		{"not a boolean", path, `{"web_search":1}`, http.StatusBadRequest},
		{"not JSON", path, `on`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := doAdmin(env, http.MethodPost, tc.path, "alice", tc.body); rec.Code != tc.want {
				t.Errorf("status %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestAdminSetKeyWebSearchRefusesCrossSiteAndAnonymous(t *testing.T) {
	env := newAdminEnv(t)
	created, _ := createKey(t, env, `{"label":"chat"}`)
	path := "/api/keys/" + created.ID + "/web-search"
	if rec := doAdmin(env, http.MethodPost, path, "", `{"web_search":true}`); rec.Code != http.StatusForbidden {
		t.Errorf("anonymous: status %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"web_search":true}`))
	req.Header.Set(ProxySecretHeader, testProxySecret)
	req.Header.Set(identityHeader, "alice")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site: status %d", rec.Code)
	}
	if stored, _, _ := env.store.KeyByID(t.Context(), created.ID); stored.WebSearch {
		t.Error("a refused request changed the key")
	}
}
