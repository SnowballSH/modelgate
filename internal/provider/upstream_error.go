package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	maxErrorBody    = 64 * 1024
	maxErrorMessage = 512
)

// ErrContextLengthExceeded is a request the upstream refused because the
// prompt does not fit the model's context window. It is also an
// ErrInvalidRequest, so a caller that does not tell the two apart still
// answers 400.
var ErrContextLengthExceeded = fmt.Errorf("%w: context window exceeded", ErrInvalidRequest)

// upstreamError is what a non-200 body says went wrong, already truncated and
// credential-redacted. It feeds the log and the error classification only: the
// returned error text stays free of upstream bodies so a provider can never
// dictate what modelgate reports to its own callers.
type upstreamError struct {
	Type      string
	Code      string
	Message   string
	RequestID string
}

func readUpstreamError(body io.Reader) upstreamError {
	raw, err := io.ReadAll(io.LimitReader(body, maxErrorBody))
	if err != nil {
		return upstreamError{}
	}
	var parsed struct {
		Error struct {
			Type    string          `json:"type"`
			Code    json.RawMessage `json:"code"`
			Message string          `json:"message"`
		} `json:"error"`
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal(raw, &parsed) == nil && parsed.Error.Message != "" {
		var code string
		_ = json.Unmarshal(parsed.Error.Code, &code)
		return upstreamError{
			Type:      sanitize(parsed.Error.Type),
			Code:      sanitize(code),
			Message:   sanitize(parsed.Error.Message),
			RequestID: sanitize(parsed.RequestID),
		}
	}
	return upstreamError{Message: sanitize(strings.TrimSpace(string(raw)))}
}

var contextWindowMessage = regexp.MustCompile(`(?i)prompt is too long|exceeds? (?:the )?context (?:window|limit)|maximum context length`)

func (e upstreamError) contextWindowExceeded() bool {
	return e.Code == "context_length_exceeded" || contextWindowMessage.MatchString(e.Message)
}

// classify maps a non-200 status to the error the gateway acts on, singling
// out the one 400 a caller can fix by shortening the conversation.
func (e upstreamError) classify(status int) error {
	if status == http.StatusBadRequest && e.contextWindowExceeded() {
		return fmt.Errorf("%w: status %d", ErrContextLengthExceeded, status)
	}
	return mapStatus(status)
}

// logsUpstreamMessage reports whether a status carries detail worth logging.
// A credential rejection does not: OpenAI's 401 body quotes part of the key it
// refused, and the status code already says everything it diagnoses.
func logsUpstreamMessage(status int) bool {
	return status != http.StatusUnauthorized && status != http.StatusForbidden
}

func sanitize(s string) string {
	return truncate(redactCredentials(s))
}

var credentialShaped = regexp.MustCompile(`(?i)(?:\bbearer\s+\S+|\bsk-[A-Za-z0-9._-]+)`)

func redactCredentials(s string) string {
	return credentialShaped.ReplaceAllString(s, "[redacted]")
}

func truncate(s string) string {
	if len(s) <= maxErrorMessage {
		return s
	}
	cut := maxErrorMessage
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
