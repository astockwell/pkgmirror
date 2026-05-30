package upstream

import (
	"context"
	"database/sql"
	"net/url"
	"time"
)

// ConfigStore is the read-only surface the fetcher uses to look up
// per-(tenant, format) settings. internal/db implements it; tests can
// pass a fake to avoid spinning a SQLite DB.
type ConfigStore interface {
	// LookupTenantUpstream returns the persisted config row for
	// (tenantID, format). When no row exists, returns (zero, nil, nil)
	// and the fetcher falls back to compiled-in defaults.
	LookupTenantUpstream(ctx context.Context, tenantID int64, format string) (*PersistedConfig, error)
}

// PersistedConfig mirrors the tenant_upstreams row (see internal/db
// migration v5). Fields are pointers/strings so the resolver can tell
// "row absent" from "row with NULL column".
type PersistedConfig struct {
	Mode             string         // 'off' | 'cache_and_serve' | 'cache_only'
	UpstreamURL      sql.NullString // NULL = use default
	MetadataTTLSec   int64
	AuthKind         sql.NullString
	AuthCredential   sql.NullString // encrypted; not used until PR Q
	UpdatedUnix      int64
}

// resolveConfig is the shared resolution logic used by Fetch and by
// the exported ResolveConfig. It applies fallbacks: row->mode, then
// compiled-in default URL when the row's upstream_url is NULL.
func resolveConfig(ctx context.Context, store ConfigStore, defaultMode Mode, defaultTTL time.Duration, tenantID int64, format string) (TenantConfig, error) {
	def, hasDefault := LookupDefault(format)

	var persisted *PersistedConfig
	if store != nil {
		p, err := store.LookupTenantUpstream(ctx, tenantID, format)
		if err != nil {
			return TenantConfig{}, err
		}
		persisted = p
	}

	cfg := TenantConfig{
		Mode:        defaultMode,
		MetadataTTL: defaultTTL,
	}

	// Default URL: only meaningful when format has a compiled-in default.
	if hasDefault {
		u, err := url.Parse(def.URL)
		if err == nil {
			cfg.UpstreamBaseURL = u
		}
		if !def.PullThroughSupported {
			// Format known but adapter not shipped yet; treat as off so
			// the per-format handler doesn't try to call us.
			cfg.Mode = ModeOff
		}
	} else {
		// Format has no canonical upstream (generic, container, ...).
		cfg.Mode = ModeOff
	}

	if persisted != nil {
		// Per-tenant override.
		m := Mode(persisted.Mode)
		if m.Valid() {
			cfg.Mode = m
		}
		if persisted.UpstreamURL.Valid && persisted.UpstreamURL.String != "" {
			if u, err := url.Parse(persisted.UpstreamURL.String); err == nil {
				cfg.UpstreamBaseURL = u
			}
		}
		if persisted.MetadataTTLSec > 0 {
			cfg.MetadataTTL = time.Duration(persisted.MetadataTTLSec) * time.Second
		}
	}

	return cfg, nil
}
