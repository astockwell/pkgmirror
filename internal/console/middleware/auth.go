// Console auth middleware: SessionAuthenticator + ProxyHeaderAuthenticator
// + route gate helpers. PR 2b ships SessionAuthenticator + gates and the
// constructor seam; PR 2c adds ProxyHeaderAuthenticator.
//
// Auth code is supply-chain spine. The rules are explicit:
//
//   - Missing credentials -> anonymous (returning nil, nil from
//     Authenticate). The route gate decides what to do.
//   - DB / session corruption -> error returned, request fails with 500
//     (fail closed).
//   - Browser-backed identities carry the user's full authority via
//     CredentialKind=Session/Proxy; auth.Identity's CanRead/CanWrite/
//     IsSystemAdmin already switch on Kind (PR 0a).

package middleware

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/astockwell/pkgmirror/internal/auth"
	"github.com/astockwell/pkgmirror/internal/tenants"
	"github.com/astockwell/pkgmirror/internal/users"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

// sessionKeyUserID is the session map key for the logged-in user id.
const sessionKeyUserID = "user_id"

// sessionKeyReturnTo stashes the URL to bounce back to after login.
const sessionKeyReturnTo = "return_to"

// SessionAuthenticator authenticates console requests via a
// gin-contrib/sessions cookie set by the /console/login handler.
//
// The session payload is intentionally minimal: just the user id. Roles
// and memberships are read fresh from the DB on every request so a
// permission revoke takes effect on the next request, not after a
// logout-login cycle.
type SessionAuthenticator struct {
	Users   *users.Store
	Tenants *tenants.Store
}

// NewSessionAuthenticator constructs a SessionAuthenticator. Returns
// auth.Authenticator so the caller doesn't have to know the concrete
// type once the seam is wired.
func NewSessionAuthenticator(u *users.Store, t *tenants.Store) auth.Authenticator {
	return &SessionAuthenticator{Users: u, Tenants: t}
}

// Authenticate implements auth.Authenticator.
func (a *SessionAuthenticator) Authenticate(ctx context.Context, r *http.Request) (*auth.Identity, error) {
	gc, ok := ginContextFrom(ctx)
	if !ok {
		// auth.Middleware wires gin.Context into the request context
		// via c.Request.Context(); if it's missing here, our middleware
		// chain is broken. Treat as anonymous + log so it surfaces.
		slog.Default().Warn("session auth: no gin.Context in request; chain misconfigured")
		return nil, nil
	}
	sess := sessions.Default(gc)
	raw := sess.Get(sessionKeyUserID)
	if raw == nil {
		return nil, nil // not logged in -> anonymous
	}
	uid, ok := raw.(int64)
	if !ok {
		// Cookie corruption or someone hand-rolled a value of the
		// wrong type. Clear the session and treat as anonymous so a
		// retry naturally produces a fresh login flow.
		slog.Default().Warn("session auth: user_id has wrong type", "got", fmt.Sprintf("%T", raw))
		sess.Clear()
		_ = sess.Save()
		return nil, nil
	}
	u, err := a.Users.GetByID(ctx, uid)
	if err != nil {
		if errors.Is(err, users.ErrNotExist) {
			// Session for a deleted user; clear and treat anonymous.
			sess.Clear()
			_ = sess.Save()
			return nil, nil
		}
		return nil, fmt.Errorf("session auth: get user %d: %w", uid, err)
	}
	mem, err := a.Tenants.Memberships(ctx, u.ID)
	if err != nil {
		return nil, fmt.Errorf("session auth: memberships for user %d: %w", u.ID, err)
	}
	return &auth.Identity{User: u, Memberships: mem, Kind: auth.CredentialSession}, nil
}

// LoginSession writes the user id into the gin-contrib/sessions cookie.
// Called from the login handler after a successful password check.
// Errors on the underlying session save are surfaced so the handler can
// log+render a friendly 500 instead of leaving the user in a half-
// authenticated state.
func LoginSession(c *gin.Context, userID int64) error {
	sess := sessions.Default(c)
	sess.Set(sessionKeyUserID, userID)
	return sess.Save()
}

// LogoutSession clears the user id (and everything else) from the
// session cookie.
func LogoutSession(c *gin.Context) error {
	sess := sessions.Default(c)
	sess.Clear()
	return sess.Save()
}

