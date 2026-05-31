package db_test

import (
	"context"
	"path/filepath"
	"testing"

	pkgdb "github.com/astockwell/pkgmirror/internal/db"
)

// TestSchemaV5_TenantUpstreamsAndUpstreamPublishedUnix exercises the
// v5 migration directly. Opens a DB (which runs all migrations through
// v5), then asserts both new artifacts are present + writable.
//
// Per the macOS APFS cleanup convention in
// .github/copilot-instructions.md, the cleanup explicitly Closes the DB
// before t.TempDir's deferred os.RemoveAll runs.
func TestSchemaV5_TenantUpstreamsAndUpstreamPublishedUnix(t *testing.T) {
	dir := t.TempDir()
	db, err := pkgdb.Open(filepath.Join(dir, "v5.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()

	// PRAGMA user_version should be 6 after a fresh open.
	var ver int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&ver); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if ver != 6 {
		t.Errorf("user_version = %d, want 6", ver)
	}

	// tenant_upstreams: insert a row covering every column.
	_, err = db.ExecContext(ctx, `
		INSERT INTO tenant_upstreams
		  (tenant_id, format, mode, upstream_url, metadata_ttl_sec,
		   auth_kind, auth_credential, updated_unix)
		VALUES
		  (1, 'pypi', 'cache_and_serve', 'https://pypi.org', 300,
		   NULL, NULL, 1700000000)
	`)
	if err != nil {
		t.Fatalf("insert tenant_upstreams: %v", err)
	}

	// PK (tenant_id, format) means a second insert with same pair fails.
	_, err = db.ExecContext(ctx, `
		INSERT INTO tenant_upstreams (tenant_id, format, mode, updated_unix)
		VALUES (1, 'pypi', 'off', 1700000001)
	`)
	if err == nil {
		t.Error("expected duplicate (tenant_id, format) to be rejected")
	}

	// Different format on same tenant is fine.
	_, err = db.ExecContext(ctx, `
		INSERT INTO tenant_upstreams (tenant_id, format, mode, updated_unix)
		VALUES (1, 'npm', 'cache_only', 1700000002)
	`)
	if err != nil {
		t.Fatalf("insert second format: %v", err)
	}

	// Round-trip the row.
	var mode, url string
	var ttl int
	row := db.QueryRowContext(ctx, `
		SELECT mode, upstream_url, metadata_ttl_sec
		  FROM tenant_upstreams
		 WHERE tenant_id = 1 AND format = 'pypi'
	`)
	if err := row.Scan(&mode, &url, &ttl); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if mode != "cache_and_serve" {
		t.Errorf("mode = %q, want cache_and_serve", mode)
	}
	if url != "https://pypi.org" {
		t.Errorf("upstream_url = %q, want https://pypi.org", url)
	}
	if ttl != 300 {
		t.Errorf("metadata_ttl_sec = %d, want 300", ttl)
	}

	// package_versions.upstream_published_unix: seed a package + version
	// and write the new column. The other quarantine columns from v3
	// must still work too (regression check).
	_, err = db.ExecContext(ctx, `
		INSERT INTO packages (id, tenant_id, type, name, lower_name, created_unix)
		VALUES (1, 1, 'pypi', 'requests', 'requests', 1700000000)
	`)
	if err != nil {
		t.Fatalf("seed package: %v", err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO package_versions
		  (id, package_id, version, lower_version, metadata_json, created_unix,
		   upstream_published_unix)
		VALUES
		  (1, 1, '2.32.4', '2.32.4', '{}', 1700000010, 1700000005)
	`)
	if err != nil {
		t.Fatalf("insert version with upstream_published_unix: %v", err)
	}

	var pub int64
	row = db.QueryRowContext(ctx,
		`SELECT upstream_published_unix FROM package_versions WHERE id = 1`)
	if err := row.Scan(&pub); err != nil {
		t.Fatalf("scan upstream_published_unix: %v", err)
	}
	if pub != 1700000005 {
		t.Errorf("upstream_published_unix = %d, want 1700000005", pub)
	}

	// Existing rows (no upstream_published_unix set) should be NULL.
	// Verify by inserting without the column.
	_, err = db.ExecContext(ctx, `
		INSERT INTO package_versions
		  (id, package_id, version, lower_version, metadata_json, created_unix)
		VALUES
		  (2, 1, '2.32.5', '2.32.5', '{}', 1700000020)
	`)
	if err != nil {
		t.Fatalf("insert version without upstream_published_unix: %v", err)
	}
	var pub2nullable any
	row = db.QueryRowContext(ctx,
		`SELECT upstream_published_unix FROM package_versions WHERE id = 2`)
	if err := row.Scan(&pub2nullable); err != nil {
		t.Fatalf("scan nullable upstream_published_unix: %v", err)
	}
	if pub2nullable != nil {
		t.Errorf("expected NULL upstream_published_unix on locally-uploaded version, got %v", pub2nullable)
	}
}

// TestSchemaV5_TenantCascade verifies the foreign-key cascade: deleting
// a tenant removes its tenant_upstreams rows. Belt-and-braces because
// PRAGMA foreign_keys must be on (Open sets it) and CASCADE is in the
// schema.
func TestSchemaV5_TenantCascade(t *testing.T) {
	dir := t.TempDir()
	db, err := pkgdb.Open(filepath.Join(dir, "v5cascade.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()

	// The default tenant (id=1) is seeded by migration v2. Create another.
	_, err = db.ExecContext(ctx, `
		INSERT INTO tenants (id, name, lower_name, visibility, created_unix)
		VALUES (2, 'doomed', 'doomed', 0, 1700000000)
	`)
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO tenant_upstreams (tenant_id, format, mode, updated_unix)
		VALUES (2, 'pypi', 'cache_and_serve', 1700000001)
	`)
	if err != nil {
		t.Fatalf("seed upstream: %v", err)
	}

	// Delete the tenant; the upstream row should go with it.
	if _, err := db.ExecContext(ctx, `DELETE FROM tenants WHERE id = 2`); err != nil {
		t.Fatalf("delete tenant: %v", err)
	}
	var n int
	row := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tenant_upstreams WHERE tenant_id = 2`)
	if err := row.Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("expected cascade to drop tenant_upstreams rows; got %d remaining", n)
	}
}

// TestSchemaV6_PackagesCreatedVia verifies migration v6:
//
//   - packages.created_via column exists and is NOT NULL
//   - default value 'uploaded' is applied to rows inserted without it
//   - any string value is accepted by the column (no CHECK constraint;
//     the application layer guards via models.CreatedVia.Valid())
//
// Inserts BOTH a row without created_via (forcing the DEFAULT) AND a
// row with an explicit created_via to cover both paths the application
// uses on production code paths.
func TestSchemaV6_PackagesCreatedVia(t *testing.T) {
	dir := t.TempDir()
	db, err := pkgdb.Open(filepath.Join(dir, "v6.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()

	// Row 1: omit created_via -> DEFAULT 'uploaded' applies.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO packages (id, tenant_id, type, name, lower_name, created_unix)
		VALUES (1, 1, 'pypi', 'Default-Provenance', 'default-provenance', 1700000000)
	`); err != nil {
		t.Fatalf("insert row 1: %v", err)
	}
	// Row 2: explicit 'pull_through'.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO packages (id, tenant_id, type, name, lower_name, created_unix, created_via)
		VALUES (2, 1, 'pypi', 'Explicit-Provenance', 'explicit-provenance', 1700000001, 'pull_through')
	`); err != nil {
		t.Fatalf("insert row 2: %v", err)
	}

	var via1, via2 string
	if err := db.QueryRowContext(ctx,
		`SELECT created_via FROM packages WHERE id = 1`).Scan(&via1); err != nil {
		t.Fatalf("read row 1 created_via: %v", err)
	}
	if via1 != "uploaded" {
		t.Errorf("default created_via = %q, want \"uploaded\"", via1)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT created_via FROM packages WHERE id = 2`).Scan(&via2); err != nil {
		t.Fatalf("read row 2 created_via: %v", err)
	}
	if via2 != "pull_through" {
		t.Errorf("explicit created_via = %q, want \"pull_through\"", via2)
	}

	// NULL must be rejected by the NOT NULL constraint.
	_, err = db.ExecContext(ctx, `
		INSERT INTO packages (id, tenant_id, type, name, lower_name, created_unix, created_via)
		VALUES (3, 1, 'pypi', 'Null-Provenance', 'null-provenance', 1700000002, NULL)
	`)
	if err == nil {
		t.Error("expected NULL created_via to be rejected by NOT NULL constraint; got no error")
	}
}
