// Package provider holds the upstream LLM API clients and their shared
// circuit breaker.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/SnowballSH/modelgate/internal/anthro"
)

var (
	ErrAuth           = errors.New("provider authentication failed")
	ErrRateLimited    = errors.New("provider rate limited")
	ErrUnavailable    = errors.New("provider unavailable")
	ErrTimeout        = errors.New("provider timeout")
	ErrInvalidRequest = errors.New("provider rejected the request")
	ErrClientAborted  = errors.New("caller aborted the request")
)

const (
	anthropicVersion = "2023-06-01"
	maxAttempts      = 3
	baseBackoff      = 100 * time.Millisecond
)

type Client struct {
	baseURL     string
	apiKey      string
	http        *http.Client
	sleep       func(time.Duration)
	idleTimeout time.Duration
}

func NewClient(baseURL, apiKey string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		baseURL:     strings.TrimRight(baseURL, "/"),
		apiKey:      apiKey,
		http:        httpClient,
		sleep:       time.Sleep,
		idleTimeout: DefaultStreamIdleTimeout,
	}
}

// SetStreamIdleTimeout replaces DefaultStreamIdleTimeout for this client's
// streamed calls; zero or less turns the idle abort off.
func (c *Client) SetStreamIdleTimeout(timeout time.Duration) {
	c.idleTimeout = timeout
}

func (c *Client) Messages(ctx context.Context, req anthro.MessagesRequest) (anthro.MessagesResponse, error) {
	req.Stream = false
	body, err := json.Marshal(req)
	if err != nil {
		return anthro.MessagesResponse{}, fmt.Errorf("%w: encode request", ErrUnavailable)
	}

	var lastErr error
	for attempt := range maxAttempts {
		if attempt > 0 {
			c.sleep(backoff(attempt))
		}
		res, status, err := c.do(ctx, body)
		if err != nil {
			lastErr = err
			if retryable(err, status) {
				continue
			}
			return anthro.MessagesResponse{}, err
		}
		var out anthro.MessagesResponse
		decodeErr := json.NewDecoder(res.Body).Decode(&out)
		res.Body.Close()
		if decodeErr != nil {
			return anthro.MessagesResponse{}, fmt.Errorf("%w: decode response", ErrUnavailable)
		}
		return out, nil
	}
	return anthro.MessagesResponse{}, lastErr
}

func (c *Client) MessagesStream(ctx context.Context, req anthro.MessagesRequest, each func(anthro.StreamEvent) error) error {
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("%w: encode request", ErrUnavailable)
	}

	var lastErr error
	for attempt := range maxAttempts {
		if attempt > 0 {
			c.sleep(backoff(attempt))
		}
		delivered, status, err := c.streamOnce(ctx, body, each)
		if err == nil {
			return nil
		}
		lastErr = err
		if delivered || !retryable(err, status) {
			return err
		}
	}
	return lastErr
}

func (c *Client) streamOnce(ctx context.Context, body []byte, each func(anthro.StreamEvent) error) (delivered bool, status int, err error) {
	ctx, guard := guardIdle(ctx, c.idleTimeout)
	defer guard.stop()
	res, status, err := c.do(ctx, body)
	if err != nil {
		return false, status, err
	}
	defer res.Body.Close()

	scanner := newSSEScanner(guard.reader(res.Body))
	for scanner.Scan() {
		payload, ok := dataPayload(scanner.Text())
		if !ok || payload == "" {
			continue
		}
		var ev anthro.StreamEvent
		if unmarshalErr := json.Unmarshal([]byte(payload), &ev); unmarshalErr != nil {
			return delivered, status, fmt.Errorf("%w: malformed stream event", ErrUnavailable)
		}
		delivered = true
		if ev.Type == "error" && ev.Error != nil {
			logStreamError(ev.Error)
		}
		if eachErr := each(ev); eachErr != nil {
			return delivered, status, eachErr
		}
		if ev.Type == "message_stop" {
			return delivered, status, nil
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return delivered, status, mapTransportError(ctx, scanErr)
	}
	return delivered, status, fmt.Errorf("%w: stream ended before message_stop", ErrUnavailable)
}

func dataPayload(line string) (string, bool) {
	rest, ok := strings.CutPrefix(line, "data:")
	if !ok {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

func (c *Client) do(ctx context.Context, body []byte) (*http.Response, int, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("%w: build request", ErrUnavailable)
	}
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	httpReq.Header.Set("content-type", "application/json")

	res, err := c.http.Do(httpReq)
	if err != nil {
		return nil, 0, mapTransportError(ctx, err)
	}
	if res.StatusCode != http.StatusOK {
		upstream := readUpstreamError(res.Body)
		res.Body.Close()
		logRejection(res.StatusCode, upstream)
		return nil, res.StatusCode, upstream.classify(res.StatusCode)
	}
	return res, res.StatusCode, nil
}

// logRejection records why Anthropic refused a call. The error type and
// request id are always safe to log; the message is left out of credential
// rejections, which have nothing to say that the status does not.
func logRejection(status int, upstream upstreamError) {
	attrs := []any{"status", status, "error_type", upstream.Type, "request_id", upstream.RequestID}
	if logsUpstreamMessage(status) {
		attrs = append(attrs, "message", upstream.Message)
	}
	slog.Warn("anthropic upstream rejected the request", attrs...)
}

func logStreamError(apiErr *anthro.APIError) {
	slog.Warn("anthropic upstream stream carried an error event",
		"error_type", sanitize(apiErr.Type), "message", sanitize(apiErr.Message))
}

func mapStatus(status int) error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("%w: status %d", ErrAuth, status)
	case status == http.StatusRequestTimeout:
		return fmt.Errorf("%w: status %d", ErrTimeout, status)
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("%w: status %d", ErrRateLimited, status)
	case status >= 400 && status < 500:
		return fmt.Errorf("%w: status %d", ErrInvalidRequest, status)
	default:
		return fmt.Errorf("%w: status %d", ErrUnavailable, status)
	}
}

func mapTransportError(ctx context.Context, err error) error {
	if errors.Is(context.Cause(ctx), errStreamIdle) {
		return fmt.Errorf("%w: %w", ErrTimeout, errStreamIdle)
	}
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return fmt.Errorf("%w: connection closed", ErrClientAborted)
	}
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return fmt.Errorf("%w: request aborted", ErrTimeout)
	}
	return fmt.Errorf("%w: transport failure", ErrUnavailable)
}

func retryable(err error, status int) bool {
	if !errors.Is(err, ErrRateLimited) && !errors.Is(err, ErrUnavailable) {
		return false
	}
	return status == 0 || status == http.StatusTooManyRequests || status >= 500
}

func backoff(attempt int) time.Duration {
	return baseBackoff << (attempt - 1)
}
