package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SnowballSH/modelgate/internal/anthro"
)

func anthropicErrorServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func anthropicErrorBody(errType, message string) string {
	return fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":%q},"request_id":"req_011CTestRequest"}`, errType, message)
}

func TestAnthropicContextWindowErrors(t *testing.T) {
	for _, message := range []string{
		"prompt is too long: 208310 tokens > 200000 maximum",
		"input length and `max_tokens` exceed context limit: 198000 + 8192 > 200000, decrease input length or `max_tokens` and try again",
	} {
		t.Run(message[:20], func(t *testing.T) {
			captureLogs(t)
			srv := anthropicErrorServer(t, http.StatusBadRequest, anthropicErrorBody("invalid_request_error", message))
			_, err := newTestClient(t, srv.URL).Messages(context.Background(), sampleRequest())
			if !errors.Is(err, ErrContextLengthExceeded) {
				t.Fatalf("err = %v, want ErrContextLengthExceeded", err)
			}
			if !errors.Is(err, ErrInvalidRequest) {
				t.Errorf("err = %v, want it to remain an ErrInvalidRequest", err)
			}
			if strings.Contains(err.Error(), "tokens") {
				t.Errorf("error leaks the upstream message: %v", err)
			}
		})
	}
}

func TestAnthropicOtherBadRequestIsNotContextWindow(t *testing.T) {
	captureLogs(t)
	srv := anthropicErrorServer(t, http.StatusBadRequest,
		anthropicErrorBody("invalid_request_error", "messages: roles must alternate between \"user\" and \"assistant\""))
	_, err := newTestClient(t, srv.URL).Messages(context.Background(), sampleRequest())
	if !errors.Is(err, ErrInvalidRequest) || errors.Is(err, ErrContextLengthExceeded) {
		t.Fatalf("err = %v, want a plain ErrInvalidRequest", err)
	}
}

func TestAnthropicContextWindowOnlyOnBadRequest(t *testing.T) {
	captureLogs(t)
	srv := anthropicErrorServer(t, http.StatusRequestEntityTooLarge, anthropicErrorBody("request_too_large", "prompt is too long"))
	_, err := newTestClient(t, srv.URL).Messages(context.Background(), sampleRequest())
	if errors.Is(err, ErrContextLengthExceeded) {
		t.Fatalf("err = %v, a 413 is a body-size refusal, not a context window one", err)
	}
}

func TestAnthropicLogsUpstreamError(t *testing.T) {
	logs := captureLogs(t)
	srv := anthropicErrorServer(t, http.StatusBadRequest,
		anthropicErrorBody("invalid_request_error", "tool_choice: type \"tool\" and \"any\" are not supported for this model."))
	_, err := newTestClient(t, srv.URL).Messages(context.Background(), sampleRequest())
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	record := logs.String()
	for _, want := range []string{"level=WARN", "anthropic upstream rejected the request", "status=400",
		"error_type=invalid_request_error", "request_id=req_011CTestRequest", "are not supported for this model"} {
		if !strings.Contains(record, want) {
			t.Errorf("log lacks %q: %s", want, record)
		}
	}
	if strings.Contains(err.Error(), "not supported") {
		t.Errorf("error leaks the upstream message: %v", err)
	}
}

func TestAnthropicLogsNoPromptText(t *testing.T) {
	logs := captureLogs(t)
	srv := anthropicErrorServer(t, http.StatusBadRequest, anthropicErrorBody("invalid_request_error", "messages.0.content.0.text: invalid"))
	req := sampleRequest()
	req.System = []anthro.TextBlock{{Type: "text", Text: "PROMPT-SYSTEM-TEXT"}}
	req.Messages[0].Content[0].Text = "PROMPT-USER-TEXT"
	_, _ = newTestClient(t, srv.URL).Messages(context.Background(), req)
	for _, forbidden := range []string{"PROMPT-SYSTEM-TEXT", "PROMPT-USER-TEXT", "test-key"} {
		if strings.Contains(logs.String(), forbidden) {
			t.Errorf("log carries %q: %s", forbidden, logs.String())
		}
	}
}

