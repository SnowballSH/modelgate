package server

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/SnowballSH/modelgate/internal/oai"
)

// Codes name every outcome the public surface reports. Most go on the wire
// as the error code; the two cap codes stay distinct in metrics and logs but
// answer with OpenAI's insufficient_quota shape.
const (
	CodeInvalidAPIKey       = "invalid_api_key"       // 401
	CodeModelNotFound       = "model_not_found"       // 404
	CodeRateLimited         = "rate_limited"          // 429
	CodeQuotaExhausted      = "quota_exhausted"       // 429 insufficient_quota
	CodeBudgetExhausted     = "budget_exhausted"      // 429 insufficient_quota
	CodeRequestTimeout      = "request_timeout"       // 408
	CodeRequestTooLarge     = "request_too_large"     // 413
	CodeInvalidRequest      = "invalid_request_error" // 400
	CodeProviderAuthError   = "provider_auth_error"   // 502
	CodeProviderUnavailable = "provider_unavailable"  // 503
	CodeTimeout             = "timeout"               // 504
	CodeInternal            = "api_error"             // 500, internal store/accounting failures
)

const insufficientQuota = "insufficient_quota"

var statusByCode = map[string]int{
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
}

func statusForCode(code string) int {
	if status, ok := statusByCode[code]; ok {
		return status
	}
	return http.StatusInternalServerError
}

func isCapCode(code string) bool {
	return code == CodeQuotaExhausted || code == CodeBudgetExhausted
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

// wireError is the OpenAI error body for an outcome code.
func wireError(code, message string) oai.ErrorBody {
	if isCapCode(code) {
		return errorBody(insufficientQuota, insufficientQuota, message)
	}
	return errorBody(errorTypeForStatus(statusForCode(code)), code, message)
}

func errorBody(errType, code, message string) oai.ErrorBody {
	return oai.ErrorBody{Error: oai.ErrorDetail{
		Message: message,
		Type:    errType,
		Code:    code,
	}}
}

// writeError answers with the outcome's status and body. A cap refusal also
// carries x-should-retry: false, which the OpenAI SDKs honour instead of
// retrying a 429 that only the next month or an operator can clear.
func writeError(w http.ResponseWriter, code, message string) {
	if isCapCode(code) {
		w.Header().Set("x-should-retry", "false")
	}
	writeJSONStatus(w, statusForCode(code), wireError(code, message))
}

func writeRefusal(w http.ResponseWriter, refusal Refusal, message string) {
	if refusal.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(refusal.RetryAfter)))
	}
	writeError(w, refusal.Code, message)
}

func retryAfterSeconds(d time.Duration) int {
	return max(1, int(math.Ceil(d.Seconds())))
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErrorStatus(w http.ResponseWriter, status int, errType, code, message string) {
	writeJSONStatus(w, status, errorBody(errType, code, message))
}

func writeNotFound(w http.ResponseWriter, message string) {
	writeErrorStatus(w, http.StatusNotFound, "invalid_request_error", "not_found", message)
}
