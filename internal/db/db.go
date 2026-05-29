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
	// v2 -> v3: supply-chain policy engine foundation.
	// Adds policy_rules, audit_log, and the columns used by quarantine
	// + license + per-tenant audit configuration. See
	// plans/supply-chain-policy-engine.md §3 for the design.
	`
CREATE TABLE IF NOT EXISTS policy_rules (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT    NOT NULL,
    kind         TEXT    NOT NULL,

    -- Selector. NULL/empty = "any" for that dimension.
    tenant_id          INTEGER REFERENCES tenants(id) ON DELETE CASCADE,
    format             TEXT,
    package_lower_name TEXT,
    version_pattern    TEXT,

    -- Decision payload.
    action       TEXT    NOT NULL,
    config_json  TEXT    NOT NULL DEFAULT '{}',
    priority     INTEGER NOT NULL DEFAULT 100,

    enabled            INTEGER NOT NULL DEFAULT 1,
    created_unix       INTEGER NOT NULL,
    created_by_user_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
    expires_unix       INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_rules_lookup
    ON policy_rules(enabled, kind, tenant_id, format);

CREATE TABLE IF NOT EXISTS audit_log (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    created_unix    INTEGER NOT NULL,

    actor_user_id   INTEGER REFERENCES users(id)  ON DELETE SET NULL,
    actor_token_id  INTEGER REFERENCES tokens(id) ON DELETE SET NULL,
    request_id      TEXT,
    remote_addr     TEXT,
    user_agent      TEXT,

    tenant_id       INTEGER REFERENCES tenants(id) ON DELETE SET NULL,
    action          TEXT    NOT NULL,
    format          TEXT,
    package         TEXT,
    version         TEXT,
    filename        TEXT,

    decision        TEXT,
    rule_id         INTEGER REFERENCES policy_rules(id) ON DELETE SET NULL,
    reason          TEXT,

    extra_json      TEXT NOT NULL DEFAULT '{}'
);

CREATE INDEX IF NOT EXISTS idx_audit_tenant_time
    ON audit_log(tenant_id, created_unix DESC);
CREATE INDEX IF NOT EXISTS idx_audit_pkg
    ON audit_log(format, package, version);
CREATE INDEX IF NOT EXISTS idx_audit_actor
    ON audit_log(actor_user_id, created_unix DESC);

-- Per-tenant audit configuration.
ALTER TABLE tenants ADD COLUMN audit_reads          INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tenants ADD COLUMN audit_retention_days INTEGER NOT NULL DEFAULT 90;

-- Quarantine + license columns on package_versions.
ALTER TABLE package_versions ADD COLUMN license                TEXT;
ALTER TABLE package_versions ADD COLUMN quarantine_reason      TEXT;
ALTER TABLE package_versions ADD COLUMN quarantined_by_rule_id INTEGER REFERENCES policy_rules(id) ON DELETE SET NULL;
`,
	// v3 -> v4: web-console authentication surface.
	//
	// Adds password-mode credential columns to users and an actor_kind
	// column to audit_log so console-authored audit rows can be
	// distinguished from PAT-authored ones at query time.
	//
	// All three columns are nullable / default-NULL for backwards
	// compatibility:
	//   - proxy-header-mode deployments never populate password_hash
	//   - registry/admin PAT calls don't populate actor_kind (it's the
	//     console authenticators in PR 2b/2c that stamp it)
	//   - existing rows in users + audit_log keep their pre-v4 shape
	//
	// password_hash is the argon2id-encoded password (see
	// internal/users/password.go). password_set_unix tracks when it was
	// last set so the console can prompt re-set after a rotation policy
	// is in place (v2). Clearing a password sets both columns to NULL
	// in the same UPDATE.
	`
ALTER TABLE users     ADD COLUMN password_hash      TEXT;
ALTER TABLE users     ADD COLUMN password_set_unix  INTEGER;
ALTER TABLE audit_log ADD COLUMN actor_kind         TEXT;
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
