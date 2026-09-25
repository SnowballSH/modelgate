package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SnowballSH/modelgate/internal/oai"
)

func TestStatusForCode(t *testing.T) {
	cases := map[string]int{
		CodeInvalidAPIKey:       http.StatusUnauthorized,
		CodeModelNotFound:       http.StatusNotFound,
		CodeRateLimited:         http.StatusTooManyRequests,
		CodeQuotaExhausted:      http.StatusTooManyRequests,
		CodeBudgetExhausted:     http.StatusTooManyRequests,
		CodeRequestTimeout:      http.StatusRequestTimeout,
		CodeRequestTooLarge:     http.StatusRequestEntityTooLarge,
		CodeInvalidRequest:      http.StatusBadRequest,
		CodeProviderAuthError:   http.StatusBadGateway,
		CodeProviderUnavailable: http.StatusServiceUnavailable,
		CodeTimeout:             http.StatusGatewayTimeout,
		CodeInternal:            http.StatusInternalServerError,
		"never_heard_of_it":     http.StatusInternalServerError,
	}
	for code, want := range cases {
		if got := statusForCode(code); got != want {
			t.Errorf("statusForCode(%q) = %d, want %d", code, got, want)
		}
	}
}

func TestWriteError(t *testing.T) {
	cases := []struct {
		code       string
		wantStatus int
		wantType   string
		wantCode   string
	}{
		{CodeInvalidAPIKey, 401, "authentication_error", CodeInvalidAPIKey},
		{CodeModelNotFound, 404, "invalid_request_error", CodeModelNotFound},
		{CodeRateLimited, 429, "rate_limit_error", CodeRateLimited},
		{CodeQuotaExhausted, 429, "insufficient_quota", "insufficient_quota"},
		{CodeBudgetExhausted, 429, "insufficient_quota", "insufficient_quota"},
		{CodeRequestTimeout, 408, "invalid_request_error", CodeRequestTimeout},
		{CodeRequestTooLarge, 413, "invalid_request_error", CodeRequestTooLarge},
		{CodeInvalidRequest, 400, "invalid_request_error", CodeInvalidRequest},
		{CodeProviderAuthError, 502, "api_error", CodeProviderAuthError},
		{CodeProviderUnavailable, 503, "api_error", CodeProviderUnavailable},
		{CodeTimeout, 504, "api_error", CodeTimeout},
		{CodeInternal, 500, "api_error", CodeInternal},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		writeError(rec, tc.code, "the message")
		if rec.Code != tc.wantStatus {
			t.Errorf("writeError(%q) status = %d, want %d", tc.code, rec.Code, tc.wantStatus)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("writeError(%q) Content-Type = %q, want application/json", tc.code, ct)
		}
		var body oai.ErrorBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("writeError(%q) body: %v", tc.code, err)
		}
		if body.Error.Code != tc.wantCode {
			t.Errorf("writeError(%q) code = %q", tc.code, body.Error.Code)
		}
		if body.Error.Type != tc.wantType {
			t.Errorf("writeError(%q) type = %q, want %q", tc.code, body.Error.Type, tc.wantType)
		}
		if body.Error.Message != "the message" {
			t.Errorf("writeError(%q) message = %q", tc.code, body.Error.Message)
		}
	}
}

func TestWriteErrorUnknownCode(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, "mystery", "boom")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	var body oai.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Type != "api_error" || body.Error.Code != "mystery" {
		t.Errorf("body = %+v", body.Error)
	}
}
