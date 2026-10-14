package config

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/SnowballSH/modelgate/internal/models"
)

type Config struct {
	PublicAddr, AdminAddr, MetricsAddr string
	DataDir                            string
	AnthropicAPIKeyFile                string
	AnthropicBaseURL                   string
	OpenAIAPIKeyFile                   string
	OpenAIBaseURL                      string
	ModelsConfigFile                   string
	BudgetMonthlyUSD                   float64
	ProviderBudgetsUSD                 map[string]float64
	AdminIdentityHeader                string
	AdminAllowedUsers                  []string
	AdminProxySecretFile               string
	DefaultMaxTokens                   int
	MaxOutputTokens                    int
	MaxBodyBytes                       int64
	RateLimitPerKeyRPM                 int
	MaxConcurrentRequests              int
	MaxConcurrentBodies                int
	MaxBodiesPerKey                    int
	RequestDeadline                    time.Duration
	Version                            string
}

func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		PublicAddr:            getenv("PUBLIC_ADDR"),
		AdminAddr:             getenv("ADMIN_ADDR"),
		MetricsAddr:           getenv("METRICS_ADDR"),
		DataDir:               getenv("DATA_DIR"),
		AnthropicAPIKeyFile:   getenv("ANTHROPIC_API_KEY_FILE"),
		AnthropicBaseURL:      withDefault(getenv("ANTHROPIC_BASE_URL"), "https://api.anthropic.com"),
		OpenAIAPIKeyFile:      getenv("OPENAI_API_KEY_FILE"),
		OpenAIBaseURL:         withDefault(getenv("OPENAI_BASE_URL"), "https://api.openai.com"),
		ModelsConfigFile:      getenv("MODELS_CONFIG_FILE"),
		AdminIdentityHeader:   withDefault(getenv("ADMIN_IDENTITY_HEADER"), "Remote-User"),
		AdminAllowedUsers:     splitList(getenv("ADMIN_ALLOWED_USERS")),
		AdminProxySecretFile:  getenv("ADMIN_PROXY_SECRET_FILE"),
		DefaultMaxTokens:      4096,
		MaxOutputTokens:       128_000,
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
		{"MODELS_CONFIG_FILE", cfg.ModelsConfigFile},
		{"BUDGET_MONTHLY_USD", getenv("BUDGET_MONTHLY_USD")},
		{"ADMIN_ALLOWED_USERS", strings.Join(cfg.AdminAllowedUsers, ",")},
		{"ADMIN_PROXY_SECRET_FILE", cfg.AdminProxySecretFile},
	}
	for _, r := range required {
		if r.value == "" {
			return Config{}, fmt.Errorf("config: %s: required but not set", r.name)
		}
	}

	budget, err := parseBudget(getenv, "BUDGET_MONTHLY_USD")
	if err != nil {
		return Config{}, err
	}
	cfg.BudgetMonthlyUSD = budget
	if cfg.ProviderBudgetsUSD, err = parseProviderBudgets(getenv); err != nil {
		return Config{}, err
	}

	if err := parsePositive(getenv, "DEFAULT_MAX_TOKENS", &cfg.DefaultMaxTokens); err != nil {
		return Config{}, err
	}
	if err := parsePositive(getenv, "MAX_OUTPUT_TOKENS", &cfg.MaxOutputTokens); err != nil {
		return Config{}, err
	}
	if cfg.DefaultMaxTokens > cfg.MaxOutputTokens {
		return Config{}, fmt.Errorf("config: DEFAULT_MAX_TOKENS: %d exceeds MAX_OUTPUT_TOKENS %d", cfg.DefaultMaxTokens, cfg.MaxOutputTokens)
	}
	if err := parsePositive(getenv, "MAX_BODY_BYTES", &cfg.MaxBodyBytes); err != nil {
		return Config{}, err
	}
	if err := parsePositive(getenv, "RATE_LIMIT_PER_KEY_RPM", &cfg.RateLimitPerKeyRPM); err != nil {
		return Config{}, err
	}
	if err := parsePositive(getenv, "MAX_CONCURRENT_REQUESTS", &cfg.MaxConcurrentRequests); err != nil {
		return Config{}, err
	}
	cfg.MaxConcurrentBodies = cfg.MaxConcurrentRequests
	if err := parsePositive(getenv, "MAX_CONCURRENT_BODIES", &cfg.MaxConcurrentBodies); err != nil {
		return Config{}, err
	}
	cfg.MaxBodiesPerKey = (cfg.MaxConcurrentBodies + 3) / 4
	if err := parsePositive(getenv, "MAX_BODIES_PER_KEY", &cfg.MaxBodiesPerKey); err != nil {
		return Config{}, err
	}

	if raw := getenv("REQUEST_DEADLINE"); raw != "" {
		deadline, err := time.ParseDuration(raw)
		if err != nil || deadline <= 0 {
			return Config{}, errors.New("config: REQUEST_DEADLINE: must be a positive duration")
		}
		cfg.RequestDeadline = deadline
	}

	return cfg, nil
}

func parseBudget(getenv func(string) string, name string) (float64, error) {
	budget, err := strconv.ParseFloat(getenv(name), 64)
	if err != nil || math.IsNaN(budget) || math.IsInf(budget, 0) || budget <= 0 {
		return 0, fmt.Errorf("config: %s: must be a positive finite number", name)
	}
	return budget, nil
}

// providerBudgetVar names the optional setting that caps one provider's
// monthly spend, e.g. BUDGET_MONTHLY_USD_ANTHROPIC.
func providerBudgetVar(provider string) string {
	return "BUDGET_MONTHLY_USD_" + strings.ToUpper(provider)
}

// parseProviderBudgets reads a ceiling for each provider that sets one; a
// provider without its variable is capped only by the global budget.
func parseProviderBudgets(getenv func(string) string) (map[string]float64, error) {
	var budgets map[string]float64
	for _, provider := range models.Providers {
		name := providerBudgetVar(provider)
		if getenv(name) == "" {
			continue
		}
		budget, err := parseBudget(getenv, name)
		if err != nil {
			return nil, err
		}
		if budgets == nil {
			budgets = map[string]float64{}
		}
		budgets[provider] = budget
	}
	return budgets, nil
}

// splitList reads a comma-separated list, dropping blank entries so a
// trailing comma cannot allow the empty identity.
func splitList(raw string) []string {
	var out []string
	for item := range strings.SplitSeq(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func withDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func parsePositive[T int | int64](getenv func(string) string, name string, dst *T) error {
	raw := getenv(name)
	if raw == "" {
		return nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 || int64(T(v)) != v {
		return fmt.Errorf("config: %s: must be a positive integer", name)
	}
	*dst = T(v)
	return nil
}
