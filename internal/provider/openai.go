package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/SnowballSH/modelgate/internal/oai"
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

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			c.sleep(backoff(attempt))
		}
		res, status, err := c.do(ctx, body)
		if err != nil {
			lastErr = err
			if retryable(err, status) {
				continue
			}
			return oai.ChatResponse{}, err
		}
		var out oai.ChatResponse
		decodeErr := json.NewDecoder(res.Body).Decode(&out)
		res.Body.Close()
		if decodeErr != nil {
			return oai.ChatResponse{}, fmt.Errorf("%w: decode response", ErrUnavailable)
		}
		return out, nil
	}
	return oai.ChatResponse{}, lastErr
}

func (c *OpenAIClient) ChatStream(ctx context.Context, req oai.ChatRequest, each func(oai.ChatChunk) error) error {
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("%w: encode request", ErrUnavailable)
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
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

func (c *OpenAIClient) streamOnce(ctx context.Context, body []byte, each func(oai.ChatChunk) error) (delivered bool, status int, err error) {
	res, status, err := c.do(ctx, body)
	if err != nil {
		return false, status, err
	}
	defer res.Body.Close()

	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
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
		return delivered, status, mapTransportError(scanErr, ctx)
	}
	return delivered, status, fmt.Errorf("%w: stream ended before [DONE]", ErrUnavailable)
}

func (c *OpenAIClient) do(ctx context.Context, body []byte) (*http.Response, int, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("%w: build request", ErrUnavailable)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	res, err := c.http.Do(httpReq)
	if err != nil {
		return nil, 0, mapTransportError(err, ctx)
	}
	if res.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64*1024))
		res.Body.Close()
		return nil, res.StatusCode, mapStatus(res.StatusCode)
	}
	return res, res.StatusCode, nil
}
