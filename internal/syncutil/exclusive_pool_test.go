// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// SPDX-License-Identifier: MIT

package syncutil

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestExclusivePool_SerializesSameIdentity is the load-bearing
// invariant: two CheckIns with the same identity cannot run
// concurrently.
func TestExclusivePool_SerializesSameIdentity(t *testing.T) {
	p := NewExclusivePool()
	var inCritical int32
	var maxConcurrent int32

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			p.CheckIn("same")
			defer p.CheckOut("same")
			n := atomic.AddInt32(&inCritical, 1)
			defer atomic.AddInt32(&inCritical, -1)
			for {
				m := atomic.LoadInt32(&maxConcurrent)
				if n <= m || atomic.CompareAndSwapInt32(&maxConcurrent, m, n) {
					break
				}
			}
			// Hold the lock briefly so any race window is wide
			// enough to be observable.
			time.Sleep(1 * time.Millisecond)
		}()
	}
	wg.Wait()
	if maxConcurrent != 1 {
		t.Fatalf("expected at most 1 concurrent holder, observed %d", maxConcurrent)
	}
}

// TestExclusivePool_AllowsDifferentIdentities verifies that distinct
// identities don't block each other.
func TestExclusivePool_AllowsDifferentIdentities(t *testing.T) {
	p := NewExclusivePool()
	p.CheckIn("a")
	defer p.CheckOut("a")
	// If the pool were a coarse-grained mutex this would block forever.
	done := make(chan struct{})
	go func() {
		p.CheckIn("b")
		p.CheckOut("b")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("CheckIn on a different identity blocked")
	}
}

// TestExclusivePool_FreesMapEntriesOnCheckOut is the reason for
// porting from forgejo: a naive sync.Map of mutexes grows forever.
// After CheckIn+CheckOut on N distinct identities, the pool's
// underlying map must be empty.
func TestExclusivePool_FreesMapEntriesOnCheckOut(t *testing.T) {
	p := NewExclusivePool()
	for i := 0; i < 1000; i++ {
		key := "ident-" + itoa(i)
		p.CheckIn(key)
		p.CheckOut(key)
	}
	if got := p.Size(); got != 0 {
		t.Fatalf("expected empty pool after all checkouts, got Size()=%d", got)
	}
}

// TestExclusivePool_NestedCountCorrect verifies that two concurrent
// waiters on the same identity both run, and the map entry is freed
// only after the *last* one checks out.
func TestExclusivePool_NestedCountCorrect(t *testing.T) {
	p := NewExclusivePool()
	p.CheckIn("k")

	started := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		close(started)
		p.CheckIn("k")
		// Verify the map entry still exists (held by us now).
		if p.Size() != 1 {
			t.Errorf("size while held by second waiter: want 1 got %d", p.Size())
		}
		p.CheckOut("k")
		close(finished)
	}()
	<-started
	// Give the goroutine time to actually block on CheckIn.
	time.Sleep(20 * time.Millisecond)
	// Size should be 1 (one identity in the pool) but count==2.
	if got := p.Size(); got != 1 {
		t.Fatalf("size while held by first + waiting second: want 1 got %d", got)
	}
	p.CheckOut("k")
	<-finished
	if got := p.Size(); got != 0 {
		t.Fatalf("size after all checkouts: want 0 got %d", got)
	}
}

// Avoid pulling in strconv just for one int->string in tests.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