// StashReturnTo remembers a URL to bounce back to after the user
// successfully logs in. Called from RequireAuth when a request is
// redirected to the login page.
func StashReturnTo(c *gin.Context, url string) error {
	if url == "" || url == "/console/login" {
		return nil
	}
	sess := sessions.Default(c)
	sess.Set(sessionKeyReturnTo, url)
	return sess.Save()
}

// PopReturnTo returns and clears the previously stashed URL, or empty
// if none is set.
func PopReturnTo(c *gin.Context) string {
	sess := sessions.Default(c)
	raw := sess.Get(sessionKeyReturnTo)
	if raw == nil {
		return ""
	}
	s, _ := raw.(string)
	sess.Delete(sessionKeyReturnTo)
	_ = sess.Save()
	return s
}

// RequireAuth gates a route to require any authenticated identity.
// password-mode: redirect (303) to /console/login with return_to
//   stashed in the session.
// proxy-header mode: render a 401 page suggesting upstream-proxy misconfig.
func (m *Middleware) RequireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := auth.FromContext(c)
		if id != nil && id.User != nil {
			c.Next()
			return
		}
		if m.AuthMode == "proxy-header" {
			c.Header("Content-Type", "text/plain; charset=utf-8")
			c.String(http.StatusUnauthorized,
				"unauthorized: upstream proxy did not set the user header (check PKGMIRROR_CONSOLE_AUTH_USER_HEADER + your reverse proxy config)")
			c.Abort()
			return
		}
		// Password mode default.
		if err := StashReturnTo(c, c.Request.URL.RequestURI()); err != nil {
			slog.Default().Warn("stash return_to failed", "err", err)
		}
		c.Redirect(http.StatusSeeOther, "/console/login")
		c.Abort()
	}
}

// RequireSystemAdmin gates a route to require auth.Identity.IsSystemAdmin.
// MUST be chained AFTER RequireAuth (depends on a non-nil identity).
func (m *Middleware) RequireSystemAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := auth.FromContext(c)
		if id == nil || !id.IsSystemAdmin() {
			c.Header("Content-Type", "text/plain; charset=utf-8")
			c.String(http.StatusForbidden, "system admin only")
			c.Abort()
			return
		}
		c.Next()
	}
}

// RequireTenantMember gates a route to require the request's identity
// can read the :tenant path parameter's tenant. Resolves the tenant by
// lower_name. Renders 404 for nonexistent (don't leak which tenants
// exist), 403 for known-but-inaccessible.
func (m *Middleware) RequireTenantMember(tenantStore *tenants.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := c.Param("tenant")
		if name == "" {
			c.String(http.StatusNotFound, "tenant not specified")
			c.Abort()
			return
		}
		t, err := tenantStore.GetByName(c.Request.Context(), name)
		if err != nil {
			c.String(http.StatusNotFound, "tenant not found")
			c.Abort()
			return
		}
		id := auth.FromContext(c)
		if id == nil || !id.CanRead(t.ID) {
			c.String(http.StatusForbidden, "no access to this tenant")
			c.Abort()
			return
		}
		c.Set("pkgmirror.console.active_tenant", t)
		c.Next()
	}
}

// ginContextFrom recovers the *gin.Context wrapped in a request context
// by gin.Context.Request.Context(). gin doesn't expose this directly;
// the standard trick is to type-assert via the well-known key gin uses
// internally. We avoid that brittleness by using a different mechanism:
// the Authenticate caller (auth.Middleware) passes c.Request.Context(),
// which carries the *gin.Context only if the caller stashed it.
//
// To make this work, console.go's wiring must stash gin.Context into
// the request context before calling auth.Middleware. We provide
// WithGinContext + GinContextFrom helpers for that.
func ginContextFrom(ctx context.Context) (*gin.Context, bool) {
	v := ctx.Value(ginContextKey{})
	if v == nil {
		return nil, false
	}
	gc, ok := v.(*gin.Context)
	return gc, ok
}

type ginContextKey struct{}

// WithGinContext attaches a *gin.Context to a context.Context so that
// the SessionAuthenticator can read the session out of it. Mounted as
// the first middleware in the console's chain so every downstream
// authenticator sees it.
func WithGinContext() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ginContextKey{}, c))
		c.Next()
	}
}
