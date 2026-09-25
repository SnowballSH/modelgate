package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

const (
	RequestIDHeader    = "X-Request-ID"
	maxRequestIDLength = 128
)

type requestIDKey struct{}

// validRequestID accepts the characters request IDs use in practice and
// nothing a log line or a header could be split on.
func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLength {
		return false
	}
	for _, c := range []byte(id) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == ':':
		default:
			return false
		}
	}
	return true
}

func newRequestID() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return "req_" + hex.EncodeToString(buf)
}

// withRequestID adopts the caller's X-Request-ID when it is valid, or mints
// one, and echoes it on the response.
func withRequestID(w http.ResponseWriter, r *http.Request) *http.Request {
	id := r.Header.Get(RequestIDHeader)
	if !validRequestID(id) {
		id = newRequestID()
	}
	w.Header().Set(RequestIDHeader, id)
	return r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id))
}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}
