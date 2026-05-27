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
