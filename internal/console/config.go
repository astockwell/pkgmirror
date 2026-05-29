package console

import (
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"
)

// Config captures every PKGMIRROR_CONSOLE_* environment variable plus
// the keys consumed by the session + CSRF middleware. See
// plans/web-console-implementation-plan.md §8 for the full table.
type Config struct {
	// Enabled wires/strips the /console route group. Set to false to
	// run the server without the console at all.
	Enabled bool

	// AuthMode is "password" or "proxy-header". PR 2 wires the
	// authenticators; PR 1 leaves them nil and serves /_ping anonymously.
	AuthMode string

	// Proxy-header mode config.
	UserHeader     string
	EmailHeader    string
	GroupsHeader   string
	TrustedProxies []string // CIDR or single IP strings
	BootstrapAdmin string   // username/email to auto-promote on first login

	// Session + CSRF keys, decoded from hex env vars.
	Session SessionConfig
	CSRFKey []byte // 32 bytes after hex decode (64 hex chars)

	// AllowEphemeralKeys gates generation-on-boot behavior in production.
	AllowEphemeralKeys bool

	// SessionTTL is the cookie MaxAge.
	SessionTTL time.Duration

	// DevDir, if set, overrides the embedded FS so edits to templates +
	// static assets are picked up without a rebuild. Dev only.
	DevDir string

	// DarkModeDefault is "auto", "light", or "dark".
	DarkModeDefault string

	// UsingTLS is set by the caller (from cfg.TLSEnabled()) so the
	// console can flip cookie Secure flags + emit HSTS.
	UsingTLS bool
}

// SessionConfig holds the decoded session keys.
type SessionConfig struct {
	AuthKey []byte        // 64 bytes after hex decode (128 hex chars)
	EncKey  []byte        // 32 bytes after hex decode (64 hex chars)
	TTL     time.Duration // copy of Config.SessionTTL, here for the middleware
	Secure  bool          // mirrors Config.UsingTLS
	Path    string        // "/console"
}

// Validate checks the config is internally consistent. Called from
// console.New(). Boot-time loud failures live here.
func (c *Config) Validate() error {
	switch c.AuthMode {
	case "password", "proxy-header":
		// ok
	case "":
		c.AuthMode = "password"
	default:
		return fmt.Errorf("PKGMIRROR_CONSOLE_AUTH_MODE must be 'password' or 'proxy-header', got %q", c.AuthMode)
	}

	if c.AuthMode == "proxy-header" && len(c.TrustedProxies) == 0 {
		return fmt.Errorf("PKGMIRROR_CONSOLE_TRUSTED_PROXIES is required in proxy-header mode")
	}

	if c.SessionTTL == 0 {
		c.SessionTTL = 24 * time.Hour
	}
	c.Session.TTL = c.SessionTTL
	c.Session.Secure = c.UsingTLS
	c.Session.Path = "/console"

	// Validate session/CSRF key lengths after hex decode.
	if len(c.Session.AuthKey) != 0 && len(c.Session.AuthKey) != 64 {
		return fmt.Errorf("PKGMIRROR_SESSION_AUTH_KEY decodes to %d bytes; need 64 (128 hex chars)", len(c.Session.AuthKey))
	}
	if len(c.Session.EncKey) != 0 && len(c.Session.EncKey) != 32 {
		return fmt.Errorf("PKGMIRROR_SESSION_ENC_KEY decodes to %d bytes; need 32 (64 hex chars)", len(c.Session.EncKey))
	}
	if len(c.CSRFKey) != 0 && len(c.CSRFKey) != 32 {
		return fmt.Errorf("PKGMIRROR_CSRF_KEY decodes to %d bytes; need 32 (64 hex chars)", len(c.CSRFKey))
	}

	if c.DarkModeDefault == "" {
		c.DarkModeDefault = "auto"
	}
	switch c.DarkModeDefault {
	case "auto", "light", "dark":
		// ok
	default:
		return fmt.Errorf("PKGMIRROR_CONSOLE_DARK_MODE_DEFAULT must be auto|light|dark, got %q", c.DarkModeDefault)
	}

	return nil
}

