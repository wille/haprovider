package ws

import (
	"context"
	"sync"
)

// tracker counts open proxied WebSocket connections and coordinates a graceful
// shutdown. http.Server.Shutdown does not track hijacked connections, so
// without this the process exits with WebSocket clients still attached and
// cuts them off without a close frame.
//
// It's a mutex-guarded counter rather than a sync.WaitGroup because
// connections can still be added while Wait is in progress, which a WaitGroup
// does not allow.
type tracker struct {
	mu      sync.Mutex
	closing bool
	n       int

	// shutdown is closed when shutdown begins; open connections watch it.
	shutdown chan struct{}

	// idle is closed once shutdown has begun and every connection has closed.
	idle chan struct{}
}

func newTracker() *tracker {
	return &tracker{
		shutdown: make(chan struct{}),
		idle:     make(chan struct{}),
	}
}

// connections is the process-wide tracker. Tests replace it.
var connections = newTracker()

// add registers a new connection. It returns false once shutdown has begun,
// in which case the caller must not open the connection.
func (t *tracker) add() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closing {
		return false
	}
	t.n++
	return true
}

// done unregisters a connection added with add.
func (t *tracker) done() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.n--
	if t.closing && t.n == 0 {
		close(t.idle)
	}
}

func (t *tracker) closeAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closing {
		return
	}
	t.closing = true
	close(t.shutdown)
	if t.n == 0 {
		close(t.idle)
	}
}

func (t *tracker) wait(ctx context.Context) error {
	select {
	case <-t.idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// CloseAll begins a graceful shutdown: every open WebSocket connection is
// closed with a "going away" close frame to both the client and the provider,
// and new connections are refused. It returns immediately; use Wait to block
// until they have closed. Safe to call more than once. Intended for
// http.Server.RegisterOnShutdown.
func CloseAll() {
	connections.closeAll()
}

// Wait blocks until every WebSocket connection has closed after CloseAll, or
// until ctx is done, in which case it returns ctx.Err().
func Wait(ctx context.Context) error {
	return connections.wait(ctx)
}
