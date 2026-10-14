//go:build integration

package integration

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/SnowballSH/modelgate/internal/config"
)

func TestStalledBodiesCannotHoldMoreThanTheBound(t *testing.T) {
	gw := startGatewayWith(t, 20, defaultTable, func(cfg *config.Config) { cfg.MaxConcurrentBodies = 1 })
	stalledKey := adminCreateKey(t, gw.AdminAddr, "stalled")
	fullKey := adminCreateKey(t, gw.AdminAddr, "bodies")

	conn, err := net.Dial("tcp", gw.PublicAddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: 4096\r\n\r\n{\"model\"", stalledKey)
	waitForMetric(t, gw.MetricsAddr, "modelgate_request_bodies_in_flight", 1)

	res := chatCompletionRaw(t, gw.PublicAddr, fullKey)
	if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") != "1" {
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		t.Fatalf("another key's request while the only body slot is held: status %d Retry-After %q, want 429 after 1s", res.StatusCode, res.Header.Get("Retry-After"))
	}
	if code := decodeErrorCode(t, res); code != "rate_limited" {
		t.Errorf("body-slot refusal code = %q, want rate_limited", code)
	}
	if got := metricValue(t, gw.MetricsAddr, `modelgate_request_body_refusals_total{limit="gateway"}`); got != 1 {
		t.Errorf("body refusals = %v, want 1", got)
	}

	_ = conn.Close()
	waitForMetric(t, gw.MetricsAddr, "modelgate_request_bodies_in_flight", 0)
	res = chatCompletionRaw(t, gw.PublicAddr, fullKey)
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("a request after the stalled client left: status %d, want 200", res.StatusCode)
	}
}

func waitForMetric(t *testing.T, metricsAddr, series string, want float64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for metricValue(t, metricsAddr, series) != want {
		if time.Now().After(deadline) {
			t.Fatalf("%s never reached %v", series, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
