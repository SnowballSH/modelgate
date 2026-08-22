package config

import (
	"strings"
	"testing"
	"time"
)

func validEnv() map[string]string {
	return map[string]string{
		"PUBLIC_ADDR":            "127.0.0.1:8080",
		"ADMIN_ADDR":             "127.0.0.1:8081",
		"DATA_DIR":               "/var/lib/modelgate",
		"ANTHROPIC_API_KEY_FILE": "/run/secrets/anthropic",
		"MODELS_CONFIG_FILE":     "/etc/modelgate/models.json",
		"BUDGET_MONTHLY_USD":     "25.50",
	}
}

func getenvFrom(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

func TestLoadMissingRequired(t *testing.T) {
	required := []string{
		"PUBLIC_ADDR",
		"ADMIN_ADDR",
		"DATA_DIR",
		"MODELS_CONFIG_FILE",
		"BUDGET_MONTHLY_USD",
	}
	for _, name := range required {
		t.Run(name, func(t *testing.T) {
			env := validEnv()
			delete(env, name)
			_, err := Load(getenvFrom(env))
			if err == nil {
				t.Fatalf("Load succeeded without %s", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("error %q does not mention %s", err, name)
			}
		})
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(getenvFrom(validEnv()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Config{
		PublicAddr:            "127.0.0.1:8080",
		AdminAddr:             "127.0.0.1:8081",
		MetricsAddr:           "",
		DataDir:               "/var/lib/modelgate",
		AnthropicAPIKeyFile:   "/run/secrets/anthropic",
		AnthropicBaseURL:      "https://api.anthropic.com",
		OpenAIBaseURL:         "https://api.openai.com",
		ModelsConfigFile:      "/etc/modelgate/models.json",
		BudgetMonthlyUSD:      25.50,
		AdminIdentityHeader:   "Remote-User",
		DefaultMaxTokens:      4096,
		MaxBodyBytes:          1 << 20,
		RateLimitPerKeyRPM:    60,
		MaxConcurrentRequests: 8,
		RequestDeadline:       10 * time.Minute,
	}
	if cfg != want {
		t.Fatalf("Load = %+v, want %+v", cfg, want)
	}
}

func TestLoadRejectsGarbage(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		value string
	}{
		{"bad float budget", "BUDGET_MONTHLY_USD", "not-a-number"},
		{"zero budget", "BUDGET_MONTHLY_USD", "0"},
		{"negative budget", "BUDGET_MONTHLY_USD", "-5"},
		{"bad max tokens", "DEFAULT_MAX_TOKENS", "many"},
		{"zero max tokens", "DEFAULT_MAX_TOKENS", "0"},
		{"negative max tokens", "DEFAULT_MAX_TOKENS", "-1"},
		{"bad body bytes", "MAX_BODY_BYTES", "1MB"},
		{"zero body bytes", "MAX_BODY_BYTES", "0"},
		{"bad rpm", "RATE_LIMIT_PER_KEY_RPM", "fast"},
		{"zero rpm", "RATE_LIMIT_PER_KEY_RPM", "0"},
		{"bad concurrency", "MAX_CONCURRENT_REQUESTS", "lots"},
		{"zero concurrency", "MAX_CONCURRENT_REQUESTS", "0"},
		{"bad duration", "REQUEST_DEADLINE", "soon"},
		{"zero duration", "REQUEST_DEADLINE", "0s"},
		{"negative duration", "REQUEST_DEADLINE", "-1m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := validEnv()
			env[tc.key] = tc.value
			_, err := Load(getenvFrom(env))
			if err == nil {
				t.Fatalf("Load accepted %s=%q", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("error %q does not mention %s", err, tc.key)
			}
		})
	}
}

func TestLoadFullEnv(t *testing.T) {
	env := map[string]string{
		"PUBLIC_ADDR":             "0.0.0.0:9000",
		"ADMIN_ADDR":              "127.0.0.1:9001",
		"METRICS_ADDR":            "127.0.0.1:9090",
		"DATA_DIR":                "/data",
		"ANTHROPIC_API_KEY_FILE":  "/secrets/key",
		"OPENAI_API_KEY_FILE":     "/secrets/openai-key",
		"OPENAI_BASE_URL":         "https://openai-proxy.example.com",
		"ANTHROPIC_BASE_URL":      "https://proxy.example.com",
		"MODELS_CONFIG_FILE":      "/conf/models.json",
		"BUDGET_MONTHLY_USD":      "100.25",
		"ADMIN_IDENTITY_HEADER":   "X-Auth-User",
		"DEFAULT_MAX_TOKENS":      "2048",
		"MAX_BODY_BYTES":          "5242880",
		"RATE_LIMIT_PER_KEY_RPM":  "120",
		"MAX_CONCURRENT_REQUESTS": "16",
		"REQUEST_DEADLINE":        "2m30s",
	}
	cfg, err := Load(getenvFrom(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Config{
		PublicAddr:            "0.0.0.0:9000",
		AdminAddr:             "127.0.0.1:9001",
		MetricsAddr:           "127.0.0.1:9090",
		DataDir:               "/data",
		AnthropicAPIKeyFile:   "/secrets/key",
		AnthropicBaseURL:      "https://proxy.example.com",
		OpenAIAPIKeyFile:      "/secrets/openai-key",
		OpenAIBaseURL:         "https://openai-proxy.example.com",
		ModelsConfigFile:      "/conf/models.json",
		BudgetMonthlyUSD:      100.25,
		AdminIdentityHeader:   "X-Auth-User",
		DefaultMaxTokens:      2048,
		MaxBodyBytes:          5242880,
		RateLimitPerKeyRPM:    120,
		MaxConcurrentRequests: 16,
		RequestDeadline:       2*time.Minute + 30*time.Second,
	}
	if cfg != want {
		t.Fatalf("Load = %+v, want %+v", cfg, want)
	}
}
