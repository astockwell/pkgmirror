// Package config loads runtime configuration from environment variables.
package config

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/astockwell/pkgmirror/internal/tenants"
)

// Config holds runtime configuration for the pkgmirror service.
type Config struct {
	Addr    string
	DataDir string
	DBPath  string
	BlobDir string

	// AdminToken, if set, is installed as the admin token at bootstrap (if
	// no admin token already exists). Empty = generate one on first boot.
	AdminToken string

	// DefaultTenantName is the tenant created on first boot.
	DefaultTenantName string

	// DefaultTenantVisibility controls anonymous read access to the default
	// tenant. Parsed from PKGMIRROR_DEFAULT_TENANT_VISIBILITY = "private" or
	// "public". Default: private.
	DefaultTenantVisibility tenants.Visibility
}

// Load reads configuration from environment variables, applying defaults.
func Load() Config {
	dataDir := envOr("PKGMIRROR_DATA_DIR", "./data")
	vis := tenants.VisibilityPrivate
	switch strings.ToLower(envOr("PKGMIRROR_DEFAULT_TENANT_VISIBILITY", "private")) {
	case "public":
		vis = tenants.VisibilityPublic
	}
	return Config{
		Addr:                    envOr("PKGMIRROR_ADDR", ":8080"),
		DataDir:                 dataDir,
		DBPath:                  envOr("PKGMIRROR_DB_PATH", filepath.Join(dataDir, "pkgmirror.db")),
		BlobDir:                 envOr("PKGMIRROR_BLOB_DIR", filepath.Join(dataDir, "blobs")),
		AdminToken:              os.Getenv("PKGMIRROR_ADMIN_TOKEN"),
		DefaultTenantName:       envOr("PKGMIRROR_DEFAULT_TENANT", "default"),
		DefaultTenantVisibility: vis,
	}
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
