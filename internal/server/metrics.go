package server

import (
	"fmt"
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/SnowballSH/modelgate/internal/store"
)

type Metrics struct {
	registry        *prometheus.Registry
	requests        *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
	tokens          *prometheus.CounterVec
	monthSpend      prometheus.Gauge
	budget          prometheus.Gauge
	providerErrors  *prometheus.CounterVec
	breakerOpen     *prometheus.GaugeVec
	inFlight        prometheus.Gauge
	keyCount        prometheus.Gauge
}

func NewMetrics(budgetUSD float64) *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "modelgate_requests_total",
			Help: "Chat requests by outcome and model.",
		}, []string{"outcome", "model"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "modelgate_request_duration_seconds",
			Help:    "Chat request duration in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"model"}),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "modelgate_tokens_total",
			Help: "Tokens processed by direction and model.",
		}, []string{"direction", "model"}),
		monthSpend: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "modelgate_month_spend_usd",
			Help: "Spend recorded for the current month in USD.",
		}),
		budget: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "modelgate_budget_usd",
			Help: "Configured monthly budget in USD.",
		}),
		providerErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "modelgate_provider_errors_total",
			Help: "Upstream provider errors by kind.",
		}, []string{"kind"}),
		breakerOpen: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "modelgate_breaker_open",
			Help: "Whether the provider circuit breaker is open.",
		}, []string{"provider"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "modelgate_in_flight",
			Help: "Provider calls currently in flight.",
		}),
		keyCount: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "modelgate_keys",
			Help: "Number of API keys in the store.",
		}),
	}
	m.registry.MustRegister(
		m.requests, m.requestDuration, m.tokens, m.monthSpend, m.budget,
		m.providerErrors, m.breakerOpen, m.inFlight, m.keyCount,
	)
	m.budget.Set(budgetUSD)
	return m
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *Metrics) ObserveRequest(outcome, model string, seconds float64) {
	m.requests.WithLabelValues(outcome, model).Inc()
	m.requestDuration.WithLabelValues(model).Observe(seconds)
}

func (m *Metrics) AddTokens(model string, u store.Usage) {
	m.tokens.WithLabelValues("input", model).Add(float64(u.InputTokens))
	m.tokens.WithLabelValues("output", model).Add(float64(u.OutputTokens))
	m.tokens.WithLabelValues("cache_read", model).Add(float64(u.CacheReadTokens))
	m.tokens.WithLabelValues("cache_write", model).Add(float64(u.CacheWriteTokens))
}

func (m *Metrics) SetMonthSpend(v float64) {
	m.monthSpend.Set(v)
}

func (m *Metrics) ProviderError(kind string) {
	m.providerErrors.WithLabelValues(kind).Inc()
}

func (m *Metrics) SetBreakerOpen(providerName string, open bool) {
	v := 0.0
	if open {
		v = 1
	}
	m.breakerOpen.WithLabelValues(providerName).Set(v)
}

func (m *Metrics) IncInFlight() { m.inFlight.Inc() }
func (m *Metrics) DecInFlight() { m.inFlight.Dec() }

func (m *Metrics) SetKeyCount(n float64) {
	m.keyCount.Set(n)
}

func NewReadyHandler(s *store.Store, keyFiles ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.Ping(r.Context()); err != nil {
			http.Error(w, "store unreachable", http.StatusServiceUnavailable)
			return
		}
		for _, keyFile := range keyFiles {
			f, err := os.Open(keyFile)
			if err != nil {
				http.Error(w, "provider key file unreadable", http.StatusServiceUnavailable)
				return
			}
			info, err := f.Stat()
			f.Close()
			if err != nil {
				http.Error(w, "provider key file unreadable", http.StatusServiceUnavailable)
				return
			}
			if info.Size() == 0 {
				http.Error(w, "provider key file empty", http.StatusServiceUnavailable)
				return
			}
		}
		fmt.Fprintln(w, "ready")
	})
}
