package keys

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	g, err := Generate(rand.Reader)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	id, secret, ok := ParseBearer("Bearer " + g.Full)
	if !ok {
		t.Fatalf("ParseBearer rejected %q", g.Full)
	}
	if id != g.ID {
		t.Errorf("id = %q, want %q", id, g.ID)
	}
	if !Verify(secret, g.SecretSHA256) {
		t.Error("Verify returned false for correct secret")
	}
	if g.Prefix != "mg_"+g.ID {
		t.Errorf("Prefix = %q, want %q", g.Prefix, "mg_"+g.ID)
	}
}

func TestVerifyWrongSecret(t *testing.T) {
	g, err := Generate(rand.Reader)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if Verify("not-the-secret", g.SecretSHA256) {
		t.Error("Verify accepted wrong secret")
	}
	if Verify("", g.SecretSHA256) {
		t.Error("Verify accepted empty secret")
	}
}

func TestParseBearerMalformed(t *testing.T) {
	cases := []struct {
		name   string
		header string
	}{
		{"empty", ""},
		{"no bearer prefix", "mg_abcdefgh_secret"},
		{"lowercase scheme", "bearer mg_abcdefgh_secret"},
		{"double space", "Bearer  mg_abcdefgh_secret"},
		{"missing mg_", "Bearer abcdefgh_secret"},
		{"id 7 chars", "Bearer mg_abcdefg_secret"},
		{"id uppercase", "Bearer mg_ABCDEFGH_secret"},
		{"id digit 1", "Bearer mg_abcdefg1_secret"},
		{"id digit 0", "Bearer mg_abcdefg0_secret"},
		{"id digit 8", "Bearer mg_abcdefg8_secret"},
		{"id digit 9", "Bearer mg_abcdefg9_secret"},
		{"no underscore after id", "Bearer mg_abcdefghsecret"},
		{"empty secret", "Bearer mg_abcdefgh_"},
		{"key too short", "Bearer mg_abc"},
		{"bearer only", "Bearer "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := ParseBearer(tc.header); ok {
				t.Errorf("ParseBearer(%q) accepted", tc.header)
			}
		})
	}
}

func TestGenerateProperties(t *testing.T) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	for range 200 {
		g, err := Generate(rand.Reader)
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if len(g.ID) != 8 {
			t.Fatalf("ID length = %d, want 8", len(g.ID))
		}
		for _, c := range g.ID {
			if !strings.ContainsRune(alphabet, c) {
				t.Fatalf("ID %q contains %q outside alphabet", g.ID, c)
			}
		}
		rest, found := strings.CutPrefix(g.Full, "mg_"+g.ID+"_")
		if !found || rest == "" {
			t.Fatalf("Full %q does not have shape mg_%s_<secret>", g.Full, g.ID)
		}
		if sha256.Sum256([]byte(rest)) != g.SecretSHA256 {
			t.Fatalf("SecretSHA256 does not match secret in Full %q", g.Full)
		}
	}
}

func TestGenerateShortReader(t *testing.T) {
	for _, n := range []int64{0, 4, 5, 20, 36} {
		if _, err := Generate(io.LimitReader(rand.Reader, n)); err == nil {
			t.Errorf("Generate with %d-byte reader succeeded", n)
		}
	}
}

func TestSecretWithURLSafeChars(t *testing.T) {
	g, err := Generate(bytes.NewReader(bytes.Repeat([]byte{0xff}, 37)))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(g.Full, "_") || !strings.ContainsAny(g.Full[len(g.Prefix)+1:], "_-") {
		t.Fatalf("expected secret with '_' or '-', got %q", g.Full)
	}
	id, secret, ok := ParseBearer("Bearer " + g.Full)
	if !ok {
		t.Fatalf("ParseBearer rejected %q", g.Full)
	}
	if id != g.ID {
		t.Errorf("id = %q, want %q", id, g.ID)
	}
	if !Verify(secret, g.SecretSHA256) {
		t.Error("Verify returned false")
	}
}
