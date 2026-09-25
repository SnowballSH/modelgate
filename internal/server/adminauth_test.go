package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdminBoundaryRefusesUniformly(t *testing.T) {
	logs := captureLogs(t)
	env := newAdminEnv(t)
	wrongSecret := strings.Repeat("wrong-", 6)

	cases := map[string]func(h http.Header){
		"no secret":     func(h http.Header) { h.Set(identityHeader, "alice") },
		"wrong secret":  func(h http.Header) { h.Set(ProxySecretHeader, wrongSecret); h.Set(identityHeader, "alice") },
		"secret prefix": func(h http.Header) { h.Set(ProxySecretHeader, testProxySecret[:31]); h.Set(identityHeader, "alice") },
		"secret sent twice": func(h http.Header) {
			h.Add(ProxySecretHeader, testProxySecret)
			h.Add(ProxySecretHeader, testProxySecret)
			h.Set(identityHeader, "alice")
		},
		"no identity":       func(h http.Header) { h.Set(ProxySecretHeader, testProxySecret) },
		"empty identity":    func(h http.Header) { h.Set(ProxySecretHeader, testProxySecret); h.Set(identityHeader, "") },
		"unlisted identity": func(h http.Header) { h.Set(ProxySecretHeader, testProxySecret); h.Set(identityHeader, "mallory") },
		"identity case":     func(h http.Header) { h.Set(ProxySecretHeader, testProxySecret); h.Set(identityHeader, "Alice") },
		"identity sent twice": func(h http.Header) {
			h.Set(ProxySecretHeader, testProxySecret)
			h.Add(identityHeader, "mallory")
			h.Add(identityHeader, "alice")
		},
	}
	var firstBody string
	for name, setHeaders := range cases {
		t.Run(name, func(t *testing.T) {
			for _, path := range []string{"/api/keys", "/api/usage", "/"} {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				setHeaders(req.Header)
				rec := httptest.NewRecorder()
				env.handler.ServeHTTP(rec, req)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("GET %s: status %d, want 403", path, rec.Code)
				}
				if firstBody == "" {
					firstBody = rec.Body.String()
				}
				if rec.Body.String() != firstBody {
					t.Fatalf("GET %s: body %q differs from %q; refusals must not say which check failed", path, rec.Body.String(), firstBody)
				}
			}
		})
	}

	if rec := doAdmin(env, http.MethodGet, "/api/keys", "bob", ""); rec.Code != http.StatusOK {
		t.Fatalf("allowlisted identity with the secret: status %d", rec.Code)
	}
	for _, leaked := range []string{testProxySecret, wrongSecret, testProxySecret[:31]} {
		if strings.Contains(logs.String(), leaked) {
			t.Fatalf("the log carries a presented or configured secret: %s", logs.String())
		}
	}
}

func TestNewAdminBoundaryValidates(t *testing.T) {
	cases := map[string]struct {
		header  string
		allowed []string
		secret  string
	}{
		"empty header":    {"", []string{"alice"}, testProxySecret},
		"empty allowlist": {identityHeader, nil, testProxySecret},
		"short secret":    {identityHeader, []string{"alice"}, strings.Repeat("s", MinProxySecretBytes-1)},
	}
	for name, tc := range cases {
		if _, err := NewAdminBoundary(tc.header, tc.allowed, []byte(tc.secret)); err == nil {
			t.Errorf("%s: NewAdminBoundary accepted it", name)
		}
	}
}

func TestLoadProxySecret(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for name, content := range map[string]string{
		"bare":          testProxySecret,
		"newline":       testProxySecret + "\n",
		"crlf":          testProxySecret + "\r\n",
		"blank lines":   testProxySecret + "\n\n",
		"exactly 32 in": strings.Repeat("s", MinProxySecretBytes) + "\n",
	} {
		secret, err := LoadProxySecret(write(name, content))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if want := strings.TrimRight(content, "\r\n"); string(secret) != want {
			t.Errorf("%s: secret has %d bytes, want %d", name, len(secret), len(want))
		}
	}

	short := strings.Repeat("s", MinProxySecretBytes-1)
	_, err := LoadProxySecret(write("short", short+"\n"))
	if err == nil {
		t.Fatal("LoadProxySecret accepted a 31-byte secret")
	}
	if strings.Contains(err.Error(), short) {
		t.Fatalf("the error %q carries the secret", err)
	}
	if _, err := LoadProxySecret(filepath.Join(dir, "absent")); err == nil {
		t.Fatal("LoadProxySecret accepted a missing file")
	}
}
