package server

import "sync"

// bodyGate bounds how many request bodies the gateway reads and holds at
// once, from before the read until the request takes a concurrency slot or
// is refused, so the memory bodies hold does not grow with the connections
// a client opens. It never queues: a request finding every slot taken is
// refused before its body is read.
type bodyGate struct {
	slots   chan struct{}
	metrics *Metrics
}

func newBodyGate(size int, m *Metrics) *bodyGate {
	return &bodyGate{slots: make(chan struct{}, size), metrics: m}
}

// enter takes a slot, or reports that none is free. The release it returns
// is idempotent.
func (g *bodyGate) enter() (release func(), ok bool) {
	select {
	case g.slots <- struct{}{}:
	default:
		g.metrics.BodyRefused()
		return nil, false
	}
	g.metrics.IncBodies()
	return sync.OnceFunc(func() {
		<-g.slots
		g.metrics.DecBodies()
	}), true
}
