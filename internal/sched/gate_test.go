package sched

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGateLimitsConcurrency(t *testing.T) {
	g := newGate(2)
	var live, peak int64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := g.Acquire(context.Background()); err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			cur := atomic.AddInt64(&live, 1)
			for {
				old := atomic.LoadInt64(&peak)
				if cur <= old || atomic.CompareAndSwapInt64(&peak, old, cur) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			atomic.AddInt64(&live, -1)
			g.Release()
		}()
	}
	wg.Wait()
	if p := atomic.LoadInt64(&peak); p > 2 {
		t.Fatalf("peak concurrency = %d, want <= 2", p)
	}
}

func TestGateAcquireRespectsContext(t *testing.T) {
	g := newGate(1)
	if err := g.Acquire(context.Background()); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := g.Acquire(ctx); err == nil {
		t.Fatal("second acquire succeeded while the gate was full")
	}
	g.Release()
	if err := g.Acquire(context.Background()); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}

func TestGateSetLimitReleasesWaiters(t *testing.T) {
	g := newGate(1)
	if err := g.Acquire(context.Background()); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		done <- g.Acquire(ctx)
	}()
	// Nobody released, but raising the limit must let the waiter through.
	time.Sleep(10 * time.Millisecond)
	g.SetLimit(3)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waiter got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("raising the limit did not wake the waiter")
	}
	if g.Limit() != 3 {
		t.Fatalf("limit = %d", g.Limit())
	}
}

func TestVMSourceIDRoundTrip(t *testing.T) {
	id := VMSourceID("abc123", 100)
	if id != "abc123_100" {
		t.Fatalf("VMSourceID = %q", id)
	}
	vmid, err := vmidFromSourceID(id)
	if err != nil || vmid != 100 {
		t.Fatalf("vmidFromSourceID = %d, %v", vmid, err)
	}
	if _, err := vmidFromSourceID("no-separator"); err == nil {
		t.Fatal("vmidFromSourceID accepted a malformed source id")
	}
}
