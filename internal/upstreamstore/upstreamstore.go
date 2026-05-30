// Package upstreamstore adapts a *sql.DB to the upstream.ConfigStore
// interface. Kept in a thin standalone package to avoid an
// internal/upstream -> internal/db dependency (db doesn't know about
// upstream; upstream doesn't know about db; this package wires them).
package upstreamstore

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/astockwell/pkgmirror/internal/upstream"
)

// SQL implements upstream.ConfigStore backed by SQLite via *sql.DB.
type SQL struct {
	db *sql.DB
}

// New wraps the given DB handle. The handle is borrowed (Close stays
// with the caller).
func New(db *sql.DB) *SQL { return &SQL{db: db} }

// LookupTenantUpstream reads the (tenantID, format) row from
// tenant_upstreams. Returns (nil, nil) when no row exists; the resolver
// then falls back to compiled-in defaults.
func (s *SQL) LookupTenantUpstream(ctx context.Context, tenantID int64, format string) (*upstream.PersistedConfig, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT mode, upstream_url, metadata_ttl_sec, auth_kind, auth_credential, updated_unix
		  FROM tenant_upstreams
		 WHERE tenant_id = ? AND format = ?
		 LIMIT 1`, tenantID, format)
	var pc upstream.PersistedConfig
	if err := row.Scan(&pc.Mode, &pc.UpstreamURL, &pc.MetadataTTLSec, &pc.AuthKind, &pc.AuthCredential, &pc.UpdatedUnix); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &pc, nil
}

// ListByTenant returns every persisted row for tenantID. The console
// admin page uses this to render the "current overrides" table; a
// format absent from the result means it falls through to compiled-in
// defaults.
func (s *SQL) ListByTenant(ctx context.Context, tenantID int64) (map[string]*upstream.PersistedConfig, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT format, mode, upstream_url, metadata_ttl_sec, auth_kind, auth_credential, updated_unix
		  FROM tenant_upstreams
		 WHERE tenant_id = ?`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]*upstream.PersistedConfig)
	for rows.Next() {
		var format string
		var pc upstream.PersistedConfig
		if err := rows.Scan(&format, &pc.Mode, &pc.UpstreamURL, &pc.MetadataTTLSec, &pc.AuthKind, &pc.AuthCredential, &pc.UpdatedUnix); err != nil {
			return nil, err
		}
		out[format] = &pc
	}
	return out, rows.Err()
}

// Set upserts the (tenantID, format) row. Use Delete to remove an
// override and fall back to compiled-in defaults.
//
// upstreamURL/metadataTTLSec may be zero values to keep them NULL/0.
// Caller is responsible for validation; this just persists.
func (s *SQL) Set(ctx context.Context, tenantID int64, format, mode, upstreamURL string, metadataTTLSec int64) error {
	var urlField sql.NullString
	if upstreamURL != "" {
		urlField = sql.NullString{Valid: true, String: upstreamURL}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tenant_upstreams (tenant_id, format, mode, upstream_url, metadata_ttl_sec, updated_unix)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(tenant_id, format) DO UPDATE SET
		    mode = excluded.mode,
		    upstream_url = excluded.upstream_url,
		    metadata_ttl_sec = excluded.metadata_ttl_sec,
		    updated_unix = excluded.updated_unix`,
		tenantID, format, mode, urlField, metadataTTLSec, time.Now().Unix())
	return err
}

// Delete removes the (tenantID, format) row so the resolver falls back
// to compiled-in defaults. No-op when no row exists.
func (s *SQL) Delete(ctx context.Context, tenantID int64, format string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM tenant_upstreams WHERE tenant_id = ? AND format = ?`,
		tenantID, format)
	return err
}