func TestAnthropicDoesNotLogCredentialRejectionMessages(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			logs := captureLogs(t)
			srv := anthropicErrorServer(t, status, anthropicErrorBody("authentication_error", "invalid x-api-key sk-ant-fakeKey123"))
			_, err := newTestClient(t, srv.URL).Messages(context.Background(), sampleRequest())
			if !errors.Is(err, ErrAuth) {
				t.Fatalf("err = %v, want ErrAuth", err)
			}
			record := logs.String()
			if !strings.Contains(record, "error_type=authentication_error") {
				t.Errorf("log lacks the error type: %s", record)
			}
			for _, forbidden := range []string{"sk-ant", "fakeKey123", "x-api-key"} {
				if strings.Contains(record, forbidden) {
					t.Errorf("log carries %q: %s", forbidden, record)
				}
			}
		})
	}
}

func TestAnthropicRedactsAndTruncatesLoggedMessage(t *testing.T) {
	logs := captureLogs(t)
	message := "bad header Bearer sk-ant-fakeKey123 " + strings.Repeat("é", 400)
	srv := anthropicErrorServer(t, http.StatusBadRequest, anthropicErrorBody("invalid_request_error", message))
	_, _ = newTestClient(t, srv.URL).Messages(context.Background(), sampleRequest())
	record := logs.String()
	for _, forbidden := range []string{"sk-ant", "fakeKey123", "Bearer"} {
		if strings.Contains(record, forbidden) {
			t.Errorf("log carries %q: %s", forbidden, record)
		}
	}
	if !strings.Contains(record, "[redacted]") || !strings.Contains(record, "…") {
		t.Errorf("log is not redacted and truncated: %s", record)
	}
}

func TestAnthropicLogsUnparsableErrorBody(t *testing.T) {
	logs := captureLogs(t)
	srv := anthropicErrorServer(t, http.StatusBadGateway, "<html>gateway exploded</html>")
	_, err := newTestClient(t, srv.URL).Messages(context.Background(), sampleRequest())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if !strings.Contains(logs.String(), "gateway exploded") {
		t.Errorf("log lacks the raw body: %s", logs.String())
	}
}

func TestAnthropicStreamLogsErrorEvent(t *testing.T) {
	logs := captureLogs(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"+
			"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n")
	}))
	defer srv.Close()

	var sawError bool
	_ = newTestClient(t, srv.URL).MessagesStream(context.Background(), sampleRequest(), func(ev anthro.StreamEvent) error {
		if ev.Type == "error" {
			sawError = true
			return errors.New("translator refused the error event")
		}
		return nil
	})
	if !sawError {
		t.Fatal("the error event never reached the consumer")
	}
	record := logs.String()
	for _, want := range []string{"level=WARN", "error_type=overloaded_error", "message=Overloaded"} {
		if !strings.Contains(record, want) {
			t.Errorf("log lacks %q: %s", want, record)
		}
	}
}

func TestOpenAIContextLengthCode(t *testing.T) {
	captureLogs(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"This model's maximum context length is 128000 tokens. However, your messages resulted in 130000 tokens.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`)
	}))
	defer srv.Close()

	_, err := newTestOpenAIClient(srv.URL).Chat(context.Background(), chatRequest())
	if !errors.Is(err, ErrContextLengthExceeded) || !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrContextLengthExceeded", err)
	}
}

func TestOpenAIErrorCodeMayBeNull(t *testing.T) {
	captureLogs(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"Unknown parameter: 'foo'.","type":"invalid_request_error","param":null,"code":null}}`)
	}))
	defer srv.Close()

	_, err := newTestOpenAIClient(srv.URL).Chat(context.Background(), chatRequest())
	if !errors.Is(err, ErrInvalidRequest) || errors.Is(err, ErrContextLengthExceeded) {
		t.Fatalf("err = %v, want a plain ErrInvalidRequest", err)
	}
}
