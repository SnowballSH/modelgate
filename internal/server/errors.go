package server

import (
	"encoding/json"
	"net/http"

	"github.com/SnowballSH/modelgate/internal/oai"
)

const (
	CodeInvalidAPIKey       = "invalid_api_key"       // 401
	CodeModelNotFound       = "model_not_found"       // 404
	CodeRateLimited         = "rate_limited"          // 429
	CodeQuotaExhausted      = "quota_exhausted"       // 429
	CodeBudgetExhausted     = "budget_exhausted"      // 429
	CodeRequestTooLarge     = "request_too_large"     // 413
	CodeInvalidRequest      = "invalid_request_error" // 400
	CodeProviderAuthError   = "provider_auth_error"   // 502
	CodeProviderUnavailable = "provider_unavailable"  // 503
	CodeTimeout             = "timeout"               // 504
	CodeInternal            = "api_error"             // 500, internal store/accounting failures
)

var statusByCode = map[string]int{
	CodeInvalidAPIKey:       http.StatusUnauthorized,
	CodeModelNotFound:       http.StatusNotFound,
	CodeRateLimited:         http.StatusTooManyRequests,
	CodeQuotaExhausted:      http.StatusTooManyRequests,
	CodeBudgetExhausted:     http.StatusTooManyRequests,
	CodeRequestTooLarge:     http.StatusRequestEntityTooLarge,
	CodeInvalidRequest:      http.StatusBadRequest,
	CodeProviderAuthError:   http.StatusBadGateway,
	CodeProviderUnavailable: http.StatusServiceUnavailable,
	CodeTimeout:             http.StatusGatewayTimeout,
	CodeInternal:            http.StatusInternalServerError,
}

func StatusForCode(code string) int {
	if status, ok := statusByCode[code]; ok {
		return status
	}
	return http.StatusInternalServerError
}

func errorTypeForStatus(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 400 && status < 500:
		return "invalid_request_error"
	default:
		return "api_error"
	}
}

func WriteError(w http.ResponseWriter, code, message string) {
	status := StatusForCode(code)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(oai.ErrorBody{Error: oai.ErrorDetail{
		Message: message,
		Type:    errorTypeForStatus(status),
		Code:    code,
	}})
}
