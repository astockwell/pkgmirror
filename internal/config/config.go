// Package config loads runtime configuration from environment variables.
package config

import (
	"os"
	"path/filepath"
)

// Config holds runtime configuration for the pkgmirror service.
type Config struct {
	// Addr is the address the HTTP server listens on (e.g. ":8080").
	Addr string

	// DataDir is the root directory under which DB + blobs live by default.
	DataDir string

	// DBPath is the SQLite database file path.
	DBPath string

	// BlobDir is the root directory for the filesystem blob backend.
	BlobDir string
}

// Load reads configuration from environment variables, applying defaults.
func Load() Config {
	dataDir := envOr("PKGMIRROR_DATA_DIR", "./data")
	cfg := Config{
		Addr:    envOr("PKGMIRROR_ADDR", ":8080"),
		DataDir: dataDir,
		DBPath:  envOr("PKGMIRROR_DB_PATH", filepath.Join(dataDir, "pkgmirror.db")),
		BlobDir: envOr("PKGMIRROR_BLOB_DIR", filepath.Join(dataDir, "blobs")),
	}
	return cfg
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
