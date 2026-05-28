// Package tenants is the data layer for tenants and their member assignments.
package tenants

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Visibility controls anonymous read access to a tenant.
type Visibility int

const (
	// VisibilityPrivate requires an authenticated identity with read access.
	VisibilityPrivate Visibility = 0
	// VisibilityPublic allows anonymous reads.
	VisibilityPublic Visibility = 1
)

// Role describes a user's permission level inside a single tenant.
type Role int

const (
	RoleReader      Role = 0
	RoleWriter      Role = 1
	RoleTenantAdmin Role = 2
)

// Tenant is a namespace for packages.
type Tenant struct {
	ID          int64
	Name        string
	LowerName   string
	Visibility  Visibility
	CreatedUnix int64
}

// Sentinel errors.
var (
	ErrNotExist  = errors.New("tenant does not exist")
	ErrDuplicate = errors.New("tenant already exists")
)

// Store wraps a *sql.DB and exposes typed tenant operations.
type Store struct{ DB *sql.DB }

// New constructs a Store.
func New(db *sql.DB) *Store { return &Store{DB: db} }

// Create inserts a new tenant.
func (s *Store) Create(ctx context.Context, name string, vis Visibility) (*Tenant, error) {
	lower := strings.ToLower(name)
	now := time.Now().Unix()
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO tenants (name, lower_name, visibility, created_unix) VALUES (?, ?, ?, ?)`,
		name, lower, int(vis), now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return nil, ErrDuplicate
		}
		return nil, fmt.Errorf("insert tenant: %w", err)
	}
	id, _ := res.LastInsertId()
	return &Tenant{ID: id, Name: name, LowerName: lower, Visibility: vis, CreatedUnix: now}, nil
}

// GetByName looks up a tenant case-insensitively by name.
func (s *Store) GetByName(ctx context.Context, name string) (*Tenant, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT id, name, lower_name, visibility, created_unix
		   FROM tenants WHERE lower_name = ?`, strings.ToLower(name))
	t := &Tenant{}
	var vis int
	if err := row.Scan(&t.ID, &t.Name, &t.LowerName, &vis, &t.CreatedUnix); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotExist
		}
		return nil, err
	}
	t.Visibility = Visibility(vis)
	return t, nil
}

// GetByID looks up a tenant by ID.
func (s *Store) GetByID(ctx context.Context, id int64) (*Tenant, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT id, name, lower_name, visibility, created_unix
		   FROM tenants WHERE id = ?`, id)
	t := &Tenant{}
	var vis int
	if err := row.Scan(&t.ID, &t.Name, &t.LowerName, &vis, &t.CreatedUnix); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotExist
		}
		return nil, err
	}
	t.Visibility = Visibility(vis)
	return t, nil
}

// List returns all tenants ordered by lower_name.
func (s *Store) List(ctx context.Context) ([]*Tenant, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, name, lower_name, visibility, created_unix FROM tenants ORDER BY lower_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Tenant
	for rows.Next() {
		t := &Tenant{}
		var vis int
		if err := rows.Scan(&t.ID, &t.Name, &t.LowerName, &vis, &t.CreatedUnix); err != nil {
			return nil, err
		}
		t.Visibility = Visibility(vis)
		out = append(out, t)
	}
	return out, rows.Err()
}

// AddMember adds a user to a tenant with the given role. If the membership
// already exists, the role is updated.
func (s *Store) AddMember(ctx context.Context, tenantID, userID int64, role Role) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO tenant_members (tenant_id, user_id, role) VALUES (?, ?, ?)
		 ON CONFLICT(tenant_id, user_id) DO UPDATE SET role = excluded.role`,
		tenantID, userID, int(role))
	return err
}

// SetVisibility updates a tenant's visibility.
func (s *Store) SetVisibility(ctx context.Context, tenantID int64, vis Visibility) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE tenants SET visibility = ? WHERE id = ?`, int(vis), tenantID)
	return err
}

// AuditReadsEnabled reports whether the tenant has opted into auditing
// successful read events. Used by the audit-wrapping policy engine to
// gate the high-volume "successful download" stream. Tenants that don't
// exist (or transient DB errors) return false; an unaudit-able tenant is
// preferable to a failed request.
func (s *Store) AuditReadsEnabled(ctx context.Context, tenantID int64) bool {
	if tenantID == 0 {
		return false
	}
	var v int
	err := s.DB.QueryRowContext(ctx,
		`SELECT audit_reads FROM tenants WHERE id = ?`, tenantID).Scan(&v)
	if err != nil {
		return false
	}
	return v != 0
}

// ResolveTenantID returns the ID of the named tenant. Used by the
// policy YAML loader to translate tenant: foo into a tenant_id.
func (s *Store) ResolveTenantID(ctx context.Context, name string) (int64, error) {
	t, err := s.GetByName(ctx, name)
	if err != nil {
		return 0, err
	}
	return t.ID, nil
}

// Memberships returns the (tenantID -> role) map for a user.
func (s *Store) Memberships(ctx context.Context, userID int64) (map[int64]Role, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT tenant_id, role FROM tenant_members WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]Role{}
	for rows.Next() {
		var tid int64
		var role int
		if err := rows.Scan(&tid, &role); err != nil {
			return nil, err
		}
		out[tid] = Role(role)
	}
	return out, rows.Err()
}
