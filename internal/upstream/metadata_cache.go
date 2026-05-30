package upstream

import (
	"bytes"
	"io"
	"sync"
	"time"
)

// cacheEntry is one metadata response held in memory. Body is the full
// fetched bytes; ETag/LastModified are forwarded as If-None-Match /
// If-Modified-Since on revalidation. expires is the wall-clock time
// after which a fresh fetch is required (TTL from TenantConfig).
type cacheEntry struct {
	body         []byte
	contentType  string
	etag         string
	lastModified time.Time
	expires      time.Time
	sizeBytes    int
}

// metadataCache is a tiny LRU keyed on CanonicalKey. Bounded by total
// bytes (PKGMIRROR_UPSTREAM_METADATA_CACHE_MAX_BYTES). When a fetch
// would exceed the budget, the oldest-accessed entries are evicted.
//
// Storing metadata bodies in process memory is intentional: PEP 503
// index pages for typical packages are 1-50KB; npm packuments rarely
// exceed 500KB; the cap stops the unbounded case (a package with
// thousands of versions). Cold-start cost is one upstream RTT.
type metadataCache struct {
	mu        sync.Mutex
	entries   map[string]*cacheEntry
	order     []string // simple LRU; rebuilt on each touch
	maxBytes  int
	bytesUsed int
}

// newMetadataCache constructs an empty cache with the given byte budget.
// maxBytes <= 0 disables caching (every Get is a miss, Put is no-op).
func newMetadataCache(maxBytes int) *metadataCache {
	return &metadataCache{
		entries:  make(map[string]*cacheEntry),
		maxBytes: maxBytes,
	}
}

// get returns the entry for key if present and not expired. Touches
// LRU order.
func (c *metadataCache) get(key string) (*cacheEntry, bool) {
	if c.maxBytes <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expires) {
		c.removeLocked(key)
		return nil, false
	}
	c.touchLocked(key)
	return e, true
}

// put stores body under key with the given TTL. Evicts oldest entries
// when over budget. body is copied; caller may reuse the slice.
func (c *metadataCache) put(key string, body []byte, contentType, etag string, lastModified time.Time, ttl time.Duration) {
	if c.maxBytes <= 0 || ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Drop any existing entry to keep bytesUsed accurate.
	c.removeLocked(key)
	if len(body) > c.maxBytes {
		// Single entry would blow the budget; skip caching.
		return
	}
	// Evict until we fit.
	for c.bytesUsed+len(body) > c.maxBytes && len(c.order) > 0 {
		c.removeLocked(c.order[0])
	}
	cp := make([]byte, len(body))
	copy(cp, body)
	e := &cacheEntry{
		body:         cp,
		contentType:  contentType,
		etag:         etag,
		lastModified: lastModified,
		expires:      time.Now().Add(ttl),
		sizeBytes:    len(cp),
	}
	c.entries[key] = e
	c.order = append(c.order, key)
	c.bytesUsed += e.sizeBytes
}

// refresh updates expiry on a 304 revalidation without rewriting body.
func (c *metadataCache) refresh(key string, ttl time.Duration) bool {
	if c.maxBytes <= 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return false
	}
	e.expires = time.Now().Add(ttl)
	c.touchLocked(key)
	return true
}

func (c *metadataCache) removeLocked(key string) {
	e, ok := c.entries[key]
	if !ok {
		return
	}
	delete(c.entries, key)
	c.bytesUsed -= e.sizeBytes
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}

func (c *metadataCache) touchLocked(key string) {
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			c.order = append(c.order, key)
			return
		}
	}
}

// resultFromCache builds a Result that reads from the cached body.
// The caller closes the body as normal.
func resultFromCache(e *cacheEntry, upstreamURL string) *Result {
	r := &Result{
		Body:          io.NopCloser(bytes.NewReader(e.body)),
		ContentType:   e.contentType,
		ContentLength: int64(e.sizeBytes),
		ETag:          e.etag,
		LastModified:  e.lastModified,
		FromCache:     true,
	}
	return r
}
