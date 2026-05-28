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

	// TmpDir is where short-lived staging files live (upload buffers,
	// in-progress OCI blob uploads). Defaults to $DATA_DIR/tmp so it
	// shares a filesystem with BlobDir — the final move-to-blob-store
	// is then a cheap rename rather than a cross-FS copy. Set this
	// explicitly if you want to put staging on a different volume
	// (e.g. a fast local SSD while BlobDir points at a slower mount).
	TmpDir string

	// AdminToken, if set, is installed as the admin token at bootstrap (if
	// no admin token already exists). Empty = generate one on first boot.
	AdminToken string

	// DefaultTenantName is the tenant created on first boot.
	DefaultTenantName string

	// DefaultTenantVisibility controls anonymous read access to the default
	// tenant. Parsed from PKGMIRROR_DEFAULT_TENANT_VISIBILITY = "private" or
	// "public". Default: private.
	DefaultTenantVisibility tenants.Visibility

	// PolicyFile is the optional path to a YAML rule file loaded at boot
	// and upserted into the policy_rules table. See
	// plans/supply-chain-policy-engine.md §10 for the schema.
	PolicyFile string

	// TLSCertFile and TLSKeyFile, if both set, switch the listener
	// from plain HTTP to HTTPS. Operators are responsible for cert
	// material — pkgmirror does not ship ACME / Let's Encrypt
	// integration. For automatic certs, run pkgmirror behind a
	// reverse proxy (nginx, Caddy, Traefik) and leave these empty.
	//
	// Both must point at PEM-encoded files. A cert chain (leaf +
	// intermediates concatenated) is the typical shape; the key
	// file is the matching private key.
	TLSCertFile string
	TLSKeyFile  string
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
		TmpDir:                  envOr("PKGMIRROR_TMP_DIR", filepath.Join(dataDir, "tmp")),
		AdminToken:              os.Getenv("PKGMIRROR_ADMIN_TOKEN"),
		DefaultTenantName:       envOr("PKGMIRROR_DEFAULT_TENANT", "default"),
		DefaultTenantVisibility: vis,
		PolicyFile:              os.Getenv("PKGMIRROR_POLICY_FILE"),
		TLSCertFile:             os.Getenv("PKGMIRROR_TLS_CERT"),
		TLSKeyFile:              os.Getenv("PKGMIRROR_TLS_KEY"),
	}
}

// TLSEnabled reports whether both cert and key files are configured.
// We treat partial config (only one set) as a misconfiguration the
// caller should catch and report — see cmd/pkgmirror/main.go.
func (c Config) TLSEnabled() bool {
	return c.TLSCertFile != "" && c.TLSKeyFile != ""
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
