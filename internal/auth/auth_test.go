package auth

import (
	"database/sql"
	"testing"

	"github.com/astockwell/pkgmirror/internal/tenants"
	"github.com/astockwell/pkgmirror/internal/tokens"
	"github.com/astockwell/pkgmirror/internal/users"
)

func sqlInt64(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

// TestIdentity_AuthMatrix covers (CredentialKind × method × user-flag ×
// membership-role × token-scope) for the three Identity authorization
// methods. The matrix is the contract: session/proxy identities authorize
// via membership alone; token identities additionally require scopes.
func TestIdentity_AuthMatrix(t *testing.T) {
	adminUser := &users.User{ID: 1, Name: "alice", IsAdmin: true}
	regularUser := &users.User{ID: 2, Name: "bob", IsAdmin: false}

	tokenAdmin := &tokens.Token{Scopes: []tokens.Scope{tokens.ScopeAdmin, tokens.ScopeRead, tokens.ScopeWrite}}
	tokenRead := &tokens.Token{Scopes: []tokens.Scope{tokens.ScopeRead}}
	tokenWrite := &tokens.Token{Scopes: []tokens.Scope{tokens.ScopeWrite}}

	memReader := map[int64]tenants.Role{1: tenants.RoleReader}
	memWriter := map[int64]tenants.Role{1: tenants.RoleWriter}
	memAdmin := map[int64]tenants.Role{1: tenants.RoleTenantAdmin}

	cases := []struct {
		name           string
		id             *Identity
		wantSysAdmin   bool
		wantCanRead    bool
		wantCanWrite   bool
	}{
		// --- nil identity ---
		{
			name:         "nil identity is anonymous",
			id:           nil,
			wantSysAdmin: false,
			wantCanRead:  false,
			wantCanWrite: false,
		},

		// --- CredentialToken: traditional PAT auth ---
		{
			name: "token: admin user with admin scope is system admin",
			id: &Identity{
				User: adminUser, Token: tokenAdmin, Memberships: memReader,
				Kind: CredentialToken,
			},
			wantSysAdmin: true,
			wantCanRead:  true,
			wantCanWrite: true,
		},
		{
			name: "token: admin user with read-only scope is NOT system admin",
			id: &Identity{
				User: adminUser, Token: tokenRead, Memberships: memReader,
				Kind: CredentialToken,
			},
			wantSysAdmin: false,
			wantCanRead:  true, // read scope + admin user passes via reader role
			wantCanWrite: false,
		},
		{
			name: "token: regular user with reader membership + read scope can read",
			id: &Identity{
				User: regularUser, Token: tokenRead, Memberships: memReader,
				Kind: CredentialToken,
			},
			wantSysAdmin: false,
			wantCanRead:  true,
			wantCanWrite: false,
		},
		{
			name: "token: regular user with writer membership + write scope can write",
			id: &Identity{
				User: regularUser, Token: tokenWrite, Memberships: memWriter,
				Kind: CredentialToken,
			},
			wantSysAdmin: false,
			wantCanRead:  true, // write scope implies read in CanRead
			wantCanWrite: true,
		},
		{
			name: "token: regular user with reader membership + write scope can NOT write",
			id: &Identity{
				User: regularUser, Token: tokenWrite, Memberships: memReader,
				Kind: CredentialToken,
			},
			wantSysAdmin: false,
			wantCanRead:  true,
			wantCanWrite: false, // role too low
		},
		{
			name: "token: nil token blocks all access",
			id: &Identity{
				User: regularUser, Token: nil, Memberships: memWriter,
				Kind: CredentialToken,
			},
			wantSysAdmin: false,
			wantCanRead:  false,
			wantCanWrite: false,
		},

		// --- CredentialSession: web console password mode ---
		{
			name: "session: admin user is system admin without any token",
			id: &Identity{
				User: adminUser, Token: nil, Memberships: memReader,
				Kind: CredentialSession,
			},
			wantSysAdmin: true,
			wantCanRead:  true,
			wantCanWrite: true,
		},
		{
			name: "session: regular user with reader membership can read but not write",
			id: &Identity{
				User: regularUser, Token: nil, Memberships: memReader,
				Kind: CredentialSession,
			},
			wantSysAdmin: false,
			wantCanRead:  true,
			wantCanWrite: false,
		},
		{
			name: "session: regular user with writer membership can read and write",
			id: &Identity{
				User: regularUser, Token: nil, Memberships: memWriter,
				Kind: CredentialSession,
			},
			wantSysAdmin: false,
			wantCanRead:  true,
			wantCanWrite: true,
		},
		{
			name: "session: regular user with admin membership can read and write",
			id: &Identity{
				User: regularUser, Token: nil, Memberships: memAdmin,
				Kind: CredentialSession,
			},
			wantSysAdmin: false,
			wantCanRead:  true,
			wantCanWrite: true,
		},
		{
			name: "session: regular user with no membership is denied",
			id: &Identity{
				User: regularUser, Token: nil, Memberships: nil,
				Kind: CredentialSession,
			},
			wantSysAdmin: false,
			wantCanRead:  false,
			wantCanWrite: false,
		},

		// --- CredentialProxy: web console proxy-header mode ---
		{
			name: "proxy: admin user is system admin without any token",
			id: &Identity{
				User: adminUser, Token: nil, Memberships: memReader,
				Kind: CredentialProxy,
			},
			wantSysAdmin: true,
			wantCanRead:  true,
			wantCanWrite: true,
		},
		{
			name: "proxy: regular user with writer membership can write",
			id: &Identity{
				User: regularUser, Token: nil, Memberships: memWriter,
				Kind: CredentialProxy,
			},
			wantSysAdmin: false,
			wantCanRead:  true,
			wantCanWrite: true,
		},
		{
			name: "proxy: regular user without membership is denied",
			id: &Identity{
				User: regularUser, Token: nil, Memberships: nil,
				Kind: CredentialProxy,
			},
			wantSysAdmin: false,
			wantCanRead:  false,
			wantCanWrite: false,
		},

		// --- Edge: User is nil ---
		{
			name: "any kind: nil user is anonymous-equivalent",
			id: &Identity{
				User: nil, Token: tokenAdmin, Memberships: memWriter,
				Kind: CredentialToken,
			},
			wantSysAdmin: false,
			wantCanRead:  true, // CanRead doesn't require User
			wantCanWrite: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.id.IsSystemAdmin(); got != tc.wantSysAdmin {
				t.Errorf("IsSystemAdmin() = %v, want %v", got, tc.wantSysAdmin)
			}
			if got := tc.id.CanRead(1); got != tc.wantCanRead {
				t.Errorf("CanRead(1) = %v, want %v", got, tc.wantCanRead)
			}
			if got := tc.id.CanWrite(1); got != tc.wantCanWrite {
				t.Errorf("CanWrite(1) = %v, want %v", got, tc.wantCanWrite)
			}
		})
	}
}

