package console_test

// Middleware-chain tests. Unlike the page-focused tests in the other
// _test.go files in this package, these target the COMPOSED chain:
// invariants that must hold on EVERY /console response regardless of
// route, plus regression tests for the specific cross-middleware bugs
// we've already fixed (PR 2b had two of them).
//
// All tests use the standard authFixture (real httptest.Server, real
// gin.Engine, real cookie jar) so we are testing the middleware chain
// production actually uses, not a mock.

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// ---- A. Baseline headers + request id (chain invariants) ----

// TestChain_SecurityHeadersOnEveryRoute asserts the SecurityHeaders
// middleware fires for every /console response that goes through the
// group's chain, regardless of route, status, or anonymous-vs-authed.
//
// NOT covered (deliberately): /console paths that hit gin's engine-
// level NoRoute handler, because that handler runs OUTSIDE the group
// chain (gin limitation; documented gap; would need a wildcard catch-
// all inside the group to fix). Tracked as a follow-up.
func TestChain_SecurityHeadersOnEveryRoute(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")

	cases := []struct {
		name       string
		path       string
		method     string
		setup      func()
		wantStatus int
	}{
		{
			name: "anonymous GET _ping",
			path: "/console/_ping", method: "GET",
			wantStatus: http.StatusOK,
		},
		{
			name: "anonymous GET login",
			path: "/console/login", method: "GET",
			wantStatus: http.StatusOK,
		},
		{
			name: "anonymous GET dashboard redirects to login",
			path: "/console/", method: "GET",
			wantStatus: http.StatusSeeOther,
		},
		{
			name: "anonymous GET tenants redirects to login",
			path: "/console/tenants", method: "GET",
			wantStatus: http.StatusSeeOther,
		},
		{
			name: "POST without CSRF returns 403 (CSRF middleware)",
			path: "/console/login", method: "POST_NO_CSRF",
			wantStatus: http.StatusForbidden,
		},
		{
			name: "authed admin GET dashboard renders 200",
			path: "/console/", method: "GET",
			setup: func() {
				f.loginAs(t, "alice", "correct-password-12chars")
			},
			wantStatus: http.StatusOK,
		},
	}

	// Required headers from middleware/securityheaders.go. CSP value
	// is checked for two distinctive directives rather than full equality
	// so adding a directive in the future doesn't break the test.
	type headerCheck struct {
		name        string
		mustContain string
	}
	wantHeaders := []headerCheck{
		{"Content-Security-Policy", "default-src 'self'"},
		{"Content-Security-Policy", "frame-ancestors 'none'"},
		{"X-Content-Type-Options", "nosniff"},
		{"X-Frame-Options", "DENY"},
		{"Referrer-Policy", "strict-origin-when-cross-origin"},
		{"Cache-Control", "no-store"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setup != nil {
				tc.setup()
			}
			var resp *http.Response
			switch tc.method {
			case "GET":
				r, _ := f.Get(t, tc.path)
				resp = r
			case "POST_NO_CSRF":
				resp = f.PostFormNoCSRF(t, tc.path, url.Values{"username": {"x"}})
			default:
				t.Fatalf("unknown method %q", tc.method)
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status=%d want=%d", resp.StatusCode, tc.wantStatus)
			}
			for _, h := range wantHeaders {
				got := resp.Header.Get(h.name)
				if !strings.Contains(got, h.mustContain) {
					t.Errorf("header %q: want substring %q, got %q",
						h.name, h.mustContain, got)
				}
			}
			// X-Request-Id is a separate invariant; folded in here so we
			// don't double the test count.
			if rid := resp.Header.Get("X-Request-Id"); rid == "" {
				t.Errorf("X-Request-Id missing from response")
			}
		})
	}
}

// ---- B. Chain integrity (regression tests for shipped bug fixes) ----

