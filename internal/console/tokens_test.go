package console_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/astockwell/pkgmirror/internal/tokens"
	"github.com/astockwell/pkgmirror/internal/users"
)

func TestProfile_RendersForSignedInAdmin(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	f.loginAs(t, "alice", "correct-password-12chars")

	resp, body := f.Get(t, "/console/profile")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	for _, want := range []string{"Profile", "alice", "Memberships", "Change password"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in body", want)
		}
	}
}

func TestProfile_ChangePassword(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	f.loginAs(t, "alice", "correct-password-12chars")

	resp := f.PostFormFrom(t, "/console/profile", "/console/profile/password", url.Values{
		"current_password": {"correct-password-12chars"},
		"new_password":     {"new-stronger-password-22"},
		"confirm_password": {"new-stronger-password-22"},
	})
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("change pw: expected 303, got %d", resp.StatusCode)
	}
	// Verify with the new password.
	_, err := f.Users.VerifyPassword(context.Background(), "alice", "new-stronger-password-22")
	if err != nil {
		t.Errorf("VerifyPassword with new pw: %v", err)
	}
}

func TestProfile_ChangePassword_RejectsWrongCurrent(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	f.loginAs(t, "alice", "correct-password-12chars")

	resp := f.PostFormFrom(t, "/console/profile", "/console/profile/password", url.Values{
		"current_password": {"wrong"},
		"new_password":     {"new-stronger-password-22"},
		"confirm_password": {"new-stronger-password-22"},
	})
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	// Failure path uses flash + 303 back to profile.
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", resp.StatusCode)
	}
	// Original password still works.
	_, err := f.Users.VerifyPassword(context.Background(), "alice", "correct-password-12chars")
	if err != nil {
		t.Errorf("original password should still verify; got %v", err)
	}
}

func TestTokens_ListEmptyState(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	f.loginAs(t, "alice", "correct-password-12chars")

	_, body := f.Get(t, "/console/tokens")
	if !strings.Contains(body, "No tokens") {
		t.Errorf("expected empty-state; got %s", body)
	}
}

func TestToken_MintAndReveal(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	f.loginAs(t, "alice", "correct-password-12chars")

	resp := f.PostFormFrom(t, "/console/tokens", "/console/tokens", url.Values{
		"name":  {"ci-token"},
		"scope": {"read", "write"},
	})
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("mint: expected 303, got %d", resp.StatusCode)
	}

	// Follow the redirect; the page should render the new plaintext once.
	_, body := f.Get(t, "/console/tokens")
	if !strings.Contains(body, "ci-token") {
		t.Errorf("expected ci-token in body")
	}
	if !strings.Contains(body, "pkm_") {
		t.Errorf("expected one-shot plaintext (pkm_...) in body; got %s", body)
	}

	// The second GET should NOT show the plaintext again (one-shot).
	_, body2 := f.Get(t, "/console/tokens")
	if strings.Contains(body2, "pkm_") {
		t.Error("expected plaintext to be revealed exactly once")
	}
}

func TestToken_RevokeOwn(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	// Seed a token directly.
	plain, tok, err := f.Tokens.Issue(context.Background(), tokens.CreateOptions{
		UserID: 1, Name: "to-revoke",
		Scopes: []tokens.Scope{tokens.ScopeRead},
	})
	_ = plain
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	f.loginAs(t, "alice", "correct-password-12chars")

	resp := f.PostFormFrom(t, "/console/tokens", "/console/tokens/"+itoa(tok.ID)+"/delete", url.Values{})
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("revoke: expected 303, got %d", resp.StatusCode)
	}
	if _, err := f.Tokens.GetByID(context.Background(), tok.ID); err == nil {
		t.Error("expected token to be revoked")
	}
}

func TestToken_CantRevokeOtherUsers(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	// Create bob + a token owned by bob.
	bob, _ := f.Users.Create(context.Background(), users.CreateOptions{Name: "bob"})
	_, bobTok, _ := f.Tokens.Issue(context.Background(), tokens.CreateOptions{
		UserID: bob.ID, Name: "bobs-token",
		Scopes: []tokens.Scope{tokens.ScopeRead},
	})
	// Alice is admin so SHE can revoke bob's token. Test as non-admin.
	hashCarol, _ := users.HashPassword("correct-password-12chars")
	carol, _ := f.Users.Create(context.Background(), users.CreateOptions{Name: "carol"})
	_ = f.Users.SetPasswordHash(context.Background(), carol.ID, hashCarol)
	f.loginAs(t, "carol", "correct-password-12chars")

	resp := f.PostFormFrom(t, "/console/tokens", "/console/tokens/"+itoa(bobTok.ID)+"/delete", url.Values{})
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 when revoking another user's token, got %d", resp.StatusCode)
	}
}
