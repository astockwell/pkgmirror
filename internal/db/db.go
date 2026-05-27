// Package db opens the SQLite database and applies schema migrations.
package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// migrations is an ordered list of schema versions. The i-th element (0-indexed)
// is the SQL that brings the schema FROM version i TO version i+1.
// SQLite's PRAGMA user_version tracks the currently applied version.
var migrations = []string{
	// v0 -> v1: initial schema (packages, versions, blobs, files, properties).
	`
CREATE TABLE IF NOT EXISTS packages (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    type            TEXT    NOT NULL,
    name            TEXT    NOT NULL,
    lower_name      TEXT    NOT NULL,
    created_unix    INTEGER NOT NULL,
    UNIQUE(type, lower_name)
);

CREATE TABLE IF NOT EXISTS package_versions (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    package_id      INTEGER NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
    version         TEXT    NOT NULL,
    lower_version   TEXT    NOT NULL,
    metadata_json   TEXT    NOT NULL DEFAULT '{}',
    created_unix    INTEGER NOT NULL,
    UNIQUE(package_id, lower_version)
);

CREATE INDEX IF NOT EXISTS idx_pkgversions_pkg
    ON package_versions(package_id, created_unix);

CREATE TABLE IF NOT EXISTS package_blobs (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    size            INTEGER NOT NULL,
    hash_md5        TEXT    NOT NULL,
    hash_sha1       TEXT    NOT NULL,
    hash_sha256     TEXT    NOT NULL UNIQUE,
    hash_sha512     TEXT    NOT NULL,
    created_unix    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS package_files (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    version_id      INTEGER NOT NULL REFERENCES package_versions(id) ON DELETE CASCADE,
    blob_id         INTEGER NOT NULL REFERENCES package_blobs(id),
    name            TEXT    NOT NULL,
    lower_name      TEXT    NOT NULL,
    is_lead         INTEGER NOT NULL DEFAULT 0,
    created_unix    INTEGER NOT NULL,
    UNIQUE(version_id, lower_name)
);

CREATE TABLE IF NOT EXISTS package_properties (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    ref_type        INTEGER NOT NULL,
    ref_id          INTEGER NOT NULL,
    name            TEXT    NOT NULL,
    value           TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_props_ref
    ON package_properties(ref_type, ref_id, name);
`,
	// v1 -> v2: multi-tenancy + users + tokens. Adds tenant_id to packages
	// and changes the uniqueness constraint to (tenant_id, type, lower_name).
	`
CREATE TABLE IF NOT EXISTS tenants (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT    NOT NULL,
    lower_name      TEXT    NOT NULL UNIQUE,
    visibility      INTEGER NOT NULL DEFAULT 0,
    created_unix    INTEGER NOT NULL
);

-- Seed a "default" tenant with ID 1 so existing packages can be backfilled
-- and the FK constraint added below is satisfiable.
INSERT INTO tenants (id, name, lower_name, visibility, created_unix)
SELECT 1, 'default', 'default', 0, CAST(strftime('%s','now') AS INTEGER)
WHERE NOT EXISTS (SELECT 1 FROM tenants WHERE lower_name = 'default');

CREATE TABLE IF NOT EXISTS users (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    name                TEXT    NOT NULL,
    lower_name          TEXT    NOT NULL UNIQUE,
    kind                INTEGER NOT NULL,
    email               TEXT,
    is_admin            INTEGER NOT NULL DEFAULT 0,
    external_provider   TEXT,
    external_subject    TEXT,
    created_unix        INTEGER NOT NULL,
    last_seen_unix      INTEGER NOT NULL DEFAULT 0
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_users_external
    ON users(external_provider, external_subject)
    WHERE external_provider IS NOT NULL;

CREATE TABLE IF NOT EXISTS tenant_members (
    tenant_id   INTEGER NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    user_id     INTEGER NOT NULL REFERENCES users(id)   ON DELETE CASCADE,
    role        INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, user_id)
);

CREATE TABLE IF NOT EXISTS tokens (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name            TEXT    NOT NULL,
    hash            TEXT    NOT NULL UNIQUE,
    scopes          TEXT    NOT NULL,
    tenant_scope    INTEGER REFERENCES tenants(id) ON DELETE CASCADE,
    created_unix    INTEGER NOT NULL,
    last_used_unix  INTEGER NOT NULL DEFAULT 0,
    expires_unix    INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_tokens_user ON tokens(user_id);

-- Add tenant_id to packages with DEFAULT 1 so any existing rows backfill
-- to the seeded "default" tenant.
ALTER TABLE packages ADD COLUMN tenant_id INTEGER NOT NULL DEFAULT 1
    REFERENCES tenants(id);

-- Recreate packages with the new UNIQUE(tenant_id, type, lower_name)
-- constraint. SQLite can't ALTER an existing UNIQUE in place.
CREATE TABLE packages_new (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    tenant_id       INTEGER NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    type            TEXT    NOT NULL,
    name            TEXT    NOT NULL,
    lower_name      TEXT    NOT NULL,
    created_unix    INTEGER NOT NULL,
    UNIQUE(tenant_id, type, lower_name)
);

INSERT INTO packages_new (id, tenant_id, type, name, lower_name, created_unix)
SELECT id, tenant_id, type, name, lower_name, created_unix FROM packages;

DROP TABLE packages;
ALTER TABLE packages_new RENAME TO packages;
`,
}

// Open opens (and creates if missing) the SQLite database at path and brings
// it up to the latest schema version.
func Open(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", path)
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := conn.Ping(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := migrate(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// migrate applies pending migrations in order and bumps PRAGMA user_version.
func migrate(conn *sql.DB) error {
	var current int
	if err := conn.QueryRow("PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	for i := current; i < len(migrations); i++ {
		tx, err := conn.Begin()
		if err != nil {
			return fmt.Errorf("begin migration v%d: %w", i+1, err)
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration v%d: %w", i+1, err)
		}
		// PRAGMA user_version does not accept bind parameters.
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("bump user_version to %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration v%d: %w", i+1, err)
		}
	}
	return nil
}