// TestChain_CSRFRejectAbortsHandler is the regression test for the bug
// fixed in PR 2b commit 7ca6b12: when gorilla/csrf's ErrorHandler ran,
// gin continued the chain anyway because nothing called c.Abort(). The
// downstream handler ran AND wrote a body, producing two response
// pages concatenated. The fix in middleware/csrf.go tracks whether
// the wrapped inner handler ran and calls c.Abort() if not.
//
// This test proves the fix by hitting a state-changing route with no
// CSRF token and asserting that no state change occurred. If the
// handler ever runs again behind a rejected CSRF, this test catches it
// immediately.
func TestChain_CSRFRejectAbortsHandler(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	f.loginAs(t, "alice", "correct-password-12chars")

	before, err := f.Tenants.Count(context.Background())
	if err != nil {
		t.Fatalf("baseline count: %v", err)
	}

	resp := f.PostFormNoCSRF(t, "/console/tenants", url.Values{
		"name":       {"should-not-exist"},
		"visibility": {"private"},
	})
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %d", resp.StatusCode)
	}

	after, err := f.Tenants.Count(context.Background())
	if err != nil {
		t.Fatalf("post count: %v", err)
	}
	if after != before {
		t.Errorf("tenant count changed: before=%d after=%d (CSRF reject did not abort the chain)",
			before, after)
	}

	// Belt-and-braces: the named tenant must not exist.
	if _, err := f.Tenants.GetByName(context.Background(), "should-not-exist"); err == nil {
		t.Error("tenant 'should-not-exist' was created despite CSRF rejection")
	}
}

// TestChain_PanicInHandlerProducesFriendly500 verifies the Recover
// middleware catches handler panics, returns 500, and keeps the
// request-id observable to the client. Without this layer a panic
// would crash the request goroutine and the user would see EOF
// instead of a usable error response.
//
// Uses a deliberately-panicking handler registered at
// /console/_test_panic via the authed group, so it inherits the full
// chain (Recover at the top, then everything else).
func TestChain_PanicInHandlerProducesFriendly500(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	registerTestRoutes(f)
	f.loginAs(t, "alice", "correct-password-12chars")

	resp, body := f.Get(t, "/console/_test_panic")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 from recovered panic, got %d", resp.StatusCode)
	}
	// Recover middleware writes a JSON body via AbortWithStatusJSON.
	// The exact shape isn't an API contract, but two things must hold:
	// it must be non-empty (chain didn't crash silently) and the
	// request-id must be observable (operator can grep logs).
	if len(body) == 0 {
		t.Error("recovered panic produced an empty body")
	}
	if !strings.Contains(body, "internal server error") {
		t.Errorf("expected 'internal server error' in body, got %q", body)
	}
	if rid := resp.Header.Get("X-Request-Id"); rid == "" {
		t.Error("X-Request-Id missing from 500 response (recover should not strip it)")
	} else if !strings.Contains(body, rid) {
		// The recover middleware echoes the request-id into the JSON
		// body so the user can include it in a bug report.
		t.Errorf("X-Request-Id %q not echoed in response body %q", rid, body)
	}
}

// TestChain_RequestIDFlowsToErrorPage verifies the request-id is
// reachable from RenderError so a user reporting a problem can name the
// request-id from the page, and an operator can grep logs for the same
// id. The chain wires this via RequestID -> baseData (reads from gin
// context) -> the error template (which prints .Body.RequestID).
//
// Registers a temporary route at /console/_test_render_error that
// invokes RenderError with a known verb, then scrapes the rendered
// HTML for the request-id and asserts it matches X-Request-Id.
func TestChain_RequestIDFlowsToErrorPage(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	registerRenderErrorRoute(t, f)
	f.loginAs(t, "alice", "correct-password-12chars")

	resp, body := f.Get(t, "/console/_test_render_error")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", resp.StatusCode)
	}
	headerRID := resp.Header.Get("X-Request-Id")
	if headerRID == "" {
		t.Fatal("X-Request-Id missing from response header")
	}

	// The error template renders: <code>{rid}</code> after "Request ID:".
	m := requestIDInPageRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("could not find request-id in page body; body=%s", body)
	}
	pageRID := m[1]
	if pageRID != headerRID {
		t.Errorf("request-id mismatch: header=%q page=%q", headerRID, pageRID)
	}
}

