package upstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"
)

// fetcherImpl is the concrete Fetcher. One per pkgmirror process; safe
// for concurrent use.
type fetcherImpl struct {
	cfg       Config
	store     ConfigStore
	allowlist *Allowlist
	guard     *PrivateIPGuard
	client    *http.Client
	limiter   *rateLimiter
	cache     *metadataCache
	sf        singleflight.Group
}

// New builds a Fetcher from cfg and the persisted-config store. The
// store may be nil in unit tests; the resolver then returns
// defaults-only.
func New(cfg Config, store ConfigStore) Fetcher {
	al := NewAllowlist(cfg.AllowedHostsExtra)
	guard := NewPrivateIPGuard(cfg.AllowPrivateIPs)
	client := newHTTPClient(httpClientConfig{
		allowlist:       al,
		privateGuard:    guard,
		allowPlaintext:  cfg.AllowPlaintext,
		connectTimeout:  cfg.ConnectTimeout,
		overallTimeout:  cfg.FetchTimeout,
		idleConnTimeout: 90 * time.Second,
		maxIdleConns:    32,
		maxConnsPerHost: 8,
		userAgent:       cfg.UserAgent,
	})
	return &fetcherImpl{
		cfg:       cfg,
		store:     store,
		allowlist: al,
		guard:     guard,
		client:    client,
		limiter:   newRateLimiter(cfg.FetchRPMPerTenant),
		cache:     newMetadataCache(cfg.MetadataCacheMaxBytes),
	}
}

// ResolveConfig resolves (tenant, format) -> TenantConfig. Pure
// pass-through to the package-level resolver; exposed for handlers that
// want to bail early on ModeOff.
func (f *fetcherImpl) ResolveConfig(ctx context.Context, tenantID int64, format string) (TenantConfig, error) {
	return resolveConfig(ctx, f.store, f.cfg.DefaultMode, f.cfg.DefaultMetadataTTL, tenantID, format)
}

// Fetch is the main entry point for per-format adapters. It enforces:
//   - Mode != off
//   - URL scheme + host allowlist
//   - Rate limit
//   - Single-flight de-dup
//   - For metadata: in-memory cache lookup + ETag/304 revalidation
//   - For blob: streamed body with size cap
//
// Caller MUST Close the returned Result.Body when done (or check
// NotModified/FromCache and act accordingly).
func (f *fetcherImpl) Fetch(ctx context.Context, req Request) (*Result, error) {
	cfg, err := f.ResolveConfig(ctx, req.TenantID, req.Format)
	if err != nil {
		return nil, fmt.Errorf("resolving config: %w", err)
	}
	if cfg.Mode == ModeOff {
		return nil, ErrUpstreamOff
	}

	// Build absolute URL.
	target, err := f.resolveURL(req, cfg)
	if err != nil {
		return nil, err
	}
	if err := checkSchemeAndHost(target, f.allowlist, f.cfg.AllowPlaintext); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstreamForbidden, err)
	}

	// Metadata cache fast path.
	if req.Kind == KindMetadata && req.CanonicalKey != "" {
		if e, ok := f.cache.get(req.CanonicalKey); ok {
			return resultFromCache(e, target.String()), nil
		}
	}

	// Rate limit (after cache hit so cached responses are free).
	if !f.limiter.Allow(req.TenantID) {
		return nil, ErrUpstreamRateLimit
	}

	// Single-flight de-dup. Keyed on the canonical key so identical
	// concurrent misses collapse to one upstream RTT.
	sfKey := fmt.Sprintf("%d|%s|%s", req.TenantID, req.Format, req.CanonicalKey)
	v, err, _ := f.sf.Do(sfKey, func() (any, error) {
		return f.doFetch(ctx, req, cfg, target)
	})
	if err != nil {
		return nil, err
	}
	res := v.(*Result)
	// When sf collapsed multiple callers onto one Result for a metadata
	// fetch, we served the body bytes from the cache slice and they're
	// safe to re-read. For blobs we never sf-share the body (see below),
	// so this is only reached for cached/304 responses.
	return res, nil
}

// resolveURL joins the per-tenant base URL with the request path. When
// UpstreamPath is already absolute the per-tenant base is ignored
// (adapters use this for cross-host blob URLs like
// files.pythonhosted.org). Either way the result goes through the
// scheme + allowlist gate before any I/O.
func (f *fetcherImpl) resolveURL(req Request, cfg TenantConfig) (*url.URL, error) {
	if strings.HasPrefix(req.UpstreamPath, "http://") || strings.HasPrefix(req.UpstreamPath, "https://") {
		u, err := url.Parse(req.UpstreamPath)
		if err != nil {
			return nil, fmt.Errorf("%w: parse: %v", ErrUpstreamForbidden, err)
		}
		return u, nil
	}
	if cfg.UpstreamBaseURL == nil {
		return nil, fmt.Errorf("%w: no upstream base url for format %s", ErrUpstreamForbidden, req.Format)
	}
	rel, err := url.Parse(req.UpstreamPath)
	if err != nil {
		return nil, fmt.Errorf("%w: parse path: %v", ErrUpstreamForbidden, err)
	}
	return cfg.UpstreamBaseURL.ResolveReference(rel), nil
}

