package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SnowballSH/modelgate/internal/config"
)

func assemblyConfig(t *testing.T) config.Config {
	t.Helper()
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	if err := os.WriteFile(keyFile, []byte("sk-ant-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	modelsFile := filepath.Join(dir, "models.json")
	table := `{"models":{"claude-sonnet-5":{"provider_model":"claude-sonnet-5",
		"input_usd_per_mtok":3,"output_usd_per_mtok":15,
		"cache_read_usd_per_mtok":0.3,"cache_write_usd_per_mtok":3.75}}}`
	if err := os.WriteFile(modelsFile, []byte(table), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.Config{
		PublicAddr:            "127.0.0.1:0",
		AdminAddr:             "127.0.0.1:0",
		MetricsAddr:           "127.0.0.1:0",
		DataDir:               filepath.Join(dir, "data"),
		AnthropicAPIKeyFile:   keyFile,
		AnthropicBaseURL:      "http://127.0.0.1:1",
		ModelsConfigFile:      modelsFile,
		BudgetMonthlyUSD:      20,
		AdminIdentityHeader:   "Remote-User",
		DefaultMaxTokens:      4096,
		MaxBodyBytes:          1 << 20,
		RateLimitPerKeyRPM:    60,
		MaxConcurrentRequests: 8,
		RequestDeadline:       time.Minute,
	}
}

func TestNewRefusesMissingKeyFile(t *testing.T) {
	cfg := assemblyConfig(t)
	cfg.AnthropicAPIKeyFile = filepath.Join(t.TempDir(), "absent")
	if _, err := New(cfg, nil); err == nil || !contains(err.Error(), "ANTHROPIC_API_KEY_FILE") {
		t.Fatalf("expected key-file startup refusal, got %v", err)
	}
}

func TestNewRefusesEmptyKeyFile(t *testing.T) {
	cfg := assemblyConfig(t)
	if err := os.WriteFile(cfg.AnthropicAPIKeyFile, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg, nil); err == nil || !contains(err.Error(), "empty") {
		t.Fatalf("expected empty-key refusal, got %v", err)
	}
}

func TestNewRefusesBadModelTable(t *testing.T) {
	cfg := assemblyConfig(t)
	if err := os.WriteFile(cfg.ModelsConfigFile, []byte(`{"models":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg, nil); err == nil || !contains(err.Error(), "MODELS_CONFIG_FILE") {
		t.Fatalf("expected model-table refusal, got %v", err)
	}
}

func TestServeReadyAndShutdown(t *testing.T) {
	cfg := assemblyConfig(t)
	s, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()

	readyURL := fmt.Sprintf("http://%s/ready", s.MetricsAddr())
	waitFor200(t, readyURL)

	res, err := http.Get(fmt.Sprintf("http://%s/nope", s.PublicAddr()))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown public route: got %d, want 404", res.StatusCode)
	}

	res, err = http.Get(fmt.Sprintf("http://%s/api/keys", s.AdminAddr()))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("admin without identity: got %d, want 401", res.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v after cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}

func waitFor200(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		res, err := http.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never answered 200", url)
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
