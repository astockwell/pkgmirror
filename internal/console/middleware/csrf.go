package middleware

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/csrf"
)

// CSRF wraps gorilla/csrf as a gin middleware. The token is exposed to
// templates via BaseData.CSRFField (populated in Console.baseData from
// csrf.TemplateField(r)); the form template renders it with
// {{ .Base.CSRFField }}.
//
// Cookie is path-scoped to /console so it doesn't bleed into the
// registry's surfaces (and vice versa). SameSite=Lax is the right
// default for an admin UI — strict would break flows that bounce
// through an OIDC IdP later, lax is enough to defend against
// cross-origin POSTs.
//
// gorilla/csrf v1.7+ defaults to assuming the request is HTTPS and
// rejects plain-HTTP Referer headers with ErrBadReferer. When the
// console is NOT behind TLS (laptop dev, test server, plain-HTTP
// proxy fronting), we flip the request context's
// csrf.PlaintextHTTPContextKey so gorilla evaluates Origin/Referer
// rules against http: schemes correctly.
func CSRF(secretKey []byte, secure bool) gin.HandlerFunc {
	if len(secretKey) == 0 {
		// Shouldn't reach here — Config.EnsureKeys gates this.
		// Returning a no-op middleware would silently disable CSRF;
		// instead, fail every request loudly so the misconfig is
		// obvious in test/dev.
		return func(c *gin.Context) {
			http.Error(c.Writer, "console misconfigured: CSRF key is empty", http.StatusInternalServerError)
			c.Abort()
		}
	}
	inner := csrf.Protect(secretKey,
		csrf.Secure(secure),
		csrf.HttpOnly(true),
		csrf.SameSite(csrf.SameSiteLaxMode),
		csrf.Path("/console"),
		csrf.ErrorHandler(http.HandlerFunc(csrfErrorPage)),
	)
	return func(c *gin.Context) {
		// Tell gorilla/csrf this is a plaintext-HTTP request when we
		// know TLS isn't in front. Without this, the Referer check
		// requires an https:// referer and rejects laptop-dev traffic.
		if !secure {
			c.Request = c.Request.WithContext(context.WithValue(
				c.Request.Context(), csrf.PlaintextHTTPContextKey, true))
		}
		// gorilla/csrf is an http.Handler-shaped middleware: it either
		// calls the inner handler (request OK) or its ErrorHandler
		// (request rejected). We track whether the inner ran; if it
		// didn't, gorilla rejected and we must abort the gin chain so
		// no downstream handler also writes to the response.
		var innerRan bool
		inner(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			innerRan = true
			c.Request = r
			c.Next()
		})).ServeHTTP(c.Writer, c.Request)
		if !innerRan {
			c.Abort()
		}
	}
}

// csrfErrorPage renders a minimal failure page. PR 4 may upgrade this
// to use Console.Render once a 403 layout is in place; for now the
// plain text response is fine since CSRF failures should be rare and
// users can always navigate back to retry.
func csrfErrorPage(w http.ResponseWriter, r *http.Request) {
	// Surface the underlying reason via slog so operators can debug
	// real misconfiguration (mismatched cookie domain, stale token,
	// Origin/Referer policy, etc).
	reason := csrf.FailureReason(r)
	slog.Default().Warn("csrf rejected",
		"path", r.URL.Path,
		"reason", reason,
		"origin", r.Header.Get("Origin"),
		"referer", r.Header.Get("Referer"))

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`<!doctype html><html><head><meta charset="utf-8">` +
		`<title>Form expired</title></head><body>` +
		`<h1>Form expired</h1>` +
		`<p>The form you submitted has expired or been tampered with. ` +
		`Please go back and try again.</p>` +
		`</body></html>`))
}
