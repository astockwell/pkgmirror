// Package db opens the SQLite database and applies the schema.
package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const schema = `
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
`

// Open opens (and creates if missing) the SQLite database at path and applies
// the schema.
func Open(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", path)
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	if err := conn.Ping(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	if _, err := conn.Exec(schema); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	return conn, nil
}
