// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2016 The Gogs Authors.
// SPDX-License-Identifier: MIT
//
// Ported from forgejo/modules/sync/exclusive_pool.go (MIT), itself
// originally from Gogs. The refcount-based map-of-mutexes pattern is
// preserved verbatim so callers can model on either codebase.

// Package syncutil provides concurrency primitives used by multiple
// package format handlers. We keep them out of the per-format packages
// because more than one format (Maven today, Debian / NuGet on the
// roadmap) needs the same per-key serialization to handle multi-file
// publishes against a shared coordinate.
package syncutil

import "sync"

// ExclusivePool serializes work by a string key. Concurrent calls to
// CheckIn with the same identity block one another; calls with
// different identities don't.
//
// Memory is bounded by the number of *concurrently-active* identities,
// not the number of identities ever seen — the per-key mutex is
// removed from the map when the last holder checks out. A naive
// sync.Map[string]*sync.Mutex grows monotonically and is the wrong
// pattern for long-running registries with high coordinate
// cardinality.
//
// Zero value is unusable; call NewExclusivePool.
type ExclusivePool struct {
	// lock guards pool + count. We hold it across map mutations only,
	// never across the per-key Lock/Unlock — so a slow CheckIn for
	// identity A never blocks CheckIn for identity B.
	lock sync.Mutex

	// pool holds the live per-key mutexes.
	pool map[string]*sync.Mutex

	// count tracks how many goroutines are waiting on (or holding)
	// each key. When count drops to zero we delete the map entry so
	// the mutex can be garbage-collected.
	count map[string]int
}

// NewExclusivePool returns an empty pool.
func NewExclusivePool() *ExclusivePool {
	return &ExclusivePool{
		pool:  make(map[string]*sync.Mutex),
		count: make(map[string]int),
	}
}

// CheckIn acquires the per-identity lock, creating one if necessary.
// Blocks while another goroutine holds the same identity. Always pair
// with a deferred CheckOut.
func (p *ExclusivePool) CheckIn(identity string) {
	p.lock.Lock()
	mu, ok := p.pool[identity]
	if !ok {
		mu = &sync.Mutex{}
		p.pool[identity] = mu
	}
	p.count[identity]++
	p.lock.Unlock()
	mu.Lock()
}

// CheckOut releases the per-identity lock and, if no other goroutine
// is waiting on the same identity, removes the map entry so the mutex
// can be collected. Calling CheckOut without a matching CheckIn
// panics (as a result of unlocking an unlocked mutex).
func (p *ExclusivePool) CheckOut(identity string) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.pool[identity].Unlock()
	if p.count[identity] == 1 {
		delete(p.pool, identity)
		delete(p.count, identity)
		return
	}
	p.count[identity]--
}

// Size returns the number of identities currently held or waited on.
// Provided for diagnostics + the test. Not exported to handlers.
func (p *ExclusivePool) Size() int {
	p.lock.Lock()
	defer p.lock.Unlock()
	return len(p.pool)
}
