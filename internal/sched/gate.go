package sched

import (
	"context"
	"sync"
)

// gate is a resizable concurrency limiter. Acquire blocks until a slot is free or
// the context is done; SetLimit may raise or lower the limit at any time (a
// lowered limit is honoured as running work finishes).
type gate struct {
	mu      sync.Mutex
	limit   int
	active  int
	waiters []chan struct{}
}

func newGate(limit int) *gate {
	if limit < 1 {
		limit = 1
	}
	return &gate{limit: limit}
}

// SetLimit changes the maximum number of concurrent holders.
func (g *gate) SetLimit(n int) {
	if n < 1 {
		n = 1
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.limit = n
	g.wakeLocked()
}

// Limit reports the configured limit.
func (g *gate) Limit() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.limit
}

// Acquire takes a slot, blocking while the gate is full.
func (g *gate) Acquire(ctx context.Context) error {
	g.mu.Lock()
	if g.active < g.limit {
		g.active++
		g.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	g.waiters = append(g.waiters, ch)
	g.mu.Unlock()

	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		g.mu.Lock()
		granted := true
		for i, w := range g.waiters {
			if w == ch {
				g.waiters = append(g.waiters[:i], g.waiters[i+1:]...)
				granted = false
				break
			}
		}
		g.mu.Unlock()
		if granted {
			// The slot was handed to us concurrently; give it back.
			g.Release()
		}
		return ctx.Err()
	}
}

// Release returns a slot.
func (g *gate) Release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active > 0 {
		g.active--
	}
	g.wakeLocked()
}

// wakeLocked hands free slots to waiters. The caller must hold g.mu.
func (g *gate) wakeLocked() {
	for g.active < g.limit && len(g.waiters) > 0 {
		ch := g.waiters[0]
		g.waiters = g.waiters[1:]
		g.active++
		close(ch)
	}
}
