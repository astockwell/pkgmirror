package middleware

import (
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
		// gorilla/csrf reads + augments the request; capture the
		// (possibly-augmented) request back into the gin context so
		// downstream handlers see the same one. csrf adds a context
		// value containing the token nonce that csrf.TemplateField
		// reads via the http.Request, not the response.
		inner(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c.Request = r
			c.Next()
		})).ServeHTTP(c.Writer, c.Request)
	}
}

// csrfErrorPage renders a minimal failure page. PR 4 may upgrade this
// to use Console.Render once a 403 layout is in place; for now the
// plain text response is fine since CSRF failures should be rare and
// users can always navigate back to retry.
func csrfErrorPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`<!doctype html><html><head><meta charset="utf-8">` +
		`<title>Form expired</title></head><body>` +
		`<h1>Form expired</h1>` +
		`<p>The form you submitted has expired or been tampered with. ` +
		`Please go back and try again.</p>` +
		`</body></html>`))
}
