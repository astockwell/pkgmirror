package console_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/astockwell/pkgmirror/internal/tenants"
	"github.com/astockwell/pkgmirror/internal/users"
)

func (f *authFixture) loginAs(t *testing.T, name, password string) {
	t.Helper()
	resp := f.PostForm(t, "/console/login", url.Values{
		"username": {name}, "password": {password},
	})
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login %s = %d", name, resp.StatusCode)
	}
}

func TestTenantsList_AdminSeesEverything(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	// Create two tenants (default already exists from migration v2).
	_, _ = f.Tenants.Create(context.Background(), "alpha", tenants.VisibilityPublic)
	_, _ = f.Tenants.Create(context.Background(), "beta", tenants.VisibilityPrivate)

	f.loginAs(t, "alice", "correct-password-12chars")

	resp, body := f.Get(t, "/console/tenants")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d", resp.StatusCode)
	}
	for _, want := range []string{"alpha", "beta", "default", "public", "private"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in list body", want)
		}
	}
}

func TestTenantsList_NonMemberSeesEmptyState(t *testing.T) {
	f := newAuthFixture(t)
	// Create a non-admin user with no memberships.
	u, err := f.Users.Create(context.Background(), users.CreateOptions{Name: "bob"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	hash, _ := users.HashPassword("correct-password-12chars")
	_ = f.Users.SetPasswordHash(context.Background(), u.ID, hash)
	_, _ = f.Tenants.Create(context.Background(), "private1", tenants.VisibilityPrivate)

	f.loginAs(t, "bob", "correct-password-12chars")

	_, body := f.Get(t, "/console/tenants")
	if !strings.Contains(body, "don&#39;t have access") {
		t.Errorf("expected empty-state for non-member; got %s", body)
	}
}

func TestTenantCreate_HappyPath(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	f.loginAs(t, "alice", "correct-password-12chars")

	resp := f.PostFormFrom(t, "/console/tenants/new", "/console/tenants", url.Values{
		"name":       {"acme"},
		"visibility": {"public"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create expected 303, got %d; body=%s", resp.StatusCode, body)
	}
	if loc := resp.Header.Get("Location"); loc != "/console/tenants/acme" {
		t.Errorf("expected redirect to detail, got %q", loc)
	}
	// Sanity: tenant exists in DB.
	got, err := f.Tenants.GetByName(context.Background(), "acme")
	if err != nil {
		t.Fatalf("GetByName after create: %v", err)
	}
	if got.Visibility != tenants.VisibilityPublic {
		t.Errorf("Visibility=%v want public", got.Visibility)
	}
}

func TestTenantCreate_RejectsInvalidName(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	f.loginAs(t, "alice", "correct-password-12chars")

	resp := f.PostFormFrom(t, "/console/tenants/new", "/console/tenants", url.Values{
		"name":       {"has spaces"},
		"visibility": {"private"},
	})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected re-render, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "Letters, digits") {
		t.Errorf("expected name validation message; got %s", body)
	}
}

func TestTenantCreate_RejectsDuplicateName(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	_, _ = f.Tenants.Create(context.Background(), "exists", tenants.VisibilityPrivate)
	f.loginAs(t, "alice", "correct-password-12chars")

	resp := f.PostFormFrom(t, "/console/tenants/new", "/console/tenants", url.Values{
		"name":       {"exists"},
		"visibility": {"private"},
	})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "already exists") {
		t.Errorf("expected duplicate message; got %s", body)
	}
}

func TestTenantNew_NonAdminGet403(t *testing.T) {
	f := newAuthFixture(t)
	u, _ := f.Users.Create(context.Background(), users.CreateOptions{Name: "bob"})
	hash, _ := users.HashPassword("correct-password-12chars")
	_ = f.Users.SetPasswordHash(context.Background(), u.ID, hash)
	f.loginAs(t, "bob", "correct-password-12chars")

	resp, _ := f.Get(t, "/console/tenants/new")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for non-admin GET /tenants/new, got %d", resp.StatusCode)
	}
}

func TestTenantDetail_404OnMissing(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	f.loginAs(t, "alice", "correct-password-12chars")

	resp, _ := f.Get(t, "/console/tenants/does-not-exist")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestTenantMember_AddAndRemove(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	bob, _ := f.Users.Create(context.Background(), users.CreateOptions{Name: "bob"})
	team, _ := f.Tenants.Create(context.Background(), "team", tenants.VisibilityPrivate)
	f.loginAs(t, "alice", "correct-password-12chars")

	// Add member.
	addResp := f.PostFormFrom(t, "/console/tenants/team", "/console/tenants/team/members", url.Values{
		"username": {"bob"},
		"role":     {"writer"},
	})
	_, _ = io.Copy(io.Discard, addResp.Body)
	_ = addResp.Body.Close()
	if addResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("add member: expected 303, got %d", addResp.StatusCode)
	}
	mem, _ := f.Tenants.Memberships(context.Background(), bob.ID)
	if mem[team.ID] != tenants.RoleWriter {
		t.Errorf("expected RoleWriter for bob/team, got %v", mem[team.ID])
	}

	// Remove member.
	rmURL := "/console/tenants/team/members/" + strings.Trim(strings.Split("/"+itoa(bob.ID)+"/delete", "/")[1], "") + "/delete"
	_ = rmURL // build a real URL
	rmResp := f.PostFormFrom(t, "/console/tenants/team", "/console/tenants/team/members/"+itoa(bob.ID)+"/delete", url.Values{})
	_, _ = io.Copy(io.Discard, rmResp.Body)
	_ = rmResp.Body.Close()
	if rmResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("remove member: expected 303, got %d", rmResp.StatusCode)
	}
	mem2, _ := f.Tenants.Memberships(context.Background(), bob.ID)
	if _, still := mem2[team.ID]; still {
		t.Error("expected member to be removed")
	}
}

func itoa(n int64) string {
	// Avoid importing strconv just for this; tests stay self-contained.
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = digits[n%10]
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
