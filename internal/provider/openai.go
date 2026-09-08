package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/SnowballSH/modelgate/internal/oai"
	"github.com/SnowballSH/modelgate/internal/oairesp"
)

const (
	chatCompletionsPath = "/v1/chat/completions"
	responsesPath       = "/v1/responses"
	maxErrorBody        = 64 * 1024
	maxErrorMessage     = 512
)

type OpenAIClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
	sleep   func(time.Duration)
}

func NewOpenAIClient(baseURL, apiKey string, httpClient *http.Client) *OpenAIClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &OpenAIClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    httpClient,
		sleep:   time.Sleep,
	}
}

func (c *OpenAIClient) Chat(ctx context.Context, req oai.ChatRequest) (oai.ChatResponse, error) {
	req.Stream = false
	body, err := json.Marshal(req)
	if err != nil {
		return oai.ChatResponse{}, fmt.Errorf("%w: encode request", ErrUnavailable)
	}
	return postJSON[oai.ChatResponse](ctx, c, chatCompletionsPath, body)
}

func (c *OpenAIClient) Responses(ctx context.Context, req oairesp.Request) (oairesp.Response, error) {
	req.Stream = false
	body, err := json.Marshal(req)
	if err != nil {
		return oairesp.Response{}, fmt.Errorf("%w: encode request", ErrUnavailable)
	}
	return postJSON[oairesp.Response](ctx, c, responsesPath, body)
}

func postJSON[T any](ctx context.Context, c *OpenAIClient, path string, body []byte) (T, error) {
	var zero T
	var lastErr error
	for attempt := range maxAttempts {
		if attempt > 0 {
			c.sleep(backoff(attempt))
		}
		res, status, err := c.do(ctx, path, body)
		if err != nil {
			lastErr = err
			if retryable(err, status) {
				continue
			}
			return zero, err
		}
		var out T
		decodeErr := json.NewDecoder(res.Body).Decode(&out)
		res.Body.Close()
		if decodeErr != nil {
			return zero, fmt.Errorf("%w: decode response", ErrUnavailable)
		}
		return out, nil
	}
	return zero, lastErr
}

func (c *OpenAIClient) ChatStream(ctx context.Context, req oai.ChatRequest, each func(oai.ChatChunk) error) error {
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("%w: encode request", ErrUnavailable)
	}
	return c.stream(func() (bool, int, error) {
		return c.chatStreamOnce(ctx, body, each)
	})
}

func (c *OpenAIClient) ResponsesStream(ctx context.Context, req oairesp.Request, each func(oairesp.StreamEvent) error) error {
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("%w: encode request", ErrUnavailable)
	}
	return c.stream(func() (bool, int, error) {
		return c.responsesStreamOnce(ctx, body, each)
	})
}

func (c *OpenAIClient) stream(once func() (bool, int, error)) error {
	var lastErr error
	for attempt := range maxAttempts {
		if attempt > 0 {
			c.sleep(backoff(attempt))
		}
		delivered, status, err := once()
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

func (c *OpenAIClient) chatStreamOnce(ctx context.Context, body []byte, each func(oai.ChatChunk) error) (delivered bool, status int, err error) {
	res, status, err := c.do(ctx, chatCompletionsPath, body)
	if err != nil {
		return false, status, err
	}
	defer res.Body.Close()

	scanner := newSSEScanner(res.Body)
	for scanner.Scan() {
		payload, ok := dataPayload(scanner.Text())
		if !ok || payload == "" {
			continue
		}
		if payload == "[DONE]" {
			return delivered, status, nil
		}
		var chunk oai.ChatChunk
		if unmarshalErr := json.Unmarshal([]byte(payload), &chunk); unmarshalErr != nil {
			return delivered, status, fmt.Errorf("%w: malformed stream chunk", ErrUnavailable)
		}
		delivered = true
		if eachErr := each(chunk); eachErr != nil {
			return delivered, status, eachErr
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return delivered, status, mapTransportError(ctx, scanErr)
	}
	return delivered, status, fmt.Errorf("%w: stream ended before [DONE]", ErrUnavailable)
}

func (c *OpenAIClient) responsesStreamOnce(ctx context.Context, body []byte, each func(oairesp.StreamEvent) error) (delivered bool, status int, err error) {
	res, status, err := c.do(ctx, responsesPath, body)
	if err != nil {
		return false, status, err
	}
	defer res.Body.Close()

	scanner := newSSEScanner(res.Body)
	var name string
	for scanner.Scan() {
		line := scanner.Text()
		if event, ok := eventName(line); ok {
			name = event
			continue
		}
		payload, ok := dataPayload(line)
		if !ok || payload == "" {
			continue
		}
		var ev oairesp.StreamEvent
		if unmarshalErr := json.Unmarshal([]byte(payload), &ev); unmarshalErr != nil {
			return delivered, status, fmt.Errorf("%w: malformed stream event", ErrUnavailable)
		}
		if ev.Type == "" {
			ev.Type = name
		}
		name = ""
		delivered = true
		if eachErr := each(ev); eachErr != nil {
			return delivered, status, eachErr
		}
		if responsesTerminal(ev.Type) {
			return delivered, status, nil
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return delivered, status, mapTransportError(ctx, scanErr)
	}
	return delivered, status, fmt.Errorf("%w: stream ended before a terminal response event", ErrUnavailable)
}

func newSSEScanner(body io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return scanner
}

func eventName(line string) (string, bool) {
	rest, ok := strings.CutPrefix(line, "event:")
	if !ok {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

func responsesTerminal(eventType string) bool {
	switch eventType {
	case "response.completed", "response.incomplete", "response.failed":
		return true
	default:
		return false
	}
}

func (c *OpenAIClient) do(ctx context.Context, path string, body []byte) (*http.Response, int, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("%w: build request", ErrUnavailable)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	res, err := c.http.Do(httpReq)
	if err != nil {
		return nil, 0, mapTransportError(ctx, err)
	}
	if res.StatusCode != http.StatusOK {
		message := upstreamErrorMessage(res.Body)
		res.Body.Close()
		if message != "" {
			slog.Warn("openai upstream rejected the request", "path", path, "status", res.StatusCode, "message", message)
		}
		return nil, res.StatusCode, mapStatus(res.StatusCode)
	}
	return res, res.StatusCode, nil
}

// upstreamErrorMessage returns what the upstream said went wrong, for the log
// only: the returned error text stays free of upstream bodies so a provider
// can never dictate what modelgate reports to its own callers.
func upstreamErrorMessage(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, maxErrorBody))
	if err != nil {
		return ""
	}
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &parsed) == nil && parsed.Error.Message != "" {
		return truncate(parsed.Error.Message)
	}
	return truncate(strings.TrimSpace(string(raw)))
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