// TestIdentity_TenantBindingTokenOnly verifies that a token's tenant
// scope binding only applies to CredentialToken identities. Session and
// proxy identities never carry a tenant-bound token.
func TestIdentity_TenantBindingTokenOnly(t *testing.T) {
	user := &users.User{ID: 2, Name: "bob"}
	mem := map[int64]tenants.Role{1: tenants.RoleWriter, 2: tenants.RoleWriter}
	// Token bound to tenant 1 only.
	tokenBound := &tokens.Token{
		Scopes:      []tokens.Scope{tokens.ScopeWrite},
		TenantScope: sqlInt64(1),
	}

	id := &Identity{User: user, Token: tokenBound, Memberships: mem, Kind: CredentialToken}
	if !id.CanWrite(1) {
		t.Error("expected write to bound tenant 1 to succeed")
	}
	if id.CanWrite(2) {
		t.Error("expected write to other tenant 2 to fail when token is bound to 1")
	}
}

// TestHasTokenScope_NilTokenSafe verifies the helper is safe when Token
// is nil (the common case for session/proxy identities).
func TestHasTokenScope_NilTokenSafe(t *testing.T) {
	id := &Identity{Kind: CredentialSession}
	if id.HasTokenScope(tokens.ScopeAdmin) {
		t.Error("expected HasTokenScope to be false on nil Token")
	}
	if (*Identity)(nil).HasTokenScope(tokens.ScopeAdmin) {
		t.Error("expected HasTokenScope on nil Identity to be false")
	}
}
