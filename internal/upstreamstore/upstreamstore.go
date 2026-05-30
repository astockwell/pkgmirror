// Package upstreamstore adapts a *sql.DB to the upstream.ConfigStore
// interface. Kept in a thin standalone package to avoid an
// internal/upstream -> internal/db dependency (db doesn't know about
// upstream; upstream doesn't know about db; this package wires them).
package upstreamstore

import (
	"context"
	"database/sql"
	"errors"

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
