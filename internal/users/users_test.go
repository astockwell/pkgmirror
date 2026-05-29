package users_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	pkgdb "github.com/astockwell/pkgmirror/internal/db"
	"github.com/astockwell/pkgmirror/internal/users"
)

// newStore opens a fresh sqlite db at v4 (via the migration chain) and
// returns a users.Store backed by it. Cleanup closes the DB and
// os.RemoveAll's the temp dir explicitly per the macOS APFS sqlite-
// cleanup-race convention in .github/copilot-instructions.md.
func newStore(t *testing.T) *users.Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := pkgdb.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})
	return users.New(db)
}

func TestSchemaV4_AddsColumns(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	u, err := store.Create(ctx, users.CreateOptions{
		Name: "alice", Kind: users.KindHuman, Email: "alice@example.com",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Verify the v4 columns are present + queryable. The SetPasswordHash
	// + ClearPassword paths exercise both columns end to end.
	if err := store.SetPasswordHash(ctx, u.ID, "$argon2id$v=19$m=65536,t=1,p=4$AAAA$BBBB"); err != nil {
		t.Fatalf("SetPasswordHash: %v", err)
	}
	if err := store.ClearPassword(ctx, u.ID); err != nil {
		t.Fatalf("ClearPassword: %v", err)
	}
}

func TestGetOrCreate_CreatesNewUser(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	u, err := store.GetOrCreate(ctx, "bob", "bob@example.com")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if u.Name != "bob" {
		t.Errorf("Name = %q, want bob", u.Name)
	}
	if !u.Email.Valid || u.Email.String != "bob@example.com" {
		t.Errorf("Email = %v, want bob@example.com", u.Email)
	}
	if u.Kind != users.KindHuman {
		t.Errorf("Kind = %v, want KindHuman", u.Kind)
	}
	if u.IsAdmin {
		t.Errorf("expected IsAdmin=false on first sight")
	}
}

func TestGetOrCreate_IsIdempotent(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	first, err := store.GetOrCreate(ctx, "charlie", "charlie@example.com")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := store.GetOrCreate(ctx, "charlie", "charlie@example.com")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("expected same ID on second call: first=%d second=%d", first.ID, second.ID)
	}
}

func TestGetOrCreate_RejectsEmptyName(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	if _, err := store.GetOrCreate(ctx, "", ""); err == nil {
		t.Error("expected error for empty name")
	}
}

func TestSetPasswordHash_RoundTripsWithVerify(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	u, err := store.Create(ctx, users.CreateOptions{Name: "dave"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	hash, err := users.HashPassword("hunter2-correct-horse")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := store.SetPasswordHash(ctx, u.ID, hash); err != nil {
		t.Fatalf("SetPasswordHash: %v", err)
	}

	// Right password succeeds and returns the user.
	got, err := store.VerifyPassword(ctx, "dave", "hunter2-correct-horse")
	if err != nil {
		t.Fatalf("VerifyPassword (correct): %v", err)
	}
	if got.ID != u.ID {
		t.Errorf("verify returned wrong user: got %d, want %d", got.ID, u.ID)
	}

	// Wrong password returns ErrPasswordMismatch.
	if _, err := store.VerifyPassword(ctx, "dave", "wrong"); !errors.Is(err, users.ErrPasswordMismatch) {
		t.Errorf("expected ErrPasswordMismatch, got %v", err)
	}
}

func TestSetPasswordHash_RejectsEmpty(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	u, _ := store.Create(ctx, users.CreateOptions{Name: "eve"})
	if err := store.SetPasswordHash(ctx, u.ID, ""); err == nil {
		t.Error("expected error for empty hash")
	}
}

func TestSetPasswordHash_ReturnsErrNotExistForUnknownUser(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	hash, _ := users.HashPassword("x")
	if err := store.SetPasswordHash(ctx, 99999, hash); !errors.Is(err, users.ErrNotExist) {
		t.Errorf("expected ErrNotExist, got %v", err)
	}
}

func TestClearPassword_RemovesHash(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	u, _ := store.Create(ctx, users.CreateOptions{Name: "frank"})
	hash, _ := users.HashPassword("x")
	_ = store.SetPasswordHash(ctx, u.ID, hash)

	if err := store.ClearPassword(ctx, u.ID); err != nil {
		t.Fatalf("ClearPassword: %v", err)
	}

	// Subsequent VerifyPassword returns ErrNoPassword.
	if _, err := store.VerifyPassword(ctx, "frank", "x"); !errors.Is(err, users.ErrNoPassword) {
		t.Errorf("expected ErrNoPassword after clear, got %v", err)
	}
}

func TestClearPassword_ReturnsErrNotExistForUnknownUser(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	if err := store.ClearPassword(ctx, 99999); !errors.Is(err, users.ErrNotExist) {
		t.Errorf("expected ErrNotExist, got %v", err)
	}
}

func TestVerifyPassword_NoUserReturnsNotExist(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	if _, err := store.VerifyPassword(ctx, "ghost", "x"); !errors.Is(err, users.ErrNotExist) {
		t.Errorf("expected ErrNotExist, got %v", err)
	}
}

func TestVerifyPassword_UserWithoutPasswordReturnsNoPassword(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	_, _ = store.Create(ctx, users.CreateOptions{Name: "hank"})
	if _, err := store.VerifyPassword(ctx, "hank", "x"); !errors.Is(err, users.ErrNoPassword) {
		t.Errorf("expected ErrNoPassword, got %v", err)
	}
}

// ---- argon2id helper tests (no DB needed) ----

func TestHashPassword_Roundtrip(t *testing.T) {
	hash, err := users.HashPassword("super-secret")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := users.VerifyHash(hash, "super-secret"); err != nil {
		t.Errorf("VerifyHash (correct): %v", err)
	}
	if err := users.VerifyHash(hash, "wrong"); !errors.Is(err, users.ErrPasswordMismatch) {
		t.Errorf("expected ErrPasswordMismatch, got %v", err)
	}
}

func TestHashPassword_DifferentSaltsEveryCall(t *testing.T) {
	// Same plaintext should produce DIFFERENT hashes (salt is random).
	// Both should still verify against the same plaintext.
	a, _ := users.HashPassword("same-password")
	b, _ := users.HashPassword("same-password")
	if a == b {
		t.Error("expected different hashes for the same plaintext (salts must differ)")
	}
	if err := users.VerifyHash(a, "same-password"); err != nil {
		t.Errorf("first hash failed to verify: %v", err)
	}
	if err := users.VerifyHash(b, "same-password"); err != nil {
		t.Errorf("second hash failed to verify: %v", err)
	}
}

func TestHashPassword_RejectsEmpty(t *testing.T) {
	if _, err := users.HashPassword(""); err == nil {
		t.Error("expected error for empty plaintext")
	}
}

func TestVerifyHash_RejectsMalformed(t *testing.T) {
	cases := []string{
		"",
		"not-a-hash",
		"$argon2id$",
		"$argon2id$bogus",
		"$argon2id$v=19$m=65536,t=1,p=4$nosalt", // missing hash segment
	}
	for _, in := range cases {
		if err := users.VerifyHash(in, "x"); err == nil {
			t.Errorf("expected error for malformed input %q", in)
		}
	}
}
