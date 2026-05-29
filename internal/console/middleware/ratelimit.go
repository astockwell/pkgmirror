package middleware

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// LoginRateLimiter caps login attempts at N per window per client IP.
// Backed by an in-memory map; one process, one limiter. Fine at our
// scale (tens of operators). If we ever need per-cluster limits, swap
// for redis-rate or similar; the seam is just the Allow method.
//
// The plan (§11.2) calls for 10 attempts/min/IP. Spoofed
// X-Forwarded-For headers MUST NOT bypass the limit unless the source
// is in TrustedProxies — ClientIPFor enforces that.
type LoginRateLimiter struct {
	max    int
	window time.Duration

	mu      sync.Mutex
	buckets map[string]*ipBucket
}

type ipBucket struct {
	count    int
	firstHit time.Time
}

// NewLoginRateLimiter builds a limiter. max=10, window=time.Minute is
// the plan's recommended pairing.
func NewLoginRateLimiter(max int, window time.Duration) *LoginRateLimiter {
	if max <= 0 {
		max = 10
	}
	if window <= 0 {
		window = time.Minute
	}
	return &LoginRateLimiter{
		max:     max,
		window:  window,
		buckets: make(map[string]*ipBucket),
	}
}

// Allow returns true if the request is under the limit. It also
// records the hit, so calling Allow twice in a row charges twice.
//
// The bucket auto-resets when the window elapses since the bucket's
// first hit (a leaky-token-bucket-lite). Periodic full GC isn't worth
// it at console-scale operator counts; old IPs simply hang around in
// the map until restart.
func (r *LoginRateLimiter) Allow(ip string) bool {
	if ip == "" {
		// Empty IP -> can't bucket; allow but observe (caller logs).
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	b, ok := r.buckets[ip]
	if !ok || now.Sub(b.firstHit) > r.window {
		r.buckets[ip] = &ipBucket{count: 1, firstHit: now}
		return true
	}
	b.count++
	return b.count <= r.max
}

// Forget drops a bucket. Called after a successful login so an honest
// user who fat-fingered their password 9 times before getting it right
// doesn't get locked out by the 10th attempt's rate-limit pre-check
// from the previous failure-burst's bucket.
func (r *LoginRateLimiter) Forget(ip string) {
	if ip == "" {
		return
	}
	r.mu.Lock()
	delete(r.buckets, ip)
	r.mu.Unlock()
}

// ClientIPFor resolves the request's client IP, honoring
// X-Forwarded-For ONLY when the immediate peer is in trustedProxies.
// trustedProxies entries can be single IPs ("1.2.3.4") or CIDRs
// ("1.2.0.0/16"). Empty trustedProxies means we never honor
// X-Forwarded-For (the immediate peer wins always).
//
// This is the centralized resolver the plan §11.2 calls out; rate
// limiter + audit row + access log all funnel through it so a spoofed
// header at a non-trusted edge can't bypass either of them.
func ClientIPFor(r *http.Request, trustedProxies []string) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !ipInList(host, trustedProxies) {
		return host
	}
	// Trusted peer: walk X-Forwarded-For right-to-left, taking the
	// first entry that is NOT itself in the trusted list. (This is the
	// standard pattern for the L7 LB chain RFC 7239 alludes to.)
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return host
	}
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		p := strings.TrimSpace(parts[i])
		if p == "" {
			continue
		}
		if !ipInList(p, trustedProxies) {
			return p
		}
	}
	return host
}

func ipInList(ip string, list []string) bool {
	if ip == "" {
		return false
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, entry := range list {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			_, ipnet, err := net.ParseCIDR(entry)
			if err == nil && ipnet.Contains(parsed) {
				return true
			}
		} else if entry == ip {
			return true
		}
	}
	return false
}

// LoginRateLimitMiddleware is a gin middleware that 429s requests over
// the limit. Pass the trusted-proxies list so spoofed XFF can't bypass.
func LoginRateLimitMiddleware(limiter *LoginRateLimiter, trustedProxies []string) gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := ClientIPFor(c.Request, trustedProxies)
		if !limiter.Allow(ip) {
			c.Header("Retry-After", "60")
			c.Header("Content-Type", "text/plain; charset=utf-8")
			c.String(http.StatusTooManyRequests,
				"Too many sign-in attempts. Please wait a minute and try again.")
			c.Abort()
			return
		}
		c.Next()
	}
}
