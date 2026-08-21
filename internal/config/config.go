package config

import (
	"fmt"
	"strconv"
	"time"
)

type Config struct {
	PublicAddr, AdminAddr, MetricsAddr string
	DataDir                            string
	AnthropicAPIKeyFile                string
	AnthropicBaseURL                   string
	ModelsConfigFile                   string
	BudgetMonthlyUSD                   float64
	AdminIdentityHeader                string
	DefaultMaxTokens                   int
	MaxBodyBytes                       int64
	RateLimitPerKeyRPM                 int
	MaxConcurrentRequests              int
	RequestDeadline                    time.Duration
}

func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		PublicAddr:            getenv("PUBLIC_ADDR"),
		AdminAddr:             getenv("ADMIN_ADDR"),
		MetricsAddr:           getenv("METRICS_ADDR"),
		DataDir:               getenv("DATA_DIR"),
		AnthropicAPIKeyFile:   getenv("ANTHROPIC_API_KEY_FILE"),
		AnthropicBaseURL:      withDefault(getenv("ANTHROPIC_BASE_URL"), "https://api.anthropic.com"),
		ModelsConfigFile:      getenv("MODELS_CONFIG_FILE"),
		AdminIdentityHeader:   withDefault(getenv("ADMIN_IDENTITY_HEADER"), "Remote-User"),
		DefaultMaxTokens:      4096,
		MaxBodyBytes:          1 << 20,
		RateLimitPerKeyRPM:    60,
		MaxConcurrentRequests: 8,
		RequestDeadline:       10 * time.Minute,
	}

	required := []struct {
		name  string
		value string
	}{
		{"PUBLIC_ADDR", cfg.PublicAddr},
		{"ADMIN_ADDR", cfg.AdminAddr},
		{"DATA_DIR", cfg.DataDir},
		{"ANTHROPIC_API_KEY_FILE", cfg.AnthropicAPIKeyFile},
		{"MODELS_CONFIG_FILE", cfg.ModelsConfigFile},
		{"BUDGET_MONTHLY_USD", getenv("BUDGET_MONTHLY_USD")},
	}
	for _, r := range required {
		if r.value == "" {
			return Config{}, fmt.Errorf("config: %s: required but not set", r.name)
		}
	}

	budget, err := strconv.ParseFloat(getenv("BUDGET_MONTHLY_USD"), 64)
	if err != nil || budget <= 0 {
		return Config{}, fmt.Errorf("config: BUDGET_MONTHLY_USD: must be a positive number")
	}
	cfg.BudgetMonthlyUSD = budget

	if err := parsePositiveInt(getenv, "DEFAULT_MAX_TOKENS", &cfg.DefaultMaxTokens); err != nil {
		return Config{}, err
	}
	if err := parsePositiveInt64(getenv, "MAX_BODY_BYTES", &cfg.MaxBodyBytes); err != nil {
		return Config{}, err
	}
	if err := parsePositiveInt(getenv, "RATE_LIMIT_PER_KEY_RPM", &cfg.RateLimitPerKeyRPM); err != nil {
		return Config{}, err
	}
	if err := parsePositiveInt(getenv, "MAX_CONCURRENT_REQUESTS", &cfg.MaxConcurrentRequests); err != nil {
		return Config{}, err
	}

	if raw := getenv("REQUEST_DEADLINE"); raw != "" {
		deadline, err := time.ParseDuration(raw)
		if err != nil || deadline <= 0 {
			return Config{}, fmt.Errorf("config: REQUEST_DEADLINE: must be a positive duration")
		}
		cfg.RequestDeadline = deadline
	}

	return cfg, nil
}

func withDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func parsePositiveInt(getenv func(string) string, name string, dst *int) error {
	raw := getenv(name)
	if raw == "" {
		return nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return fmt.Errorf("config: %s: must be a positive integer", name)
	}
	*dst = v
	return nil
}

func parsePositiveInt64(getenv func(string) string, name string, dst *int64) error {
	raw := getenv(name)
	if raw == "" {
		return nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 {
		return fmt.Errorf("config: %s: must be a positive integer", name)
	}
	*dst = v
	return nil
}
