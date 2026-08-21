package keys

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"io"
	"strings"
)

const (
	idAlphabet   = "abcdefghijklmnopqrstuvwxyz234567"
	idRandBytes  = 5
	idLen        = 8
	secretBytes  = 32
	keyPrefix    = "mg_"
	bearerScheme = "Bearer "
)

var idEncoding = base32.NewEncoding(idAlphabet).WithPadding(base32.NoPadding)

type Generated struct {
	Full         string
	ID           string
	Prefix       string
	SecretSHA256 [32]byte
}

func Generate(rand io.Reader) (Generated, error) {
	buf := make([]byte, idRandBytes+secretBytes)
	if _, err := io.ReadFull(rand, buf); err != nil {
		return Generated{}, err
	}
	id := idEncoding.EncodeToString(buf[:idRandBytes])
	secret := base64.RawURLEncoding.EncodeToString(buf[idRandBytes:])
	prefix := keyPrefix + id
	return Generated{
		Full:         prefix + "_" + secret,
		ID:           id,
		Prefix:       prefix,
		SecretSHA256: sha256.Sum256([]byte(secret)),
	}, nil
}

func ParseBearer(header string) (id, secret string, ok bool) {
	key, found := strings.CutPrefix(header, bearerScheme)
	if !found {
		return "", "", false
	}
	rest, found := strings.CutPrefix(key, keyPrefix)
	if !found || len(rest) < idLen+2 || rest[idLen] != '_' {
		return "", "", false
	}
	id, secret = rest[:idLen], rest[idLen+1:]
	for _, c := range []byte(id) {
		if !strings.ContainsRune(idAlphabet, rune(c)) {
			return "", "", false
		}
	}
	return id, secret, true
}

func Verify(secret string, digest [32]byte) bool {
	sum := sha256.Sum256([]byte(secret))
	return subtle.ConstantTimeCompare(sum[:], digest[:]) == 1
}
