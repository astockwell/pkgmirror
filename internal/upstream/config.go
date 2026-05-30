package upstream

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the boot-time configuration consumed by New. Operators
// populate it from environment via LoadConfigFromEnv. Tests can build
// it directly to exercise edge cases.
type Config struct {
	// DefaultMode is the mode used when a tenant has no row in
	// tenant_upstreams. Defaults to cache_and_serve - on by default
	// per user decision in plans/upstream-pull-through.md S11.
	DefaultMode Mode

	// AllowedHostsExtra extends the compiled-in allowlist. Cannot be
	// shrunk; cannot be edited from the web console.
	AllowedHostsExtra []string

	// AllowPrivateIPs disables the SSRF private-range guard. Required
	// for intermediate-mirror deployments (pkgmirror -> another
	// pkgmirror on internal network).
	AllowPrivateIPs bool

	// AllowPlaintext permits http:// upstreams (and http:// redirects).
	// Off by default. Even with this on, the allowlist still applies.
	AllowPlaintext bool

	// FetchTimeout is the overall per-fetch deadline (connect + body).
	FetchTimeout time.Duration

	// ConnectTimeout is the dial + TLS-handshake deadline.
	ConnectTimeout time.Duration

	// MaxBytesPerFetch caps any single upstream response body. Beyond
	// this the fetcher returns ErrUpstreamTooLarge.
	MaxBytesPerFetch int64

	// FetchRPMPerTenant is the per-tenant token-bucket rate cap.
	FetchRPMPerTenant int

	// UserAgent identifies pkgmirror to upstream operators.
	UserAgent string

	// MetadataCacheMaxBytes caps the in-process metadata cache. 0
	// disables caching (debug only - kills upstream throughput).
	MetadataCacheMaxBytes int

	// DefaultMetadataTTL is used when a tenant_upstreams row has 0
	// for metadata_ttl_sec (i.e. fresh row from the migration).
	DefaultMetadataTTL time.Duration
}

// DefaultConfig returns conservative defaults that pass review:
// HTTPS-only, 30s overall / 5s connect, 100MB per-fetch, 600 req/min
// per tenant, 64MB metadata cache, 5min TTL. Tighter for prod via env.
func DefaultConfig() Config {
	return Config{
		DefaultMode:           ModeCacheAndServe,
		AllowedHostsExtra:     nil,
		AllowPrivateIPs:       false,
		AllowPlaintext:        false,
		FetchTimeout:          30 * time.Second,
		ConnectTimeout:        5 * time.Second,
		MaxBytesPerFetch:      100 * 1024 * 1024, // 100MB
		FetchRPMPerTenant:     600,
		UserAgent:             "pkgmirror/0.1 (+https://github.com/astockwell/pkgmirror)",
		MetadataCacheMaxBytes: 64 * 1024 * 1024, // 64MB
		DefaultMetadataTTL:    5 * time.Minute,
	}
}

// LoadConfigFromEnv builds a Config from PKGMIRROR_UPSTREAM_* env vars,
// falling back to DefaultConfig for anything unset. Unknown values are
// ignored and the default kept (we log to stderr in PR D's launcher
// integration).
//
// Env vars consumed:
//
//   PKGMIRROR_UPSTREAM_DEFAULT_MODE
//       'off' | 'cache_and_serve' | 'cache_only' (case-insensitive)
//   PKGMIRROR_UPSTREAM_ALLOWED_HOSTS
//       comma-separated extra hostnames added to the compiled-in
//       allowlist
//   PKGMIRROR_UPSTREAM_ALLOW_PRIVATE_IPS
//       'true' to permit RFC1918/loopback/link-local targets
//   PKGMIRROR_UPSTREAM_ALLOW_PLAINTEXT
//       'true' to permit http://
//   PKGMIRROR_UPSTREAM_FETCH_TIMEOUT          (e.g. '30s', '2m')
//   PKGMIRROR_UPSTREAM_CONNECT_TIMEOUT        (e.g. '5s')
//   PKGMIRROR_UPSTREAM_MAX_BYTES_PER_FETCH    (decimal bytes)
//   PKGMIRROR_UPSTREAM_FETCH_RPM_PER_TENANT   (positive int)
//   PKGMIRROR_UPSTREAM_USER_AGENT
//   PKGMIRROR_UPSTREAM_METADATA_CACHE_MAX_BYTES
//   PKGMIRROR_UPSTREAM_DEFAULT_METADATA_TTL   (duration)
func LoadConfigFromEnv() Config {
	cfg := DefaultConfig()

	if v := strings.ToLower(strings.TrimSpace(os.Getenv("PKGMIRROR_UPSTREAM_DEFAULT_MODE"))); v != "" {
		m := Mode(v)
		if m.Valid() {
			cfg.DefaultMode = m
		}
	}
	if v := os.Getenv("PKGMIRROR_UPSTREAM_ALLOWED_HOSTS"); v != "" {
		for _, h := range strings.Split(v, ",") {
			h = strings.TrimSpace(h)
			if h != "" {
				cfg.AllowedHostsExtra = append(cfg.AllowedHostsExtra, h)
			}
		}
	}
	cfg.AllowPrivateIPs = envBool("PKGMIRROR_UPSTREAM_ALLOW_PRIVATE_IPS", cfg.AllowPrivateIPs)
	cfg.AllowPlaintext = envBool("PKGMIRROR_UPSTREAM_ALLOW_PLAINTEXT", cfg.AllowPlaintext)
	cfg.FetchTimeout = envDuration("PKGMIRROR_UPSTREAM_FETCH_TIMEOUT", cfg.FetchTimeout)
	cfg.ConnectTimeout = envDuration("PKGMIRROR_UPSTREAM_CONNECT_TIMEOUT", cfg.ConnectTimeout)
	cfg.MaxBytesPerFetch = envInt64("PKGMIRROR_UPSTREAM_MAX_BYTES_PER_FETCH", cfg.MaxBytesPerFetch)
	cfg.FetchRPMPerTenant = envInt("PKGMIRROR_UPSTREAM_FETCH_RPM_PER_TENANT", cfg.FetchRPMPerTenant)
	if v := strings.TrimSpace(os.Getenv("PKGMIRROR_UPSTREAM_USER_AGENT")); v != "" {
		cfg.UserAgent = v
	}
	cfg.MetadataCacheMaxBytes = envInt("PKGMIRROR_UPSTREAM_METADATA_CACHE_MAX_BYTES", cfg.MetadataCacheMaxBytes)
	cfg.DefaultMetadataTTL = envDuration("PKGMIRROR_UPSTREAM_DEFAULT_METADATA_TTL", cfg.DefaultMetadataTTL)

	return cfg
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "":
		return def
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	}
	return def
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func envInt64(key string, def int64) int64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func envDuration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}
