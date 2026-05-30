package upstream

import (
	"sync"
	"time"
)

// rateLimiter is a per-tenant token bucket on upstream fetch rate.
// Default refill is PKGMIRROR_UPSTREAM_FETCH_RPM_PER_TENANT requests
// per minute. The bucket capacity equals one minute of refill so brief
// bursts get through but a runaway loop is bounded.
type rateLimiter struct {
	mu       sync.Mutex
	buckets  map[int64]*bucket
	rpm      int
	capacity int
}

type bucket struct {
	tokens     float64
	lastRefill time.Time
}

func newRateLimiter(rpm int) *rateLimiter {
	if rpm <= 0 {
		rpm = 60 // safe default
	}
	return &rateLimiter{
		buckets:  make(map[int64]*bucket),
		rpm:      rpm,
		capacity: rpm,
	}
}

// Allow returns true if tenantID may make a fetch right now, deducting
// one token. When false the caller should return ErrUpstreamRateLimit.
func (r *rateLimiter) Allow(tenantID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	b, ok := r.buckets[tenantID]
	if !ok {
		b = &bucket{tokens: float64(r.capacity), lastRefill: now}
		r.buckets[tenantID] = b
	}
	// Refill.
	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens += elapsed * float64(r.rpm) / 60.0
	if b.tokens > float64(r.capacity) {
		b.tokens = float64(r.capacity)
	}
	b.lastRefill = now
	if b.tokens < 1.0 {
		return false
	}
	b.tokens--
	return true
}
