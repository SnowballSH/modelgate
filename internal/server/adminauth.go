package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
)

const (
	ProxySecretHeader   = "X-Modelgate-Proxy-Secret"
	MinProxySecretBytes = 32
)

// AdminBoundary authenticates the admin listener at the application
// boundary (D037): every request must carry the proxy's shared secret and
// an identity on the allowlist. A reachable port alone proves nothing, since
// other host-loopback containers can reach it too.
type AdminBoundary struct {
	identityHeader string
	allowed        map[string]struct{}
	secretDigest   [sha256.Size]byte
}

func NewAdminBoundary(identityHeader string, allowed []string, secret []byte) (*AdminBoundary, error) {
	if identityHeader == "" {
		return nil, errors.New("admin boundary: identity header is empty")
	}
	if len(allowed) == 0 {
		return nil, errors.New("admin boundary: the identity allowlist is empty")
	}
	if len(secret) < MinProxySecretBytes {
		return nil, fmt.Errorf("admin boundary: the proxy secret is %d bytes, want at least %d", len(secret), MinProxySecretBytes)
	}
	b := &AdminBoundary{
		identityHeader: identityHeader,
		allowed:        make(map[string]struct{}, len(allowed)),
		secretDigest:   sha256.Sum256(secret),
	}
	for _, identity := range allowed {
		b.allowed[identity] = struct{}{}
	}
	return b, nil
}

// LoadProxySecret reads the shared secret file, trimming only the trailing
// line ending a file editor or a secret renderer adds.
func LoadProxySecret(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	secret := []byte(strings.TrimRight(string(raw), "\r\n"))
	if len(secret) < MinProxySecretBytes {
		return nil, fmt.Errorf("%s holds a %d-byte secret, want at least %d", path, len(secret), MinProxySecretBytes)
	}
	return secret, nil
}

// Authorize returns the caller's identity, or the reason it refused. Both
// headers must appear exactly once: a duplicated header means some hop
// appended instead of replacing, and neither value can be trusted. Digests
// give the secret comparison a fixed length, so its timing reveals nothing.
func (b *AdminBoundary) Authorize(r *http.Request) (identity, refusal string) {
	secrets := r.Header.Values(ProxySecretHeader)
	if len(secrets) != 1 {
		return "", "proxy_secret"
	}
	presented := sha256.Sum256([]byte(secrets[0]))
	if subtle.ConstantTimeCompare(presented[:], b.secretDigest[:]) != 1 {
		return "", "proxy_secret"
	}
	identities := r.Header.Values(b.identityHeader)
	if len(identities) != 1 {
		return "", "identity"
	}
	if _, ok := b.allowed[identities[0]]; !ok {
		return "", "identity"
	}
	return identities[0], ""
}
