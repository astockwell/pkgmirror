package console_test

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/astockwell/pkgmirror/internal/audit"
	"github.com/astockwell/pkgmirror/internal/console"
	consolemw "github.com/astockwell/pkgmirror/internal/console/middleware"
	pkgdb "github.com/astockwell/pkgmirror/internal/db"
	"github.com/astockwell/pkgmirror/internal/models"
	"github.com/astockwell/pkgmirror/internal/policy"
	"github.com/astockwell/pkgmirror/internal/tenants"
	"github.com/astockwell/pkgmirror/internal/tokens"
	"github.com/astockwell/pkgmirror/internal/users"

	"github.com/gin-gonic/gin"
)

// authFixture is a full integration harness for the PR 2b auth flow:
// real sqlite + users + tenants + tokens + audit + console, exposed via
// an httptest.Server with a cookie jar so login state survives across
// requests in a single test.
type authFixture struct {
	t       *testing.T
	DB      *sql.DB
	Users   *users.Store
	Tenants *tenants.Store
	Tokens  *tokens.Store
	Audit   audit.Logger
	Models  *models.Store
	Rules   *policy.RuleStore
	Console *console.Console
	Server  *httptest.Server
	Client  *http.Client
}

func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	dir := t.TempDir()
	db, err := pkgdb.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})

	userStore := users.New(db)
	tenantStore := tenants.New(db)
	tokenStore := tokens.New(db)
	modelStore := models.New(db)
	ruleStore := policy.NewRuleStore(db)
	auditLogger := audit.New(db, 256)
	t.Cleanup(func() { _ = auditLogger.Close() })

	cfg := fixtureConfig(t)
	authn := consolemw.NewSessionAuthenticator(userStore, tenantStore)
	c, err := console.New(console.Deps{
		Config:        cfg,
		Users:         userStore,
		Tenants:       tenantStore,
		Models:        modelStore,
		Tokens:        tokenStore,
		Audit:         auditLogger,
		Rules:         ruleStore,
		Authenticator: authn,
		AppVersion:    "test",
	})
	if err != nil {
		t.Fatalf("console.New: %v", err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	if err := c.Register(r); err != nil {
		t.Fatalf("register: %v", err)
	}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	jar, _ := cookiejar.New(nil)
	cli := &http.Client{
		Jar: jar,
		// Don't auto-follow redirects so tests can inspect 303s.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return &authFixture{
		t: t, DB: db,
		Users: userStore, Tenants: tenantStore, Tokens: tokenStore, Audit: auditLogger, Models: modelStore, Rules: ruleStore,
		Console: c, Server: srv, Client: cli,
	}
}

// CreateAdmin makes a system admin with the given username + password.
// Returns the user.
func (f *authFixture) CreateAdmin(t *testing.T, name, password string) *users.User {
	t.Helper()
	u, err := f.Users.Create(context.Background(), users.CreateOptions{
		Name: name, Kind: users.KindHuman, IsAdmin: true,
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	hash, err := users.HashPassword(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := f.Users.SetPasswordHash(context.Background(), u.ID, hash); err != nil {
		t.Fatalf("set password: %v", err)
	}
	return u
}

// CreateUserWithToken makes a regular user + mints a PAT. Returns user
// and the plaintext token (only visible at creation time).
func (f *authFixture) CreateUserWithToken(t *testing.T, name string) (*users.User, string) {
	t.Helper()
	u, err := f.Users.Create(context.Background(), users.CreateOptions{
		Name: name, Kind: users.KindHuman,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	plaintext, _, err := f.Tokens.Issue(context.Background(), tokens.CreateOptions{
		UserID: u.ID,
		Name:   "test-pat",
		Scopes: []tokens.Scope{tokens.ScopeRead, tokens.ScopeWrite},
	})
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	return u, plaintext
}

// Get the page, returning status + body.
func (f *authFixture) Get(t *testing.T, path string) (*http.Response, string) {
	t.Helper()
	resp, err := f.Client.Get(f.Server.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, string(body)
}

// csrfTokenRE pulls the CSRF token out of a rendered form. gorilla/csrf
// renders <input type="hidden" name="gorilla.csrf.Token" value="...">.
var csrfTokenRE = regexp.MustCompile(`name="gorilla\.csrf\.Token" value="([^"]+)"`)

// PostForm POSTs values to path after first GETing it (to scrape the CSRF
// token + populate the session cookie). Returns the POST response.
func (f *authFixture) PostForm(t *testing.T, path string, vals url.Values) *http.Response {
	return f.PostFormFrom(t, path, path, vals)
}

// PostFormFrom is like PostForm but primes the CSRF cookie via a GET
// to a different path. Used for POST-only routes like /console/logout
// where a GET would 404.
func (f *authFixture) PostFormFrom(t *testing.T, primerPath, postPath string, vals url.Values) *http.Response {
	t.Helper()
	// Prime the session + CSRF token.
	pre, body := f.Get(t, primerPath)
	if pre.StatusCode != http.StatusOK {
		t.Fatalf("priming GET %s = %d", primerPath, pre.StatusCode)
	}
	m := csrfTokenRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no CSRF token in body of %s", primerPath)
	}
	vals.Set("gorilla.csrf.Token", m[1])

	req, _ := http.NewRequest(http.MethodPost, f.Server.URL+postPath, strings.NewReader(vals.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// gorilla/csrf requires a matching Referer for plain-HTTP requests
	// (the test server is HTTP). Set it so the middleware accepts.
	req.Header.Set("Referer", f.Server.URL+postPath)
	resp, err := f.Client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", postPath, err)
	}
	return resp
}

// PostFormNoCSRF skips the CSRF priming step. Used to verify the CSRF
// middleware actually rejects requests without a token.
func (f *authFixture) PostFormNoCSRF(t *testing.T, path string, vals url.Values) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, f.Server.URL+path, strings.NewReader(vals.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := f.Client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

// ---- Tests ----

func TestLoginPage_Renders200(t *testing.T) {
	f := newAuthFixture(t)
	resp, body := f.Get(t, "/console/login")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `name="username"`) {
		t.Errorf("expected username field; got %s", body)
	}
	if !strings.Contains(body, `name="password"`) {
		t.Errorf("expected password field; got %s", body)
	}
	if !strings.Contains(body, `name="gorilla.csrf.Token"`) {
		t.Errorf("expected CSRF token field; got %s", body)
	}
}

func TestLogin_RejectsBadCredentialsGenerically(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")

	// Wrong password — must NOT distinguish from "user not exist".
	resp := f.PostForm(t, "/console/login", url.Values{
		"username": {"alice"},
		"password": {"wrong"},
	})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 re-render, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "Sign-in failed") {
		t.Errorf("expected generic failure message; got %s", body)
	}

	// Unknown user — same generic message.
	resp2 := f.PostForm(t, "/console/login", url.Values{
		"username": {"ghost"},
		"password": {"anything"},
	})
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	if !strings.Contains(string(body2), "Sign-in failed") {
		t.Errorf("expected same generic message for unknown user; got %s", body2)
	}
}

func TestLogin_SuccessSetsCookieAndRedirects(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")

	resp := f.PostForm(t, "/console/login", url.Values{
		"username": {"alice"},
		"password": {"correct-password-12chars"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 303, got %d; body=%s", resp.StatusCode, body)
	}
	if loc := resp.Header.Get("Location"); loc != "/console/" {
		t.Errorf("expected redirect to /console/, got %q", loc)
	}
}

func TestLogin_RateLimitFiresAfter10(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")

	// 10 wrong passwords keep returning 200 re-renders.
	for i := 0; i < 10; i++ {
		resp := f.PostForm(t, "/console/login", url.Values{
			"username": {"alice"},
			"password": {"wrong"},
		})
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("attempt %d unexpected status %d", i+1, resp.StatusCode)
		}
	}
	// 11th should 429.
	resp := f.PostForm(t, "/console/login", url.Values{
		"username": {"alice"},
		"password": {"wrong"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected 429 on 11th attempt, got %d", resp.StatusCode)
	}
}

func TestLogin_RateLimitClearsAfterSuccess(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")

	// Burn 9 attempts.
	for i := 0; i < 9; i++ {
		resp := f.PostForm(t, "/console/login", url.Values{
			"username": {"alice"}, "password": {"wrong"},
		})
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	// 10th = real password = success + bucket clear.
	ok := f.PostForm(t, "/console/login", url.Values{
		"username": {"alice"}, "password": {"correct-password-12chars"},
	})
	_, _ = io.Copy(io.Discard, ok.Body)
	_ = ok.Body.Close()
	if ok.StatusCode != http.StatusSeeOther {
		t.Fatalf("success expected 303, got %d", ok.StatusCode)
	}

	// Fresh attempts in a new session should NOT be rate-limited.
	f.Client.Jar, _ = cookiejar.New(nil)
	resp := f.PostForm(t, "/console/login", url.Values{
		"username": {"alice"}, "password": {"wrong"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("after successful login + new jar, expected fresh allow; got %d", resp.StatusCode)
	}
}

func TestLogout_ClearsSession(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")

	// Sign in.
	resp := f.PostForm(t, "/console/login", url.Values{
		"username": {"alice"}, "password": {"correct-password-12chars"},
	})
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	// Logout. After signing in, /console/login redirects (signed-in
	// users get bounced to /console/), so prime from /console/set-
	// password which always renders a form regardless of auth state.
	logout := f.PostFormFrom(t, "/console/set-password", "/console/logout", url.Values{})
	defer logout.Body.Close()
	if logout.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout expected 303, got %d", logout.StatusCode)
	}

	// Subsequent authed request (the ping route is anonymous in PR 2b
	// since no page handlers landed yet, but /console/set-password
	// should at least not act on the cleared cookie). Just check the
	// login page renders without the "you're already signed in" path.
	page, body := f.Get(t, "/console/login")
	if page.StatusCode != http.StatusOK {
		t.Fatalf("login page after logout = %d", page.StatusCode)
	}
	if !strings.Contains(body, `name="password"`) {
		t.Error("expected fresh login form after logout")
	}
}

func TestSetPassword_HappyPath(t *testing.T) {
	f := newAuthFixture(t)
	u, plaintext := f.CreateUserWithToken(t, "bob")

	resp := f.PostForm(t, "/console/set-password", url.Values{
		"username":         {"bob"},
		"token":            {plaintext},
		"new_password":     {"new-strong-password-12"},
		"confirm_password": {"new-strong-password-12"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 303 redirect, got %d; body=%s", resp.StatusCode, body)
	}

	// Verify password was actually written.
	verified, err := f.Users.VerifyPassword(context.Background(), "bob", "new-strong-password-12")
	if err != nil {
		t.Fatalf("VerifyPassword after set: %v", err)
	}
	if verified.ID != u.ID {
		t.Errorf("got user id %d, want %d", verified.ID, u.ID)
	}
}

func TestSetPassword_RejectsBadToken(t *testing.T) {
	f := newAuthFixture(t)
	_, _ = f.CreateUserWithToken(t, "bob")

	resp := f.PostForm(t, "/console/set-password", url.Values{
		"username":         {"bob"},
		"token":            {"pkm_obviously_invalid"},
		"new_password":     {"new-strong-password-12"},
		"confirm_password": {"new-strong-password-12"},
	})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 re-render, got %d", resp.StatusCode)
	}
	// html/template escapes the apostrophe in "Couldn't" so check the
	// escaped form.
	if !strings.Contains(string(body), "Couldn&#39;t set password") {
		t.Errorf("expected generic failure message; got %s", body)
	}
}

func TestSetPassword_RejectsTokenForDifferentUser(t *testing.T) {
	f := newAuthFixture(t)
	_, _ = f.CreateUserWithToken(t, "bob")
	_, dianeToken := f.CreateUserWithToken(t, "diane")

	// Try to set bob's password with diane's token. Must fail.
	resp := f.PostForm(t, "/console/set-password", url.Values{
		"username":         {"bob"},
		"token":            {dianeToken},
		"new_password":     {"new-strong-password-12"},
		"confirm_password": {"new-strong-password-12"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected re-render, got %d", resp.StatusCode)
	}

	// Bob still has no password.
	_, err := f.Users.VerifyPassword(context.Background(), "bob", "new-strong-password-12")
	if err == nil {
		t.Error("expected no password set for bob (his account wasn't authorized)")
	}
}

func TestSetPassword_RejectsMismatchedConfirm(t *testing.T) {
	f := newAuthFixture(t)
	_, plaintext := f.CreateUserWithToken(t, "bob")

	resp := f.PostForm(t, "/console/set-password", url.Values{
		"username":         {"bob"},
		"token":            {plaintext},
		"new_password":     {"first-12chars-here"},
		"confirm_password": {"different-12chars-here"},
	})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected re-render, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "don&#39;t match") {
		t.Errorf("expected mismatch message; got %s", body)
	}
}

func TestSetPassword_RejectsShortPassword(t *testing.T) {
	f := newAuthFixture(t)
	_, plaintext := f.CreateUserWithToken(t, "bob")

	resp := f.PostForm(t, "/console/set-password", url.Values{
		"username":         {"bob"},
		"token":            {plaintext},
		"new_password":     {"short"},
		"confirm_password": {"short"},
	})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "12 characters") {
		t.Errorf("expected length validation; got %s", body)
	}
}

func TestCSRF_PostWithoutTokenIs403(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")

	resp := f.PostFormNoCSRF(t, "/console/login", url.Values{
		"username": {"alice"},
		"password": {"correct-password-12chars"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}

// TestNoLoginPageInProxyMode confirms password-mode-only routes return
// 403 (with a friendly page) when the console is in proxy-header mode.
func TestNoLoginPageInProxyMode(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.AuthMode = "proxy-header"
	cfg.TrustedProxies = []string{"127.0.0.1"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	dir := t.TempDir()
	db, err := pkgdb.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	userStore := users.New(db)
	tenantStore := tenants.New(db)

	c, err := console.New(console.Deps{
		Config:        cfg,
		Users:         userStore,
		Tenants:       tenantStore,
		Authenticator: consolemw.NewSessionAuthenticator(userStore, tenantStore),
		AppVersion:    "test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	if err := c.Register(r); err != nil {
		t.Fatalf("register: %v", err)
	}
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/console/login")
	if err != nil {
		t.Fatalf("GET login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 in proxy-header mode, got %d", resp.StatusCode)
	}
}
