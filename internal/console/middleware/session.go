package middleware

import (
	"encoding/gob"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
)

// SessionConfig is exactly the shape Console.Config.Session takes; we
// declare it here too so the middleware doesn't import the parent
// console package (avoiding an import cycle).
type SessionConfig struct {
	AuthKey []byte
	EncKey  []byte
	TTL     time.Duration
	Secure  bool
	Path    string
}

// NewSessionMiddleware wires gin-contrib/sessions on top of a cookie
// store (no SQLite session table; see plans/web-console.md §5.4).
//
// Keys must be the decoded-byte form (64 bytes auth, 32 bytes encryption);
// the parent config loader handles hex decode before getting here. We
// validate the byte lengths and fail loud so the wrong shape never
// silently degrades to plaintext or unauthed cookies.
func NewSessionMiddleware(cfg SessionConfig) (gin.HandlerFunc, error) {
	if len(cfg.AuthKey) != 64 {
		return nil, fmt.Errorf("session auth key: need 64 bytes after hex decode, got %d", len(cfg.AuthKey))
	}
	if len(cfg.EncKey) != 32 {
		return nil, fmt.Errorf("session enc key: need 32 bytes after hex decode, got %d", len(cfg.EncKey))
	}

	store := cookie.NewStore(cfg.AuthKey, cfg.EncKey)
	maxAge := int(cfg.TTL.Seconds())
	if maxAge <= 0 {
		maxAge = int((24 * time.Hour).Seconds())
	}
	path := cfg.Path
	if path == "" {
		path = "/console"
	}
	store.Options(sessions.Options{
		Path:     path,
		MaxAge:   maxAge,
		Secure:   cfg.Secure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	// Gorilla's session encodes values via gob. Register every type that
	// goes through AddFlash so gob.Encode doesn't fail at runtime.
	gob.Register(FlashMessage{})

	return sessions.Sessions("pkgmirror_session", store), nil
}