var requestIDInPageRE = regexp.MustCompile(`Request ID:\s*<code>([a-f0-9]+)</code>`)

// ---- C. Auth gate composition ----

// TestChain_RequireAuthRedirectStashesReturnTo verifies the
// anonymous-to-protected-route round-trip:
//
//  1. Anonymous GET /console/tenants -> 303 to /console/login (RequireAuth)
//  2. Auth middleware stashes /console/tenants as return_to in the session
//  3. POST /console/login with credentials succeeds (loginSubmit)
//  4. loginSubmit pops return_to and redirects to /console/tenants
//
// This proves the chain ordering: Session must run before Auth
// (RequireAuth needs the session to stash), and CSRF must not break
// the session cookie that carries return_to between requests.
func TestChain_RequireAuthRedirectStashesReturnTo(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")

	// Step 1+2: anonymous GET stashes return_to and redirects.
	r1, _ := f.Get(t, "/console/tenants")
	if r1.StatusCode != http.StatusSeeOther {
		t.Fatalf("anon GET expected 303, got %d", r1.StatusCode)
	}
	if loc := r1.Header.Get("Location"); loc != "/console/login" {
		t.Errorf("expected redirect to /console/login, got %q", loc)
	}

	// Step 3: login. PostForm primes via GET /console/login which does
	// NOT touch return_to (loginPage is read-only on session).
	r2 := f.PostForm(t, "/console/login", url.Values{
		"username": {"alice"},
		"password": {"correct-password-12chars"},
	})
	defer r2.Body.Close()
	_, _ = io.Copy(io.Discard, r2.Body)
	if r2.StatusCode != http.StatusSeeOther {
		t.Fatalf("login expected 303, got %d", r2.StatusCode)
	}

	// Step 4: redirect target is the originally-requested path.
	if loc := r2.Header.Get("Location"); loc != "/console/tenants" {
		t.Errorf("expected redirect back to /console/tenants, got %q", loc)
	}
}

// ---- D. Rate limit composition ----

// TestChain_RateLimitDoesNotConsumeOnCSRFReject documents the
// architectural choice that CSRF runs BEFORE the login rate limiter in
// the chain. The practical consequence: an attacker who can't supply a
// CSRF token can never trip the rate limit because their requests are
// short-circuited by CSRF first. The legitimate-user rate limit
// (10/min/IP) only counts CSRF-valid attempts.
//
// Test: fire 15 POSTs to /console/login with NO CSRF token. Each must
// return 403 (CSRF). The 11th must NOT return 429 (the rate-limit
// counter must not have ticked at all). After the burst, a fully-valid
// login must still succeed (rate-limit bucket still empty).
func TestChain_RateLimitDoesNotConsumeOnCSRFReject(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")

	// 15 attempts well past the 10/min default limit.
	for i := 0; i < 15; i++ {
		resp := f.PostFormNoCSRF(t, "/console/login", url.Values{
			"username": {"alice"}, "password": {"correct-password-12chars"},
		})
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("attempt %d: expected 403 (CSRF), got %d", i+1, resp.StatusCode)
		}
	}

	// Now do a legitimate login. If the rate limit had been consumed
	// by the CSRF-rejected attempts above, this would 429.
	resp := f.PostForm(t, "/console/login", url.Values{
		"username": {"alice"},
		"password": {"correct-password-12chars"},
	})
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("legitimate login after CSRF-rejected burst: expected 303, got %d (rate limit consumed by CSRF rejects?)",
			resp.StatusCode)
	}
}

// ---- helpers ----

// registerRenderErrorRoute is a thin alias kept for readability at
// the call site of TestChain_RequestIDFlowsToErrorPage. The real
// route registration lives in middleware_chain_helpers_test.go
// (separate file because it imports gin and we want this file to
// stay focused on assertions).
func registerRenderErrorRoute(t *testing.T, f *authFixture) {
	t.Helper()
	registerTestRoutes(f)
}
