package client

import (
	"context"
	"sync"
)

// gate is the in-flight bound (CLT-012): excess calls wait, they are never
// refused; close fails waiters with the typed connection-closed error
// (CLT-004). A buffered channel is the permit set; a separate closed channel
// unblocks waiters on shutdown.
type gate struct {
	sem    chan struct{}
	closed chan struct{}
	once   sync.Once
}

func newGate(limit int) *gate {
	if limit < 1 {
		limit = 1
	}
	return &gate{
		sem:    make(chan struct{}, limit),
		closed: make(chan struct{}),
	}
}

// acquire takes one permit, waiting when the bound is reached (CLT-012).
// Returns the typed closed error if the gate closes, or the context error if
// ctx is cancelled while waiting.
func (g *gate) acquire(ctx context.Context) error {
	select {
	case <-g.closed:
		return closedError()
	default:
	}
	select {
	case g.sem <- struct{}{}:
		// A close racing with this acquire must still surface as closed.
		select {
		case <-g.closed:
			<-g.sem
			return closedError()
		default:
			return nil
		}
	case <-g.closed:
		return closedError()
	case <-ctx.Done():
		if err := ctx.Err(); err == context.DeadlineExceeded {
			return &TimeoutError{Msg: "timed out"}
		}
		return &ConnectionError{Msg: "call cancelled: " + ctx.Err().Error()}
	}
}

// release returns one permit. Call only after a successful acquire.
func (g *gate) release() {
	<-g.sem
}

// close unblocks every waiter with the closed error (CLT-004). Idempotent.
func (g *gate) close() {
	g.once.Do(func() { close(g.closed) })
}
