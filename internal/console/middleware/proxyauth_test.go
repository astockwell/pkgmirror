package middleware

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"

	pkgdb "github.com/astockwell/pkgmirror/internal/db"
	"github.com/astockwell/pkgmirror/internal/tenants"
	"github.com/astockwell/pkgmirror/internal/users"
)

// proxyFixture spins up a real users + tenants store on a fresh
// sqlite for ProxyHeaderAuthenticator tests.
type proxyFixture struct {
	t     *testing.T
	Users *users.Store
}

func newProxyFixture(t *testing.T) *proxyFixture {
	t.Helper()
	dir := t.TempDir()
	db, err := pkgdb.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &proxyFixture{t: t, Users: users.New(db)}
}

func (f *proxyFixture) auth(opts ProxyHeaderOptions) *ProxyHeaderAuthenticator {
	if opts.Users == nil {
		opts.Users = f.Users
	}
	if opts.Tenants == nil {
		// Tenants is only used for memberships lookup; tests that
		// don't care can use a fresh store backed by the same DB.
		opts.Tenants = tenants.New(opts.Users.DB)
	}
	a := NewProxyHeaderAuthenticator(opts).(*ProxyHeaderAuthenticator)
	return a
}

// --- Trust gate ---

func TestProxyAuth_RejectsUntrustedPeer(t *testing.T) {
	f := newProxyFixture(t)
	a := f.auth(ProxyHeaderOptions{
		TrustedProxies: []string{"10.0.0.0/8"},
		UserHeader:     "X-Forwarded-User",
	})

	r := httptest.NewRequest("GET", "/console/", nil)
	r.RemoteAddr = "203.0.113.5:51234" // untrusted internet client
	r.Header.Set("X-Forwarded-User", "alice")

	id, err := a.Authenticate(context.Background(), r)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if id != nil {
		t.Errorf("expected anonymous (nil identity) for untrusted peer; got %+v", id)
	}

	// And critically: NO user got auto-created.
	if _, err := f.Users.GetByName(context.Background(), "alice"); err == nil {
		t.Error("untrusted-peer header MUST NOT auto-create users")
	}
}

func TestProxyAuth_AcceptsTrustedPeerWithHeader(t *testing.T) {
	f := newProxyFixture(t)
	a := f.auth(ProxyHeaderOptions{
		TrustedProxies: []string{"10.0.0.0/8"},
		UserHeader:     "X-Forwarded-User",
		EmailHeader:    "X-Forwarded-Email",
	})

	r := httptest.NewRequest("GET", "/console/", nil)
	r.RemoteAddr = "10.0.0.5:51234"
	r.Header.Set("X-Forwarded-User", "alice")
	r.Header.Set("X-Forwarded-Email", "alice@example.com")

	id, err := a.Authenticate(context.Background(), r)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id == nil || id.User == nil {
		t.Fatalf("expected hydrated identity; got nil")
	}
	if id.User.Name != "alice" {
		t.Errorf("Name=%q want alice", id.User.Name)
	}
	if !id.User.Email.Valid || id.User.Email.String != "alice@example.com" {
		t.Errorf("Email=%v want alice@example.com", id.User.Email)
	}
	if id.User.IsAdmin {
		t.Error("default user should not be admin")
	}
}

func TestProxyAuth_TrustedPeerWithoutHeaderIsAnonymous(t *testing.T) {
	f := newProxyFixture(t)
	a := f.auth(ProxyHeaderOptions{
		TrustedProxies: []string{"10.0.0.0/8"},
		UserHeader:     "X-Forwarded-User",
	})
	r := httptest.NewRequest("GET", "/console/", nil)
	r.RemoteAddr = "10.0.0.5:51234"
	// no user header

	id, err := a.Authenticate(context.Background(), r)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if id != nil {
		t.Errorf("expected anonymous when header missing; got %+v", id)
	}
}

// --- XFF spoofing ---

// TestProxyAuth_SpoofedXFFDoesNotPromoteSource is plan §11.2s
// "spoofed X-Forwarded-For from an untrusted upstream" row. Even with
// the spoofed XFF making it LOOK like the request came from a trusted
// proxy, the IMMEDIATE peer is what gates trust.
func TestProxyAuth_SpoofedXFFDoesNotPromoteSource(t *testing.T) {
	f := newProxyFixture(t)
	a := f.auth(ProxyHeaderOptions{
		TrustedProxies: []string{"10.0.0.0/8"},
		UserHeader:     "X-Forwarded-User",
	})

	r := httptest.NewRequest("GET", "/console/", nil)
	r.RemoteAddr = "203.0.113.99:51234" // untrusted peer
	r.Header.Set("X-Forwarded-For", "10.0.0.1")
	r.Header.Set("X-Forwarded-User", "alice")

	id, err := a.Authenticate(context.Background(), r)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if id != nil {
		t.Error("spoofed XFF MUST NOT trick the trust gate")
	}
}

// --- Bootstrap admin ---

