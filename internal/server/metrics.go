package server

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/SnowballSH/modelgate/internal/accounting"
	"github.com/SnowballSH/modelgate/internal/models"
	"github.com/SnowballSH/modelgate/internal/provider"
	"github.com/SnowballSH/modelgate/internal/store"
)

const scrapeQueryTimeout = 5 * time.Second

type Metrics struct {
	registry        *prometheus.Registry
	requests        *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
	tokens          *prometheus.CounterVec
	budget          prometheus.Gauge
	providerErrors  *prometheus.CounterVec
	bookingFailures prometheus.Counter
	inFlight        prometheus.Gauge
	keyCount        prometheus.Gauge
	bodies          prometheus.Gauge
	bodyRefusals    prometheus.Counter
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
		budget: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "modelgate_budget_usd",
			Help: "Configured monthly budget in USD.",
		}),
		providerErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "modelgate_provider_errors_total",
			Help: "Upstream provider errors by kind.",
		}, []string{"kind"}),
		bookingFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "modelgate_usage_booking_failures_total",
			Help: "Usage bookings the store refused; the provider billed spend the quota and budget do not count.",
		}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "modelgate_in_flight",
			Help: "Provider calls currently in flight.",
		}),
		keyCount: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "modelgate_keys",
			Help: "Number of API keys in the store.",
		}),
		bodies: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "modelgate_request_bodies_in_flight",
			Help: "Request bodies being read or held before admission.",
		}),
		bodyRefusals: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "modelgate_request_body_refusals_total",
			Help: "Requests refused unread because every body slot was taken.",
		}),
	}
	m.registry.MustRegister(
		m.requests, m.requestDuration, m.tokens, m.budget,
		m.providerErrors, m.bookingFailures, m.inFlight, m.keyCount,
		m.bodies, m.bodyRefusals,
	)
	m.budget.Set(budgetUSD)
	return m
}

// TrackMonthSpend exports the current month's recorded spend, read from the
// store at scrape time so the gauge follows the calendar across a month
// rollover. A failed read exports NaN rather than a stale value.
func (m *Metrics) TrackMonthSpend(s *store.Store, now func() time.Time) {
	m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "modelgate_month_spend_usd",
		Help: "Spend recorded for the current month in USD.",
	}, func() float64 {
		ctx, cancel := context.WithTimeout(context.Background(), scrapeQueryTimeout)
		defer cancel()
		spend, err := s.MonthSpend(ctx, accounting.Month(now()))
		if err != nil {
			return math.NaN()
		}
		return spend
	}))
}

// TrackProviderSpend exports each provider's ceiling and the current
// month's spend attributed to it, for every provider with a ceiling or with
// spend this month, read at scrape time like the month's total. A failed
// read exports NaN for every provider with a ceiling.
func (m *Metrics) TrackProviderSpend(acct *accounting.Accountant, now func() time.Time) {
	m.registry.MustRegister(&providerSpendCollector{acct: acct, now: now})
}

var (
	providerBudgetDesc = prometheus.NewDesc("modelgate_provider_budget_usd",
		"Configured monthly ceiling of one provider in USD.", []string{"provider"}, nil)
	providerSpendDesc = prometheus.NewDesc("modelgate_provider_month_spend_usd",
		"Spend recorded for the current month against one provider in USD.", []string{"provider"}, nil)
)

type providerSpendCollector struct {
	acct *accounting.Accountant
	now  func() time.Time
}

func (c *providerSpendCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- providerBudgetDesc
	ch <- providerSpendDesc
}

func (c *providerSpendCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), scrapeQueryTimeout)
	defer cancel()
	spends, err := c.acct.ProviderSpend(ctx, c.now())
	if err != nil {
		spends = c.unreadable()
	}
	for _, p := range spends {
		if p.BudgetUSD != nil {
			ch <- prometheus.MustNewConstMetric(providerBudgetDesc, prometheus.GaugeValue, *p.BudgetUSD, p.Provider)
		}
		ch <- prometheus.MustNewConstMetric(providerSpendDesc, prometheus.GaugeValue, p.SpendUSD, p.Provider)
	}
}

func (c *providerSpendCollector) unreadable() []accounting.ProviderSpend {
	var out []accounting.ProviderSpend
	for _, provider := range models.Providers {
		if budget := c.acct.ProviderBudget(provider); budget != nil {
			out = append(out, accounting.ProviderSpend{Provider: provider, SpendUSD: math.NaN(), BudgetUSD: budget})
		}
	}
	return out
}

// TrackBreaker exports whether the provider's circuit is refusing calls,
// evaluated at scrape time so the gauge closes when the cooldown ends.
func (m *Metrics) TrackBreaker(providerName string, b *provider.Breaker) {
	m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name:        "modelgate_breaker_open",
		Help:        "Whether the provider circuit breaker is open.",
		ConstLabels: prometheus.Labels{"provider": providerName},
	}, func() float64 {
		if b.Allow() {
			return 0
		}
		return 1
	}))
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

func (m *Metrics) ProviderError(kind string) {
	m.providerErrors.WithLabelValues(kind).Inc()
}

func (m *Metrics) BookingFailed() { m.bookingFailures.Inc() }

func (m *Metrics) IncInFlight() { m.inFlight.Inc() }
func (m *Metrics) DecInFlight() { m.inFlight.Dec() }

func (m *Metrics) IncBodies()   { m.bodies.Inc() }
func (m *Metrics) DecBodies()   { m.bodies.Dec() }
func (m *Metrics) BodyRefused() { m.bodyRefusals.Inc() }

func (m *Metrics) SetKeyCount(n float64) {
	m.keyCount.Set(n)
}

type readinessStore interface {
	Ping(ctx context.Context) error
	ProbeWrite(ctx context.Context, at time.Time) error
}

func NewReadyHandler(s readinessStore, now func() time.Time, keyFiles ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.Ping(r.Context()); err != nil {
			http.Error(w, "store unreachable", http.StatusServiceUnavailable)
			return
		}
		if err := s.ProbeWrite(r.Context(), now()); err != nil {
			http.Error(w, "store not writable", http.StatusServiceUnavailable)
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
