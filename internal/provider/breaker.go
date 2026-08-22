package provider

import (
	"errors"
	"sync"
	"time"
)

type Breaker struct {
	mu          sync.Mutex
	threshold   int
	cooldown    time.Duration
	now         func() time.Time
	failures    int
	open        bool
	lastFailure time.Time
}

func NewBreaker(threshold int, cooldown time.Duration, now func() time.Time) *Breaker {
	if now == nil {
		now = time.Now
	}
	return &Breaker{threshold: threshold, cooldown: cooldown, now: now}
}

func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.open {
		return true
	}
	return b.now().Sub(b.lastFailure) >= b.cooldown
}

func (b *Breaker) Record(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err == nil {
		b.failures = 0
		b.open = false
		return
	}
	if !isProviderFailure(err) {
		return
	}
	b.failures++
	b.lastFailure = b.now()
	if b.failures >= b.threshold {
		b.open = true
	}
}

// isProviderFailure decides what may open the circuit. Timeouts are
// excluded: they are usually the gateway's own request deadline expiring
// under a legitimately long stream, and a genuinely hung provider also
// produces transport errors that do count.
func isProviderFailure(err error) bool {
	return errors.Is(err, ErrAuth) ||
		errors.Is(err, ErrRateLimited) ||
		errors.Is(err, ErrUnavailable)
}