// doFetch is the actual HTTP round-trip. Body for blobs is wrapped in
// a size-capped reader; metadata bodies are read in full so we can
// cache them (typical metadata is small; large packuments hit the
// MaxBytesPerFetch cap and fail closed).
func (f *fetcherImpl) doFetch(ctx context.Context, req Request, cfg TenantConfig, target *url.URL) (*Result, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrUpstreamUpstream, err)
	}
	httpReq.Header.Set("Accept-Encoding", "identity") // simpler hash-on-the-fly later
	if req.IfNoneMatch != "" {
		httpReq.Header.Set("If-None-Match", req.IfNoneMatch)
	}
	if !req.IfModifiedSince.IsZero() {
		httpReq.Header.Set("If-Modified-Since", req.IfModifiedSince.UTC().Format(http.TimeFormat))
	}

	resp, err := f.client.Do(httpReq)
	if err != nil {
		// Map common transport errors to our sentinels for cleaner
		// caller branching.
		if errors.Is(err, ErrUpstreamForbidden) {
			return nil, fmt.Errorf("%w: %v", ErrUpstreamForbidden, err)
		}
		if isTimeoutErr(err) {
			return nil, fmt.Errorf("%w: %v", ErrUpstreamTimeout, err)
		}
		return nil, fmt.Errorf("%w: %v", ErrUpstreamUpstream, err)
	}

	switch {
	case resp.StatusCode == http.StatusNotModified:
		// Refresh TTL and serve from cache.
		_ = resp.Body.Close()
		if req.Kind == KindMetadata && req.CanonicalKey != "" {
			if f.cache.refresh(req.CanonicalKey, cfg.MetadataTTL) {
				if e, ok := f.cache.get(req.CanonicalKey); ok {
					r := resultFromCache(e, target.String())
					r.NotModified = false // serve full body; client got fresh-cache
					return r, nil
				}
			}
		}
		// No cached body to serve - synthesize an empty NotModified.
		return &Result{
			Body:        io.NopCloser(bytes.NewReader(nil)),
			NotModified: true,
			UpstreamURL: target,
		}, nil

	case resp.StatusCode == http.StatusNotFound:
		_ = resp.Body.Close()
		return nil, ErrUpstreamNotFound

	case resp.StatusCode == http.StatusForbidden:
		_ = resp.Body.Close()
		return nil, ErrUpstreamForbidden

	case resp.StatusCode == http.StatusTooManyRequests:
		_ = resp.Body.Close()
		return nil, ErrUpstreamRateLimit

	case resp.StatusCode >= 500 || resp.StatusCode >= 400:
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: status %d", ErrUpstreamUpstream, resp.StatusCode)
	}

	result := &Result{
		ContentType:   resp.Header.Get("Content-Type"),
		ContentLength: resp.ContentLength,
		ETag:          resp.Header.Get("ETag"),
		UpstreamURL:   target,
	}
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		if t, err := http.ParseTime(lm); err == nil {
			result.LastModified = t
		}
	}

	// Enforce size cap before reading.
	if f.cfg.MaxBytesPerFetch > 0 && result.ContentLength > f.cfg.MaxBytesPerFetch {
		_ = resp.Body.Close()
		return nil, ErrUpstreamTooLarge
	}

	if req.Kind == KindMetadata {
		// Read in full, cache, and return a re-readable result.
		limited := io.LimitReader(resp.Body, f.cfg.MaxBytesPerFetch+1)
		body, readErr := io.ReadAll(limited)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("%w: read body: %v", ErrUpstreamUpstream, readErr)
		}
		if int64(len(body)) > f.cfg.MaxBytesPerFetch {
			return nil, ErrUpstreamTooLarge
		}
		if req.CanonicalKey != "" {
			f.cache.put(req.CanonicalKey, body, result.ContentType, result.ETag, result.LastModified, cfg.MetadataTTL)
		}
		result.Body = io.NopCloser(bytes.NewReader(body))
		result.ContentLength = int64(len(body))
		return result, nil
	}

	// Blob: caller streams + computes hash. Wrap with size-capped reader
	// so a runaway upstream can't blow our heap or local disk.
	result.Body = newCappedReadCloser(resp.Body, f.cfg.MaxBytesPerFetch)
	return result, nil
}

// isTimeoutErr identifies context-deadline + net.Error timeouts.
func isTimeoutErr(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	type timeouter interface{ Timeout() bool }
	var t timeouter
	if errors.As(err, &t) {
		return t.Timeout()
	}
	return false
}

// cappedReadCloser wraps a ReadCloser with a hard byte cap. Read
// returns ErrUpstreamTooLarge when the cap is exceeded.
type cappedReadCloser struct {
	rc       io.ReadCloser
	max      int64
	read     int64
	tripped  bool
}

func newCappedReadCloser(rc io.ReadCloser, max int64) io.ReadCloser {
	return &cappedReadCloser{rc: rc, max: max}
}

func (c *cappedReadCloser) Read(p []byte) (int, error) {
	if c.tripped {
		return 0, ErrUpstreamTooLarge
	}
	if c.max > 0 {
		remaining := c.max - c.read
		if remaining <= 0 {
			c.tripped = true
			return 0, ErrUpstreamTooLarge
		}
		if int64(len(p)) > remaining {
			p = p[:remaining]
		}
	}
	n, err := c.rc.Read(p)
	c.read += int64(n)
	if c.max > 0 && c.read >= c.max {
		// Probe one more byte; if upstream has more, fail.
		var probe [1]byte
		extra, _ := c.rc.Read(probe[:])
		if extra > 0 {
			c.tripped = true
			return n, ErrUpstreamTooLarge
		}
	}
	return n, err
}

func (c *cappedReadCloser) Close() error { return c.rc.Close() }
