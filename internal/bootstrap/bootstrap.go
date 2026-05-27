// Package bootstrap performs first-boot setup: creates a default tenant
// (if missing), an "admin" service-account user (if missing), and an
// initial admin token (if no admin token exists yet).
package bootstrap

import (
	"context"
	"errors"
	"fmt"

	"github.com/astockwell/pkgmirror/internal/tenants"
	"github.com/astockwell/pkgmirror/internal/tokens"
	"github.com/astockwell/pkgmirror/internal/users"
)

// Options controls bootstrap behavior.
type Options struct {
	// DefaultTenantName is the tenant created (or used) at boot.
	// If empty, defaults to "default".
	DefaultTenantName string

	// DefaultTenantVisibility is applied only if the default tenant is being
	// created for the first time.
	DefaultTenantVisibility tenants.Visibility

	// AdminToken is the plaintext token to install as the admin token. If
	// empty AND no admin token exists yet, one is generated and returned
	// via Result.GeneratedAdminToken.
	AdminToken string
}

// Result reports what bootstrap did.
type Result struct {
	// DefaultTenant is always populated.
	DefaultTenant *tenants.Tenant

	// AdminUser is always populated.
	AdminUser *users.User

	// GeneratedAdminToken is set ONLY when bootstrap minted a fresh admin
	// token (i.e. there was no existing admin token AND Options.AdminToken
	// was empty). Callers are expected to display this once to the operator.
	GeneratedAdminToken string
}

// Ensure runs the bootstrap. It is idempotent.
func Ensure(
	ctx context.Context,
	ts *tenants.Store,
	us *users.Store,
	tk *tokens.Store,
	opts Options,
) (*Result, error) {
	name := opts.DefaultTenantName
	if name == "" {
		name = "default"
	}

	tenant, err := ts.GetByName(ctx, name)
	if err != nil {
		if !errors.Is(err, tenants.ErrNotExist) {
			return nil, fmt.Errorf("bootstrap: lookup default tenant: %w", err)
		}
		tenant, err = ts.Create(ctx, name, opts.DefaultTenantVisibility)
		if err != nil {
			return nil, fmt.Errorf("bootstrap: create default tenant: %w", err)
		}
	} else if tenant.Visibility != opts.DefaultTenantVisibility {
		// The v2 migration seeds the default tenant as private. Honor the
		// operator's PKGMIRROR_DEFAULT_TENANT_VISIBILITY env var by
		// synchronizing the row on every boot.
		if err := ts.SetVisibility(ctx, tenant.ID, opts.DefaultTenantVisibility); err != nil {
			return nil, fmt.Errorf("bootstrap: update default tenant visibility: %w", err)
		}
		tenant.Visibility = opts.DefaultTenantVisibility
	}

	admin, err := us.GetByName(ctx, "admin")
	if err != nil {
		if !errors.Is(err, users.ErrNotExist) {
			return nil, fmt.Errorf("bootstrap: lookup admin: %w", err)
		}
		admin, err = us.Create(ctx, users.CreateOptions{
			Name:    "admin",
			Kind:    users.KindService,
			IsAdmin: true,
		})
		if err != nil {
			return nil, fmt.Errorf("bootstrap: create admin: %w", err)
		}
	}

	if err := ts.AddMember(ctx, tenant.ID, admin.ID, tenants.RoleTenantAdmin); err != nil {
		return nil, fmt.Errorf("bootstrap: add admin membership: %w", err)
	}

	result := &Result{DefaultTenant: tenant, AdminUser: admin}

	count, err := tk.CountByUser(ctx, admin.ID)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: count admin tokens: %w", err)
	}
	if count == 0 {
		issueOpts := tokens.CreateOptions{
			UserID: admin.ID,
			Name:   "bootstrap-admin",
			Scopes: []tokens.Scope{tokens.ScopeRead, tokens.ScopeWrite, tokens.ScopeAdmin},
		}
		if opts.AdminToken != "" {
			if _, err := tk.IssueWithPlaintext(ctx, issueOpts, opts.AdminToken); err != nil {
				return nil, fmt.Errorf("bootstrap: install supplied admin token: %w", err)
			}
		} else {
			plaintext, _, err := tk.Issue(ctx, issueOpts)
			if err != nil {
				return nil, fmt.Errorf("bootstrap: issue admin token: %w", err)
			}
			result.GeneratedAdminToken = plaintext
		}
	}

	return result, nil
}
