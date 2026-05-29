// Package users is the data layer for principals (humans + service accounts).
package users

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Kind distinguishes humans from service accounts (CI bots, deploy tokens).
// Service accounts have no email and typically authenticate only via tokens.
type Kind int

const (
	KindHuman   Kind = 0
	KindService Kind = 1
)

// User is a principal.
type User struct {
	ID               int64
	Name             string
	LowerName        string
	Kind             Kind
	Email            sql.NullString
	IsAdmin          bool
	ExternalProvider sql.NullString
	ExternalSubject  sql.NullString
	CreatedUnix      int64
	LastSeenUnix     int64
}

// Sentinel errors.
var (
	ErrNotExist  = errors.New("user does not exist")
	ErrDuplicate = errors.New("user already exists")
)

// Store wraps *sql.DB.
type Store struct{ DB *sql.DB }

// New constructs a Store.
func New(db *sql.DB) *Store { return &Store{DB: db} }

// CreateOptions captures the inputs to Create.
type CreateOptions struct {
	Name             string
	Kind             Kind
	Email            string // optional; empty -> NULL
	IsAdmin          bool
	ExternalProvider string // optional; empty -> NULL
	ExternalSubject  string // optional; empty -> NULL
}

// Create inserts a new user.
func (s *Store) Create(ctx context.Context, opts CreateOptions) (*User, error) {
	lower := strings.ToLower(opts.Name)
	now := time.Now().Unix()
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO users
		   (name, lower_name, kind, email, is_admin, external_provider, external_subject, created_unix)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		opts.Name, lower, int(opts.Kind),
		nullString(opts.Email),
		boolToInt(opts.IsAdmin),
		nullString(opts.ExternalProvider),
		nullString(opts.ExternalSubject),
		now,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return nil, ErrDuplicate
		}
		return nil, fmt.Errorf("insert user: %w", err)
	}
	id, _ := res.LastInsertId()
	return s.GetByID(ctx, id)
}

// GetByID looks up a user by ID.
func (s *Store) GetByID(ctx context.Context, id int64) (*User, error) {
	return s.scanOne(s.DB.QueryRowContext(ctx, baseSelect+` WHERE id = ?`, id))
}

// GetByName looks up a user by name (case-insensitive).
func (s *Store) GetByName(ctx context.Context, name string) (*User, error) {
	return s.scanOne(s.DB.QueryRowContext(ctx, baseSelect+` WHERE lower_name = ?`, strings.ToLower(name)))
}

// GetByExternal looks up a user by their IdP (provider, subject).
func (s *Store) GetByExternal(ctx context.Context, provider, subject string) (*User, error) {
	return s.scanOne(s.DB.QueryRowContext(ctx,
		baseSelect+` WHERE external_provider = ? AND external_subject = ?`,
		provider, subject))
}

// TouchLastSeen updates the last_seen_unix timestamp.
func (s *Store) TouchLastSeen(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE users SET last_seen_unix = ? WHERE id = ?`, time.Now().Unix(), id)
	return err
}

// GetOrCreate looks up a user by name; creates one if not found. Used
// by the console's proxy-header authenticator on first sight of a new
// upstream-provided username.
//
// The returned user has Kind=KindHuman, IsAdmin=false; promotion to
// admin is a separate step (the console's bootstrap-admin env var or a
// system admin clicking promote in the UI).
//
// Note this races with concurrent first-sight requests for the same
// name: if two land at once, one wins the unique constraint, the other
// retries the read. We catch ErrDuplicate explicitly and re-read.
func (s *Store) GetOrCreate(ctx context.Context, name, email string) (*User, error) {
	if name == "" {
		return nil, fmt.Errorf("user name is empty")
	}
	u, err := s.GetByName(ctx, name)
	if err == nil {
		return u, nil
	}
	if !errors.Is(err, ErrNotExist) {
		return nil, err
	}
	u, err = s.Create(ctx, CreateOptions{
		Name:  name,
		Kind:  KindHuman,
		Email: email,
	})
	if err == nil {
		return u, nil
	}
	if errors.Is(err, ErrDuplicate) {
		// Lost the race; re-read.
		return s.GetByName(ctx, name)
	}
	return nil, err
}

// SetPasswordHash writes the supplied (already-encoded) argon2id hash
// to the user's row. The caller is responsible for producing the hash
// via HashPassword — keeping argon2id usage outside the store means
// the cost parameters can rotate without touching the DB layer.
func (s *Store) SetPasswordHash(ctx context.Context, id int64, hashed string) error {
	if hashed == "" {
		return fmt.Errorf("password hash is empty")
	}
	now := time.Now().Unix()
	res, err := s.DB.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, password_set_unix = ? WHERE id = ?`,
		hashed, now, id)
	if err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotExist
	}
	return nil
}

// ClearPassword wipes the password columns. Used when a system admin
// resets a user's account back to the no-password state (the user must
// then re-bootstrap via the console's set-password flow).
func (s *Store) ClearPassword(ctx context.Context, id int64) error {
	res, err := s.DB.ExecContext(ctx,
		`UPDATE users SET password_hash = NULL, password_set_unix = NULL WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("clear password: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotExist
	}
	return nil
}

// VerifyPassword looks up the user by name and verifies plaintext
// against the stored argon2id hash. Returns the user on success.
//
// Error semantics — callers MUST map them all to the same opaque
// "authentication failed" message to avoid leaking which step failed:
//
//   - ErrNotExist: no user with that name
//   - ErrNoPassword: user exists but has no password set
//   - ErrPasswordMismatch: user exists, has a password, plaintext is wrong
//   - any other error: DB / argon2 / hash-format failure → 500
func (s *Store) VerifyPassword(ctx context.Context, name, plaintext string) (*User, error) {
	u, err := s.GetByName(ctx, name)
	if err != nil {
		return nil, err
	}
	var hash sql.NullString
	if err := s.DB.QueryRowContext(ctx,
		`SELECT password_hash FROM users WHERE id = ?`, u.ID).Scan(&hash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotExist
		}
		return nil, fmt.Errorf("read password hash: %w", err)
	}
	if !hash.Valid || hash.String == "" {
		return nil, ErrNoPassword
	}
	if err := VerifyHash(hash.String, plaintext); err != nil {
		return nil, err
	}
	return u, nil
}

const baseSelect = `SELECT id, name, lower_name, kind, email, is_admin,
       external_provider, external_subject, created_unix, last_seen_unix
  FROM users`

func (s *Store) scanOne(row *sql.Row) (*User, error) {
	u := &User{}
	var kind, isAdmin int
	if err := row.Scan(
		&u.ID, &u.Name, &u.LowerName, &kind, &u.Email, &isAdmin,
		&u.ExternalProvider, &u.ExternalSubject, &u.CreatedUnix, &u.LastSeenUnix,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotExist
		}
		return nil, err
	}
	u.Kind = Kind(kind)
	u.IsAdmin = isAdmin != 0
	return u, nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
