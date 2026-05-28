// Package auth resolves an Authorization header (Basic or Bearer) into an
// Identity that handlers can use to authorize requests against tenants.
//
// The Authenticator interface is the seam where future identity sources
// plug in: today we ship TokenAuthenticator (DB-backed PATs); tomorrow an
// OIDCAuthenticator can be slotted in alongside without any handler changes.
package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"

	"github.com/astockwell/pkgmirror/internal/tenants"
	"github.com/astockwell/pkgmirror/internal/tokens"
	"github.com/astockwell/pkgmirror/internal/users"

	"github.com/gin-gonic/gin"
)

// Identity describes the resolved caller for a request.
type Identity struct {
	User        *users.User
	Token       *tokens.Token         // non-nil if authed via token
	Memberships map[int64]tenants.Role // tenant ID -> role
}

// CanRead reports whether this identity is permitted to read packages in
// the given tenant. Admins always can; otherwise the user must have
// reader+ membership AND a token that carries the "read" scope.
func (id *Identity) CanRead(tenantID int64) bool {
	if id == nil {
		return false
	}
	if id.User != nil && id.User.IsAdmin && id.HasTokenScope(tokens.ScopeAdmin) {
		return true
	}
	if !id.tokenAllowsTenant(tenantID) {
		return false
	}
	if !id.HasTokenScope(tokens.ScopeRead) && !id.HasTokenScope(tokens.ScopeWrite) {
		return false
	}
	role, ok := id.Memberships[tenantID]
	return ok && role >= tenants.RoleReader
}

// CanWrite reports whether this identity is permitted to upload packages
// in the given tenant.
func (id *Identity) CanWrite(tenantID int64) bool {
	if id == nil {
		return false
	}
	if id.User != nil && id.User.IsAdmin && id.HasTokenScope(tokens.ScopeAdmin) {
		return true
	}
	if !id.tokenAllowsTenant(tenantID) {
		return false
	}
	if !id.HasTokenScope(tokens.ScopeWrite) {
		return false
	}
	role, ok := id.Memberships[tenantID]
	return ok && role >= tenants.RoleWriter
}

// IsSystemAdmin reports whether this identity carries system-admin authority.
func (id *Identity) IsSystemAdmin() bool {
	return id != nil && id.User != nil && id.User.IsAdmin && id.HasTokenScope(tokens.ScopeAdmin)
}

// HasTokenScope reports whether the request's token includes the scope. If
// the request was authenticated some other way (no token), returns false.
func (id *Identity) HasTokenScope(s tokens.Scope) bool {
	return id != nil && id.Token != nil && id.Token.HasScope(s)
}

func (id *Identity) tokenAllowsTenant(tenantID int64) bool {
	if id == nil || id.Token == nil {
		return false
	}
	if !id.Token.TenantScope.Valid {
		return true
	}
	return id.Token.TenantScope.Int64 == tenantID
}

// Authenticator turns an HTTP request into an Identity, or returns nil for
// anonymous requests. Returning an error is reserved for unexpected failures
// (DB unreachable, etc.) — a missing or unrecognized credential is NOT an
// error; the middleware will treat such requests as anonymous and let the
// route guards decide what to do.
type Authenticator interface {
	Authenticate(ctx context.Context, r *http.Request) (*Identity, error)
}

// TokenAuthenticator authenticates requests using DB-backed personal access
// tokens. Tokens are accepted via:
//
//   - Authorization: Bearer <token>
//   - Authorization: Basic base64(<anything>:<token>)
type TokenAuthenticator struct {
	Tokens  *tokens.Store
	Users   *users.Store
	Tenants *tenants.Store
}