func TestProxyAuth_BootstrapAdmin_PromotesByName(t *testing.T) {
	f := newProxyFixture(t)
	a := f.auth(ProxyHeaderOptions{
		TrustedProxies: []string{"10.0.0.0/8"},
		UserHeader:     "X-Forwarded-User",
		BootstrapAdmin: "alice",
	})

	r := httptest.NewRequest("GET", "/console/", nil)
	r.RemoteAddr = "10.0.0.5:51234"
	r.Header.Set("X-Forwarded-User", "alice")

	id, err := a.Authenticate(context.Background(), r)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !id.User.IsAdmin {
		t.Error("expected first sign-in to promote to admin")
	}

	// Re-read from DB to confirm the promotion actually persisted.
	fresh, _ := f.Users.GetByID(context.Background(), id.User.ID)
	if !fresh.IsAdmin {
		t.Error("expected IsAdmin to persist in DB after promotion")
	}
}

func TestProxyAuth_BootstrapAdmin_PromotesByEmail(t *testing.T) {
	f := newProxyFixture(t)
	a := f.auth(ProxyHeaderOptions{
		TrustedProxies: []string{"10.0.0.0/8"},
		UserHeader:     "X-Forwarded-User",
		EmailHeader:    "X-Forwarded-Email",
		BootstrapAdmin: "alice@example.com",
	})

	r := httptest.NewRequest("GET", "/console/", nil)
	r.RemoteAddr = "10.0.0.5:51234"
	r.Header.Set("X-Forwarded-User", "alice")
	r.Header.Set("X-Forwarded-Email", "alice@example.com")

	id, err := a.Authenticate(context.Background(), r)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !id.User.IsAdmin {
		t.Error("expected email-match to promote")
	}
}

func TestProxyAuth_BootstrapAdmin_CaseInsensitive(t *testing.T) {
	f := newProxyFixture(t)
	a := f.auth(ProxyHeaderOptions{
		TrustedProxies: []string{"10.0.0.0/8"},
		UserHeader:     "X-Forwarded-User",
		BootstrapAdmin: "ALICE",
	})
	r := httptest.NewRequest("GET", "/console/", nil)
	r.RemoteAddr = "10.0.0.5:51234"
	r.Header.Set("X-Forwarded-User", "alice")

	id, err := a.Authenticate(context.Background(), r)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !id.User.IsAdmin {
		t.Error("case-insensitive match should promote")
	}
}

// TestProxyAuth_BootstrapAdmin_ConvergentOnRepeatedSignIn: once
// promoted, subsequent sign-ins are no-ops (no double promotion, no
// duplicated audit row spam if we add one — we just don't trigger).
func TestProxyAuth_BootstrapAdmin_ConvergentOnRepeatedSignIn(t *testing.T) {
	f := newProxyFixture(t)
	promoteCount := 0
	a := f.auth(ProxyHeaderOptions{
		TrustedProxies: []string{"10.0.0.0/8"},
		UserHeader:     "X-Forwarded-User",
		BootstrapAdmin: "alice",
	})
	// Wrap the Promote func to count calls.
	a.Promote = func(ctx context.Context, userID int64) error {
		promoteCount++
		_, err := a.Users.DB.ExecContext(ctx, `UPDATE users SET is_admin = 1 WHERE id = ?`, userID)
		return err
	}

	r := httptest.NewRequest("GET", "/console/", nil)
	r.RemoteAddr = "10.0.0.5:51234"
	r.Header.Set("X-Forwarded-User", "alice")

	for i := 0; i < 3; i++ {
		_, err := a.Authenticate(context.Background(), r)
		if err != nil {
			t.Fatalf("err on round %d: %v", i, err)
		}
	}
	if promoteCount != 1 {
		t.Errorf("Promote called %d times; want exactly 1 (convergent)", promoteCount)
	}
}

func TestProxyAuth_BootstrapAdmin_DoesNotPromoteOthers(t *testing.T) {
	f := newProxyFixture(t)
	a := f.auth(ProxyHeaderOptions{
		TrustedProxies: []string{"10.0.0.0/8"},
		UserHeader:     "X-Forwarded-User",
		BootstrapAdmin: "alice",
	})

	r := httptest.NewRequest("GET", "/console/", nil)
	r.RemoteAddr = "10.0.0.5:51234"
	r.Header.Set("X-Forwarded-User", "mallory")

	id, err := a.Authenticate(context.Background(), r)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if id.User.IsAdmin {
		t.Error("non-matching user should NOT be promoted")
	}
}

// --- Empty TrustedProxies ---

func TestProxyAuth_EmptyTrustedProxiesNeverAccepts(t *testing.T) {
	f := newProxyFixture(t)
	a := f.auth(ProxyHeaderOptions{
		TrustedProxies: nil,
		UserHeader:     "X-Forwarded-User",
	})
	r := httptest.NewRequest("GET", "/console/", nil)
	r.RemoteAddr = "127.0.0.1:51234" // even localhost
	r.Header.Set("X-Forwarded-User", "alice")

	id, err := a.Authenticate(context.Background(), r)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if id != nil {
		t.Error("with empty TrustedProxies the trust gate must reject everything")
	}
}
