package server

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SnowballSH/modelgate/internal/accounting"
	"github.com/SnowballSH/modelgate/internal/config"
	"github.com/SnowballSH/modelgate/internal/models"
	"github.com/SnowballSH/modelgate/internal/provider"
	"github.com/SnowballSH/modelgate/internal/store"
)

const (
	breakerThreshold = 5
	breakerCooldown  = 30 * time.Second
	drainTimeout     = 30 * time.Second
)

type Server struct {
	store    *store.Store
	public   *http.Server
	admin    *http.Server
	metrics  *http.Server
	pubLn    net.Listener
	admLn    net.Listener
	metLn    net.Listener
	sentinel *Metrics
}

// New performs every fail-closed startup check, opens the store, and binds
// all listeners. Any missing precondition is an error before a port opens.
func New(cfg config.Config, static http.Handler) (*Server, error) {
	keyBytes, err := os.ReadFile(cfg.AnthropicAPIKeyFile)
	if err != nil {
		return nil, fmt.Errorf("startup: ANTHROPIC_API_KEY_FILE: %w", err)
	}
	apiKey := strings.TrimSpace(string(keyBytes))
	if apiKey == "" {
		return nil, fmt.Errorf("startup: ANTHROPIC_API_KEY_FILE %s is empty", cfg.AnthropicAPIKeyFile)
	}

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("startup: DATA_DIR: %w", err)
	}
	probe := filepath.Join(cfg.DataDir, ".writable")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		return nil, fmt.Errorf("startup: DATA_DIR %s is not writable: %w", cfg.DataDir, err)
	}
	_ = os.Remove(probe)

	table, err := models.LoadTable(cfg.ModelsConfigFile)
	if err != nil {
		return nil, fmt.Errorf("startup: MODELS_CONFIG_FILE: %w", err)
	}

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("startup: open store: %w", err)
	}

	acct := accounting.New(st, cfg.BudgetMonthlyUSD)
	metrics := NewMetrics(cfg.BudgetMonthlyUSD)
	breaker := provider.NewBreaker(breakerThreshold, breakerCooldown, time.Now)
	client := provider.NewClient(cfg.AnthropicBaseURL, apiKey, &http.Client{})
	guards := NewGuards(st, acct, table, cfg.RateLimitPerKeyRPM, cfg.MaxConcurrentRequests, cfg.MaxBodyBytes, time.Now)

	s := &Server{store: st, sentinel: metrics}
	s.public = &http.Server{Handler: NewPublicHandler(
		guards, table, acct, st, client, breaker, metrics,
		cfg.DefaultMaxTokens, cfg.MaxBodyBytes, cfg.RequestDeadline, time.Now)}
	s.admin = &http.Server{Handler: NewAdminHandler(
		st, acct, table, metrics, cfg.AdminIdentityHeader,
		cfg.BudgetMonthlyUSD, time.Now, rand.Reader, static)}

	if s.pubLn, err = net.Listen("tcp", cfg.PublicAddr); err != nil {
		s.closeAll()
		return nil, fmt.Errorf("startup: PUBLIC_ADDR: %w", err)
	}
	if s.admLn, err = net.Listen("tcp", cfg.AdminAddr); err != nil {
		s.closeAll()
		return nil, fmt.Errorf("startup: ADMIN_ADDR: %w", err)
	}
	if cfg.MetricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", metrics.Handler())
		mux.Handle("/ready", NewReadyHandler(st, cfg.AnthropicAPIKeyFile))
		s.metrics = &http.Server{Handler: mux}
		if s.metLn, err = net.Listen("tcp", cfg.MetricsAddr); err != nil {
			s.closeAll()
			return nil, fmt.Errorf("startup: METRICS_ADDR: %w", err)
		}
	}
	return s, nil
}

func (s *Server) PublicAddr() string { return s.pubLn.Addr().String() }
func (s *Server) AdminAddr() string  { return s.admLn.Addr().String() }
func (s *Server) MetricsAddr() string {
	if s.metLn == nil {
		return ""
	}
	return s.metLn.Addr().String()
}

// Serve blocks until ctx is cancelled or a listener fails, then stops
// accepting and drains in-flight requests for up to drainTimeout.
func (s *Server) Serve(ctx context.Context) error {
	errCh := make(chan error, 3)
	serve := func(srv *http.Server, ln net.Listener) {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}
	go serve(s.public, s.pubLn)
	go serve(s.admin, s.admLn)
	if s.metrics != nil {
		go serve(s.metrics, s.metLn)
	}

	var cause error
	select {
	case <-ctx.Done():
	case cause = <-errCh:
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	for _, srv := range []*http.Server{s.public, s.admin, s.metrics} {
		if srv != nil {
			_ = srv.Shutdown(drainCtx)
		}
	}
	_ = s.store.Close()
	return cause
}

func (s *Server) closeAll() {
	for _, ln := range []net.Listener{s.pubLn, s.admLn, s.metLn} {
		if ln != nil {
			_ = ln.Close()
		}
	}
	_ = s.store.Close()
}

// Run is the production entry point: New, then Serve until ctx ends.
func Run(ctx context.Context, cfg config.Config, static http.Handler) error {
	s, err := New(cfg, static)
	if err != nil {
		return err
	}
	return s.Serve(ctx)
}
