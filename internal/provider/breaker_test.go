package provider

import (
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestBreaker(threshold int, cooldown time.Duration) (*Breaker, *fakeClock) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	return NewBreaker(threshold, cooldown, clock.now), clock
}

func TestBreakerOpensAtThreshold(t *testing.T) {
	b, _ := newTestBreaker(3, time.Minute)
	b.Record(ErrUnavailable)
	b.Record(ErrRateLimited)
	if !b.Allow() {
		t.Fatal("breaker opened before threshold")
	}
	b.Record(ErrTimeout)
	if b.Allow() {
		t.Fatal("breaker did not open at threshold")
	}
}

func TestBreakerAllowAfterCooldown(t *testing.T) {
	b, clock := newTestBreaker(1, time.Minute)
	b.Record(ErrAuth)
	if b.Allow() {
		t.Fatal("expected open breaker within cooldown")
	}
	clock.advance(30 * time.Second)
	if b.Allow() {
		t.Fatal("expected open breaker before cooldown elapsed")
	}
	clock.advance(30 * time.Second)
	if !b.Allow() {
		t.Fatal("expected half-open breaker after cooldown")
	}
}

func TestBreakerSuccessInHalfOpenCloses(t *testing.T) {
	b, clock := newTestBreaker(1, time.Minute)
	b.Record(ErrUnavailable)
	clock.advance(time.Minute)
	if !b.Allow() {
		t.Fatal("expected half-open breaker")
	}
	b.Record(nil)
	clock.advance(-time.Minute + time.Second)
	if !b.Allow() {
		t.Fatal("expected closed breaker after half-open success")
	}
}

func TestBreakerFailureInHalfOpenReopens(t *testing.T) {
	b, clock := newTestBreaker(2, time.Minute)
	b.Record(ErrUnavailable)
	b.Record(ErrUnavailable)
	clock.advance(time.Minute)
	if !b.Allow() {
		t.Fatal("expected half-open breaker")
	}
	b.Record(ErrUnavailable)
	if b.Allow() {
		t.Fatal("expected breaker to re-open on half-open failure")
	}
	clock.advance(time.Minute)
	if !b.Allow() {
		t.Fatal("expected half-open breaker after second cooldown")
	}
}

func TestBreakerNilRecordResetsConsecutiveCount(t *testing.T) {
	b, _ := newTestBreaker(2, time.Minute)
	b.Record(ErrUnavailable)
	b.Record(nil)
	b.Record(ErrUnavailable)
	if !b.Allow() {
		t.Fatal("nil Record should reset the consecutive failure count")
	}
	b.Record(ErrUnavailable)
	if b.Allow() {
		t.Fatal("expected breaker open after two consecutive failures")
	}
}
