package middleware

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/astockwell/pkgmirror/internal/auth"
	"github.com/astockwell/pkgmirror/internal/tenants"
	"github.com/astockwell/pkgmirror/internal/users"
)

// ProxyHeaderAuthenticator authenticates console requests via headers
// set by a trusted upstream proxy (oauth2-proxy, Cloudflare Access,
// AWS ALB OIDC, Authelia, etc).
//
// Two rules are load-bearing:
//
//  1. We MUST verify the immediate peer is in TrustedProxies before
//     trusting any header. Without that gate, anyone on the network
//     can supply a spoofed X-Forwarded-User header and get admin
//     access to whoever they claim to be.
//
//  2. We MUST resolve the immediate peer via the centralized
//     ClientIPFor logic (which itself walks the trusted chain). gins
//     c.ClientIP() honors XFF too aggressively for our threat model.
//
// Bootstrap-admin promotion lives here too: on every login, if
// PKGMIRROR_CONSOLE_BOOTSTRAP_ADMIN matches the resolved user
// (case-insensitive on name OR email) AND the user is not already
// admin, set IsAdmin=true + emit one audit row. Convergent: after the
// first promotion subsequent matching sign-ins are no-ops.
type ProxyHeaderAuthenticator struct {
	Users   *users.Store
	Tenants *tenants.Store

	// UserHeader / EmailHeader / GroupsHeader are the configured
	// header names (defaults: X-Forwarded-User, X-Forwarded-Email,
	// X-Forwarded-Groups). GroupsHeader is read but currently ignored
	// (group-to-role mapping is deferred to v2).
	UserHeader   string
	EmailHeader  string
	GroupsHeader string

	// TrustedProxies is the IP/CIDR allowlist of upstream proxies we
	// trust. Empty = boot fail (Config.Validate enforces).
	TrustedProxies []string

	// BootstrapAdmin is the env-configured name/email to auto-promote
	// to admin on first matching login. Empty = no auto-promotion.
	// Compared case-insensitively against user.Name AND user.Email.
	BootstrapAdmin string

	// Promote is the function that flips IsAdmin to true. Decoupled
	// so tests can stub it.
	Promote func(ctx context.Context, userID int64) error
}

// NewProxyHeaderAuthenticator constructs the authenticator from a
// Config-derived blob. Returns the auth.Authenticator interface so the
// caller doesnt need to know the concrete type.
func NewProxyHeaderAuthenticator(opts ProxyHeaderOptions) auth.Authenticator {
	a := &ProxyHeaderAuthenticator{
		Users:          opts.Users,
		Tenants:        opts.Tenants,
		UserHeader:     opts.UserHeader,
		EmailHeader:    opts.EmailHeader,
		GroupsHeader:   opts.GroupsHeader,
		TrustedProxies: opts.TrustedProxies,
		BootstrapAdmin: opts.BootstrapAdmin,
	}
	if a.Promote == nil {
		a.Promote = func(ctx context.Context, userID int64) error {
			_, err := opts.Users.DB.ExecContext(ctx,
				`UPDATE users SET is_admin = 1 WHERE id = ?`, userID)
			return err
		}
	}
	return a
}

// ProxyHeaderOptions is what NewProxyHeaderAuthenticator consumes;
// avoids growing a long positional signature.
type ProxyHeaderOptions struct {
	Users          *users.Store
	Tenants        *tenants.Store
	UserHeader     string
	EmailHeader    string
	GroupsHeader   string
	TrustedProxies []string
	BootstrapAdmin string
}

// Authenticate implements auth.Authenticator.
//
// Failure modes by intent:
//   - peer NOT in TrustedProxies   -> nil, nil (anonymous; headers IGNORED)
//   - peer trusted, header missing -> nil, nil (anonymous)
//   - peer trusted, header set     -> hydrate identity + bootstrap if needed
//   - DB error                     -> wrap and return (caller fails closed)
func (a *ProxyHeaderAuthenticator) Authenticate(ctx context.Context, r *http.Request) (*auth.Identity, error) {
	if !a.isTrustedPeer(r) {
		// Strict: never trust headers from an untrusted peer, even if
		// they happen to be set. This is the load-bearing security
		// property; the gate exists precisely to defeat spoofing.
		return nil, nil
	}
	rawUser := strings.TrimSpace(r.Header.Get(a.headerName(a.UserHeader, "X-Forwarded-User")))
	if rawUser == "" {
		return nil, nil // anonymous; header not present
	}
	rawEmail := strings.TrimSpace(r.Header.Get(a.headerName(a.EmailHeader, "X-Forwarded-Email")))

	u, err := a.Users.GetOrCreate(ctx, rawUser, rawEmail)
	if err != nil {
		return nil, fmt.Errorf("proxy-header auth: get-or-create %q: %w", rawUser, err)
	}

	// Bootstrap admin promotion. Convergent: a no-op once IsAdmin is
	// already true. Loud at INFO so operators can see it once.
	if a.BootstrapAdmin != "" && !u.IsAdmin && a.matchesBootstrap(u) {
		if err := a.Promote(ctx, u.ID); err != nil {
			// Promotion failure should not block sign-in; the user
			// can still navigate as a regular user. Warn loudly.
			slog.Default().Warn("bootstrap admin promotion failed",
				"user_id", u.ID, "user_name", u.Name, "err", err)
		} else {
			slog.Default().Info("BOOTSTRAP ADMIN PROMOTION",
				"user_id", u.ID, "user_name", u.Name,
				"reason", "PKGMIRROR_CONSOLE_BOOTSTRAP_ADMIN matched")
			u.IsAdmin = true
		}
	}

	mem, err := a.Tenants.Memberships(ctx, u.ID)
	if err != nil {
		return nil, fmt.Errorf("proxy-header auth: memberships for user %d: %w", u.ID, err)
	}
	return &auth.Identity{
		User:        u,
		Memberships: mem,
		Kind:        auth.CredentialProxy,
	}, nil
}

// isTrustedPeer returns true when the immediate remote addr is in the
// configured TrustedProxies list. Uses the same parsing/matching as
// ClientIPFor so the two surfaces never disagree.
func (a *ProxyHeaderAuthenticator) isTrustedPeer(r *http.Request) bool {
	if len(a.TrustedProxies) == 0 {
		return false
	}
	// We deliberately do NOT call ClientIPFor here, because that
	// function CONSUMES XFF when the peer is trusted. For the gate
	// itself we only care about the immediate peer.
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	// Strip IPv6 brackets if present.
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	return ipInList(host, a.TrustedProxies)
}

// matchesBootstrap is case-insensitive against user.Name and (if set)
// user.Email. PKGMIRROR_CONSOLE_BOOTSTRAP_ADMIN can be either form;
// operators don't have to guess which.
func (a *ProxyHeaderAuthenticator) matchesBootstrap(u *users.User) bool {
	if a.BootstrapAdmin == "" || u == nil {
		return false
	}
	want := strings.ToLower(strings.TrimSpace(a.BootstrapAdmin))
	if strings.ToLower(u.Name) == want {
		return true
	}
	if u.Email.Valid && strings.ToLower(u.Email.String) == want {
		return true
	}
	return false
}

func (a *ProxyHeaderAuthenticator) headerName(configured, fallback string) string {
	if configured != "" {
		return configured
	}
	return fallback
}

// sentinel used by tests; never referenced in real handlers.
var _ = errors.New
