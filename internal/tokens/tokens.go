// Package tokens stores and verifies opaque bearer/basic auth tokens.
//
// Tokens are opaque, high-entropy random strings of the form
// "pkm_<32 base32 chars>". The plaintext is shown to the user exactly once
// at creation time; the database stores only SHA-256(plaintext) hex for
// constant-time lookup at request time.
//
// SHA-256 (not bcrypt) is the correct primitive here because the input has
// >150 bits of entropy from crypto/rand — rainbow tables and brute force
// are irrelevant. This is the same approach GitHub uses for personal access
// tokens (e.g. "ghp_...").
package tokens

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Scope is a permission a token carries. Multiple scopes per token, CSV-encoded
// in the database.
type Scope string

const (
	// ScopeRead lets the token perform reads against any tenant where the
	// owning user has reader+ membership.
	ScopeRead Scope = "read"
	// ScopeWrite lets the token perform writes against any tenant where the
	// owning user has writer+ membership.
	ScopeWrite Scope = "write"
	// ScopeAdmin grants system-wide admin privileges. Only meaningful if the
	// owning user has users.User.IsAdmin set.
	ScopeAdmin Scope = "admin"
)

// Prefix is the public, machine-recognizable token prefix.
const Prefix = "pkm_"

// randomLen is the number of base32 characters of randomness in a token
// (160 bits of entropy).
const randomLen = 32

// Token is a database row representing a stored token's metadata. The
// plaintext value is never present in this struct after creation.
type Token struct {
	ID            int64
	UserID        int64
	Name          string
	Hash          string
	Scopes        []Scope
	TenantScope   sql.NullInt64 // NULL = use the user's tenant memberships
	CreatedUnix   int64
	LastUsedUnix  int64
	ExpiresUnix   int64
}

// Sentinel errors.
var (
	ErrNotExist = errors.New("token does not exist")
	ErrExpired  = errors.New("token has expired")
)

// Store wraps *sql.DB.
type Store struct{ DB *sql.DB }

// New constructs a Store.
func New(db *sql.DB) *Store { return &Store{DB: db} }

// CreateOptions captures the inputs to Issue.
type CreateOptions struct {
	UserID      int64
	Name        string
	Scopes      []Scope
	TenantScope int64 // 0 means "no per-token tenant restriction"
	ExpiresUnix int64 // 0 means "no expiry"
}

// Issue generates a fresh token, stores its hash, and returns the
// plaintext + the stored row. The caller MUST show the plaintext to the
// user immediately; it cannot be recovered later.
func (s *Store) Issue(ctx context.Context, opts CreateOptions) (plaintext string, t *Token, err error) {
	if opts.UserID == 0 {
		return "", nil, errors.New("tokens.Issue: UserID is required")
	}
	if len(opts.Scopes) == 0 {
		return "", nil, errors.New("tokens.Issue: at least one scope is required")
	}
	plaintext, err = generate()
	if err != nil {
		return "", nil, err
	}
	return s.persist(ctx, opts, plaintext)
}

// IssueWithPlaintext stores a caller-supplied plaintext (useful when the
// operator pins a fixed admin token via environment variable). The plaintext
// must already include the "pkm_" prefix.
func (s *Store) IssueWithPlaintext(ctx context.Context, opts CreateOptions, plaintext string) (*Token, error) {
	if !strings.HasPrefix(plaintext, Prefix) {
		return nil, fmt.Errorf("tokens.IssueWithPlaintext: token must start with %q", Prefix)
	}
	_, t, err := s.persist(ctx, opts, plaintext)
	return t, err
}

func (s *Store) persist(ctx context.Context, opts CreateOptions, plaintext string) (string, *Token, error) {
	hash := Hash(plaintext)
	scopes := scopesCSV(opts.Scopes)
	now := time.Now().Unix()
	var tenantScope any
	if opts.TenantScope > 0 {
		tenantScope = opts.TenantScope
	}
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO tokens
		   (user_id, name, hash, scopes, tenant_scope, created_unix, expires_unix)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		opts.UserID, opts.Name, hash, scopes, tenantScope, now, opts.ExpiresUnix)
	if err != nil {
		return "", nil, fmt.Errorf("insert token: %w", err)
	}
	id, _ := res.LastInsertId()
	t := &Token{
		ID:          id,
		UserID:      opts.UserID,
		Name:        opts.Name,
		Hash:        hash,
		Scopes:      opts.Scopes,
		CreatedUnix: now,
		ExpiresUnix: opts.ExpiresUnix,
	}
	if opts.TenantScope > 0 {
		t.TenantScope = sql.NullInt64{Int64: opts.TenantScope, Valid: true}
	}
	return plaintext, t, nil
}

// Lookup finds a token by its plaintext value, or returns ErrNotExist.
// It also returns ErrExpired if the token is past its expiry.
func (s *Store) Lookup(ctx context.Context, plaintext string) (*Token, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT id, user_id, name, hash, scopes, tenant_scope,
		        created_unix, last_used_unix, expires_unix
		   FROM tokens WHERE hash = ?`, Hash(plaintext))
	t := &Token{}
	var scopesCSV string
	if err := row.Scan(
		&t.ID, &t.UserID, &t.Name, &t.Hash, &scopesCSV, &t.TenantScope,
		&t.CreatedUnix, &t.LastUsedUnix, &t.ExpiresUnix,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotExist
		}
		return nil, err
	}
	t.Scopes = parseScopes(scopesCSV)
	if t.ExpiresUnix != 0 && t.ExpiresUnix < time.Now().Unix() {
		return nil, ErrExpired
	}
	return t, nil
}

// TouchLastUsed updates the last_used_unix column. Non-blocking failures are
// logged by the caller.
func (s *Store) TouchLastUsed(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE tokens SET last_used_unix = ? WHERE id = ?`, time.Now().Unix(), id)
	return err
}

// CountByUser returns the number of tokens owned by a user. Used by the
// bootstrap flow to decide whether to mint an initial admin token.
func (s *Store) CountByUser(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tokens WHERE user_id = ?`, userID).Scan(&n)
	return n, err
}

// HasScope reports whether the token carries the given scope.
func (t *Token) HasScope(s Scope) bool {
	for _, x := range t.Scopes {
		if x == s {
			return true
		}
	}
	return false
}

// Hash returns the lookup hash for a plaintext token.
func Hash(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

func generate() (string, error) {
	// 20 random bytes encoded as unpadded base32 = 32 chars of [a-z2-7].
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("tokens.generate: read random: %w", err)
	}
	body := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
	if len(body) != randomLen {
		return "", fmt.Errorf("tokens.generate: unexpected length %d", len(body))
	}
	return Prefix + body, nil
}

func scopesCSV(scopes []Scope) string {
	if len(scopes) == 0 {
		return ""
	}
	parts := make([]string, 0, len(scopes))
	for _, s := range scopes {
		parts = append(parts, string(s))
	}
	return strings.Join(parts, ",")
}

func parseScopes(csv string) []Scope {
	if csv == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	out := make([]Scope, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, Scope(p))
		}
	}
	return out
}
