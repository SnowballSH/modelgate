package server

import "sync"

const (
	bodyLimitGateway = "gateway"
	bodyLimitKey     = "key"
)

// bodyGate bounds how many request bodies the gateway reads and holds at
// once, and how many of them one key holds, from before the read until the
// request takes a concurrency slot or is refused, so the memory bodies hold
// does not grow with the connections a client opens and one key cannot
// starve the others. It never queues: a request over either bound is
// refused before its body is read.
type bodyGate struct {
	slots   chan struct{}
	perKey  int
	metrics *Metrics

	mu    sync.Mutex
	byKey map[string]int
}

func newBodyGate(size, perKey int, m *Metrics) *bodyGate {
	return &bodyGate{slots: make(chan struct{}, size), perKey: perKey, metrics: m, byKey: map[string]int{}}
}

// enter takes a slot for the key, or refuses the request. The release it
// returns is idempotent.
func (g *bodyGate) enter(keyID string) (release func(), refusal *Refusal) {
	if !g.claimForKey(keyID) {
		g.metrics.BodyRefused(bodyLimitKey)
		return nil, &Refusal{Code: CodeRateLimited, RetryAfter: slotRetryAfter, Message: "this key's concurrent request limit is reached; retry shortly"}
	}
	select {
	case g.slots <- struct{}{}:
	default:
		g.releaseForKey(keyID)
		g.metrics.BodyRefused(bodyLimitGateway)
		return nil, &Refusal{Code: CodeRateLimited, RetryAfter: slotRetryAfter, Message: "too many request bodies in flight; retry shortly"}
	}
	g.metrics.IncBodies()
	return sync.OnceFunc(func() {
		<-g.slots
		g.releaseForKey(keyID)
		g.metrics.DecBodies()
	}), nil
}

func (g *bodyGate) claimForKey(keyID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.byKey[keyID] >= g.perKey {
		return false
	}
	g.byKey[keyID]++
	return true
}

func (g *bodyGate) releaseForKey(keyID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.byKey[keyID]--; g.byKey[keyID] == 0 {
		delete(g.byKey, keyID)
	}
}