// LoadConfigFromEnv reads PKGMIRROR_CONSOLE_* and the session/CSRF key
// env vars into a Config. Missing keys are NOT generated here; that's
// the caller's job (so dev vs prod behavior stays explicit).
func LoadConfigFromEnv() (Config, error) {
	c := Config{
		Enabled:            getenvBool("PKGMIRROR_CONSOLE_ENABLED", true),
		AuthMode:           os.Getenv("PKGMIRROR_CONSOLE_AUTH_MODE"),
		UserHeader:         envDefault("PKGMIRROR_CONSOLE_AUTH_USER_HEADER", "X-Forwarded-User"),
		EmailHeader:        envDefault("PKGMIRROR_CONSOLE_AUTH_EMAIL_HEADER", "X-Forwarded-Email"),
		GroupsHeader:       envDefault("PKGMIRROR_CONSOLE_AUTH_GROUPS_HEADER", "X-Forwarded-Groups"),
		TrustedProxies:     splitCSV(os.Getenv("PKGMIRROR_CONSOLE_TRUSTED_PROXIES")),
		BootstrapAdmin:     os.Getenv("PKGMIRROR_CONSOLE_BOOTSTRAP_ADMIN"),
		AllowEphemeralKeys: getenvBool("PKGMIRROR_CONSOLE_ALLOW_EPHEMERAL_KEYS", false),
		DevDir:             os.Getenv("PKGMIRROR_CONSOLE_DEV_DIR"),
		DarkModeDefault:    os.Getenv("PKGMIRROR_CONSOLE_DARK_MODE_DEFAULT"),
	}

	if v := os.Getenv("PKGMIRROR_SESSION_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("PKGMIRROR_SESSION_TTL: %w", err)
		}
		c.SessionTTL = d
	}

	var err error
	if c.Session.AuthKey, err = decodeHexEnv("PKGMIRROR_SESSION_AUTH_KEY"); err != nil {
		return c, err
	}
	if c.Session.EncKey, err = decodeHexEnv("PKGMIRROR_SESSION_ENC_KEY"); err != nil {
		return c, err
	}
	if c.CSRFKey, err = decodeHexEnv("PKGMIRROR_CSRF_KEY"); err != nil {
		return c, err
	}

	return c, nil
}

// EnsureKeys generates session + CSRF keys if missing. Behavior:
//
//   - If all three are set: no-op.
//   - If any is missing AND devMode || AllowEphemeralKeys: generate the
//     missing ones, log a warning, return the values so callers can
//     surface them.
//   - If any is missing in production with AllowEphemeralKeys=false:
//     return an error (boot fails loud).
//
// devMode is typically derived from gin.Mode() == gin.DebugMode OR
// Config.DevDir != "".
func (c *Config) EnsureKeys(devMode bool, randomBytes func(n int) ([]byte, error)) (generated bool, err error) {
	missing := len(c.Session.AuthKey) == 0 || len(c.Session.EncKey) == 0 || len(c.CSRFKey) == 0
	if !missing {
		return false, nil
	}
	if !devMode && !c.AllowEphemeralKeys {
		return false, fmt.Errorf("PKGMIRROR_SESSION_AUTH_KEY / PKGMIRROR_SESSION_ENC_KEY / PKGMIRROR_CSRF_KEY are required in production; run `make gen-keys` and add them to your env, or set PKGMIRROR_CONSOLE_ALLOW_EPHEMERAL_KEYS=true to override")
	}
	if len(c.Session.AuthKey) == 0 {
		c.Session.AuthKey, err = randomBytes(64)
		if err != nil {
			return false, fmt.Errorf("generate session auth key: %w", err)
		}
	}
	if len(c.Session.EncKey) == 0 {
		c.Session.EncKey, err = randomBytes(32)
		if err != nil {
			return false, fmt.Errorf("generate session enc key: %w", err)
		}
	}
	if len(c.CSRFKey) == 0 {
		c.CSRFKey, err = randomBytes(32)
		if err != nil {
			return false, fmt.Errorf("generate CSRF key: %w", err)
		}
	}
	return true, nil
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "", "0":
		if v == "" {
			return def
		}
		return false
	case "false", "no", "off":
		return false
	case "1", "true", "yes", "on":
		return true
	}
	return def
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func decodeHexEnv(key string) ([]byte, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return nil, nil
	}
	b, err := hex.DecodeString(v)
	if err != nil {
		return nil, fmt.Errorf("%s: not valid hex: %w", key, err)
	}
	return b, nil
}
