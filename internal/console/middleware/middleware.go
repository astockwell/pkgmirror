// Package middleware contains the gin middleware that powers the web
// console: session/CSRF/security-headers (PR 1) and the authenticator +
// auth gates (PR 2).
//
// Two seemingly-redundant flash helpers live here (AddFlash + the
// Flash* convenience wrappers) so handlers pick a typed verb instead
// of stringly-typing every call site.
package middleware

import "log/slog"

// Middleware bundles per-console gate helpers. New() returns one; the
// Console stores it for handlers to grab via Console.Middleware().
//
// PR 1 ships only the FlashHelpers indirectly (functions, not methods);
// the gate methods (RequireAuth, RequireSystemAdmin, etc.) arrive in
// PR 2 once the authenticator exists.
type Middleware struct {
	Logger   *slog.Logger
	AuthMode string // "password" | "proxy-header" — PR 2 uses this
}

// Deps for constructing a Middleware.
type Deps struct {
	Logger   *slog.Logger
	AuthMode string
}

// New builds the middleware bundle.
func New(d Deps) *Middleware {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Middleware{Logger: d.Logger, AuthMode: d.AuthMode}
}