// Authenticate implements Authenticator.
func (a *TokenAuthenticator) Authenticate(ctx context.Context, r *http.Request) (*Identity, error) {
	raw := extractToken(r)
	if raw == "" {
		return nil, nil
	}
	tok, err := a.Tokens.Lookup(ctx, raw)
	if err != nil {
		if errors.Is(err, tokens.ErrNotExist) || errors.Is(err, tokens.ErrExpired) {
			return nil, nil
		}
		return nil, err
	}
	u, err := a.Users.GetByID(ctx, tok.UserID)
	if err != nil {
		return nil, err
	}
	mem, err := a.Tenants.Memberships(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	// Fire-and-forget bookkeeping; surface failures only via logs.
	go func() {
		_ = a.Tokens.TouchLastUsed(context.Background(), tok.ID)
		_ = a.Users.TouchLastSeen(context.Background(), u.ID)
	}()
	return &Identity{User: u, Token: tok, Memberships: mem}, nil
}

// extractToken pulls the plaintext token from the Authorization header.
func extractToken(r *http.Request) string {
	// NuGet clients (dotnet / nuget.exe) send the PAT in a custom
	// X-NuGet-ApiKey header rather than Authorization. Honor it
	// before falling back to the standard Authorization path. See
	// https://learn.microsoft.com/en-us/nuget/api/package-publish-resource#request-parameters
	if k := strings.TrimSpace(r.Header.Get("X-NuGet-ApiKey")); k != "" {
		return k
	}
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(h, "Bearer "):
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	case strings.HasPrefix(h, "Basic "):
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(strings.TrimPrefix(h, "Basic ")))
		if err != nil {
			return ""
		}
		idx := strings.IndexByte(string(decoded), ':')
		if idx < 0 {
			return ""
		}
		return string(decoded[idx+1:])
	case strings.HasPrefix(h, tokens.Prefix):
		// Some clients (notably `gem push` and cargo's "Token <value>"
		// variants without the scheme prefix) send the raw token as the
		// Authorization header value. Our tokens carry a recognizable
		// pkm_ prefix so this case is unambiguous — anything that
		// doesn't start with the prefix is rejected.
		return strings.TrimSpace(h)
	}
	return ""
}

// gin context key for the resolved identity.
const ginIdentityKey = "pkgmirror.identity"

// Middleware returns a Gin middleware that resolves the request's identity
// and attaches it under the conventional context key. Handlers retrieve it
// via FromContext.
func Middleware(a Authenticator) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := a.Authenticate(c.Request.Context(), c.Request)
		if err != nil {
			// Authentication errors are server problems, not client problems.
			// Log via Gin and treat as anonymous.
			_ = c.Error(err)
		}
		if id != nil {
			c.Set(ginIdentityKey, id)
		}
		c.Next()
	}
}

// FromContext returns the resolved identity for the current request, or nil
// if anonymous.
func FromContext(c *gin.Context) *Identity {
	v, ok := c.Get(ginIdentityKey)
	if !ok {
		return nil
	}
	id, _ := v.(*Identity)
	return id
}

// RequireRead enforces read access to the named tenant. If the tenant is
// public, all callers pass. Otherwise an authenticated identity with read
// access is required. On denial the response is written and false returned.
func RequireRead(c *gin.Context, tenant *tenants.Tenant) bool {
	if tenant.Visibility == tenants.VisibilityPublic {
		return true
	}
	id := FromContext(c)
	if id == nil {
		writeChallenge(c, http.StatusUnauthorized)
		return false
	}
	if !id.CanRead(tenant.ID) {
		c.String(http.StatusForbidden, "forbidden")
		return false
	}
	return true
}

// RequireWrite enforces write access to the named tenant.
func RequireWrite(c *gin.Context, tenant *tenants.Tenant) bool {
	id := FromContext(c)
	if id == nil {
		writeChallenge(c, http.StatusUnauthorized)
		return false
	}
	if !id.CanWrite(tenant.ID) {
		c.String(http.StatusForbidden, "forbidden")
		return false
	}
	return true
}

func writeChallenge(c *gin.Context, status int) {
	c.Header("WWW-Authenticate", `Basic realm="pkgmirror"`)
	c.String(status, http.StatusText(status))
}
