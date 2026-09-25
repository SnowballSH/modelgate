package provider

import (
	"context"
	"errors"
	"io"
	"time"
)

// DefaultStreamIdleTimeout bounds how long a streamed call may go without a
// byte from the upstream, response headers included. It sits well inside the
// gateway's request deadline and well above the silences a healthy stream
// shows: pings, and the pause before output while the model reasons.
const DefaultStreamIdleTimeout = 5 * time.Minute

var errStreamIdle = errors.New("upstream stream idle")

// idleGuard cancels a stream's context once the upstream has sent nothing for
// the timeout; every byte read re-arms it. A timeout of zero or less turns the
// guard off.
type idleGuard struct {
	timeout time.Duration
	timer   *time.Timer
	cancel  context.CancelCauseFunc
}

func guardIdle(parent context.Context, timeout time.Duration) (context.Context, *idleGuard) {
	ctx, cancel := context.WithCancelCause(parent)
	guard := &idleGuard{timeout: timeout, cancel: cancel}
	if timeout > 0 {
		guard.timer = time.AfterFunc(timeout, func() { cancel(errStreamIdle) })
	}
	return ctx, guard
}

func (g *idleGuard) reader(r io.Reader) io.Reader {
	if g.timer == nil {
		return r
	}
	return activityReader{r: r, touch: func() { g.timer.Reset(g.timeout) }}
}

func (g *idleGuard) stop() {
	if g.timer != nil {
		g.timer.Stop()
	}
	g.cancel(nil)
}

type activityReader struct {
	r     io.Reader
	touch func()
}

func (a activityReader) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 {
		a.touch()
	}
	return n, err
}
