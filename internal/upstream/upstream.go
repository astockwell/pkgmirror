// Package upstream implements JIT pull-through caching: per-tenant,
// per-format fetching of packages from canonical public registries
// (or operator-configured private mirrors) when a local lookup misses.
//
// See plans/upstream-pull-through.md for the architecture + threat
// model. The load-bearing security properties live here:
//
//   - hostname allowlist gate (compiled in + env-extensible, NOT
//     editable from any user-facing UI)
//   - private-IP / loopback / link-local rejection in the dialer
//   - per-fetch size cap + timeout
//   - single-flight de-dup per (tenant, format, canonical_key)
//
// This package is format-agnostic; per-format adapters live alongside
// each format handler (internal/packages/<format>/upstream.go).
package upstream

import (
	"context"
	"errors"
	"io"
	"net/url"
	"time"
)

// Mode controls a (tenant, format)'s pull-through behavior. See
// plans/upstream-pull-through.md S1 for semantics.
type Mode string

const (
	// ModeOff disables pull-through. Misses 404 immediately.
	ModeOff Mode = "off"
	// ModeCacheAndServe is the default. Fetch on miss, persist, serve.
	ModeCacheAndServe Mode = "cache_and_serve"
	// ModeCacheOnly fetches on miss and persists as quarantined.
	// Operator must promote before serving.
	ModeCacheOnly Mode = "cache_only"
)

// Valid reports whether m is one of the recognized modes.
func (m Mode) Valid() bool {
	switch m {
	case ModeOff, ModeCacheAndServe, ModeCacheOnly:
		return true
	}
	return false
}

// Kind tells the fetcher whether the resource is mutable metadata
// (cache with short TTL, revalidate with ETag) or an immutable blob
// (cache forever via the existing blob store).
type Kind int

const (
	// KindMetadata is a mutable index, packument, repomd.xml, etc.
	KindMetadata Kind = iota
	// KindBlob is an immutable artifact: .whl, .tgz, .gem, .rpm, etc.
	KindBlob
)

// Request is the input to Fetcher.Fetch. Per-format adapters build it
// from an incoming gin request and pass it in.
type Request struct {
	// TenantID identifies which tenant's upstream config to use.
	TenantID int64

	// Format is the lowercase format name ("pypi", "npm", ...).
	Format string

	// Kind controls the cache strategy (metadata vs blob).
	Kind Kind

	// UpstreamPath is the path (or full URL) the per-format adapter
	// resolved. If absolute, it must point at a host in the configured
	// allowlist. If relative, it's joined with the per-tenant
	// upstream_url (which itself must be allowlisted).
	UpstreamPath string

	// CanonicalKey uniquely identifies this resource for single-flight
	// de-duplication and for logging. Per-format adapters compose it.
	// Examples:
	//   "pypi:default:requests:simple"             (per-package index)
	//   "pypi:default:requests:2.32.4:requests-2.32.4-py3-none-any.whl"
	//   "npm:default:lodash:packument"
	CanonicalKey string

	// IfNoneMatch / IfModifiedSince are forwarded to upstream when set,
	// enabling 304 revalidation on metadata GETs.
	IfNoneMatch     string
	IfModifiedSince time.Time
}

// Result is what Fetcher.Fetch returns on a successful upstream contact.
// Caller MUST Close Body when done.
type Result struct {
	// Body is the streamed upstream response. May be a tee'd reader
	// that writes to a backing buffer as the caller reads.
	Body io.ReadCloser

	// ContentType, ContentLength, ETag, LastModified come from upstream
	// for metadata responses; ContentLength is -1 if unknown.
	ContentType   string
	ContentLength int64
	ETag          string
	LastModified  time.Time

	// UpstreamURL is the fully-resolved URL the fetcher contacted. Used
	// in audit log + slog. May be nil for FromCache=true results.
	UpstreamURL *url.URL

	// UpstreamPublishedUnix carries the upstream's stated publish time
	// when the per-format adapter can extract it cheaply (or 0 if not).
	// Some formats require a second hop (PyPI uses the Warehouse JSON
	// API). The adapter decides whether to pay that cost.
	UpstreamPublishedUnix int64

	// FromCache is true for metadata served from the in-memory cache.
	// Caller skips persist in that case.
	FromCache bool

	// NotModified is true when this is a 304 response to the caller's
	// conditional GET. Body is empty; caller should serve from local
	// metadata cache.
	NotModified bool
}

// Sentinel errors. Callers MUST use errors.Is to distinguish.
var (
	// ErrUpstreamOff means the (tenant, format) has Mode = off. The
	// per-format handler should 404 as if pull-through did not exist.
	ErrUpstreamOff = errors.New("upstream pull-through disabled")

	// ErrUpstreamNotFound means upstream returned 404. The per-format
	// handler should also 404; do NOT retry.
	ErrUpstreamNotFound = errors.New("upstream returned 404")

	// ErrUpstreamRateLimit is local rate-limit (per-tenant). Returned as
	// 429 to the client with Retry-After.
	ErrUpstreamRateLimit = errors.New("local upstream-fetch rate limit tripped")

	// ErrUpstreamTooLarge means the upstream response exceeded the
	// configured per-fetch byte cap. Body is closed; client gets 502.
	ErrUpstreamTooLarge = errors.New("upstream response exceeded max size")

	// ErrUpstreamForbidden means the resolved upstream URL hit the
	// allowlist or private-IP gate. Boot-time config bug; not a runtime
	// error operators see in production.
	ErrUpstreamForbidden = errors.New("upstream host blocked by allowlist or private-IP guard")

	// ErrUpstreamTimeout means the upstream connect/transfer exceeded
	// the configured timeout.
	ErrUpstreamTimeout = errors.New("upstream fetch timed out")

	// ErrUpstreamUpstream is a generic upstream-side failure (5xx,
	// invalid response, etc.). Distinct from ErrUpstreamNotFound so
	// callers can choose whether to surface as 502 vs 404.
	ErrUpstreamUpstream = errors.New("upstream returned an error")
)

// Fetcher is the surface per-format handlers call from their miss path.
// One instance per pkgmirror process; constructed by New (in fetcher.go).
type Fetcher interface {
	// Fetch resolves the (tenant, format) config, applies the
	// allowlist + rate-limit + size guards, and returns a streaming
	// body from upstream. The caller is responsible for persisting +
	// running the policy engine + emitting the audit row.
	Fetch(ctx context.Context, req Request) (*Result, error)

	// ResolveConfig returns the effective (Mode, upstream URL, metadata
	// TTL) for (tenant, format). Useful for handlers that need to make
	// decisions before deciding to call Fetch (e.g. early-out on
	// ModeOff to avoid building a Request struct).
	ResolveConfig(ctx context.Context, tenantID int64, format string) (TenantConfig, error)
}

// TenantConfig is the resolved-per-request configuration for a
// (tenant, format) pair. Comes from tenant_upstreams row if present,
// or from defaults.go if absent.
type TenantConfig struct {
	Mode            Mode
	UpstreamBaseURL *url.URL
	MetadataTTL     time.Duration
	// AuthKind + AuthCredential populated in PR Q.
}
