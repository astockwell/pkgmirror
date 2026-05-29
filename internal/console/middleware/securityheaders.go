package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// SecurityHeaders adds baseline browser security headers to every console
// response. Non-negotiable for an admin UI; pairs deliberately with the
// no-inline-scripts decision in plans/web-console-implementation-plan.md
// (the CSP forbids inline scripts, which is exactly why per-page external
// JS via BaseData.ScriptURLs is the right pattern).
//
// usingTLS controls whether HSTS is emitted. Setting it without actually
// terminating TLS would lock users out of the http endpoint, so the
// caller must only set it when TLS is in front (either at the gin
// server itself or at a trusted upstream proxy).
func SecurityHeaders(usingTLS bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self'; "+
				"style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; "+
				"font-src 'self'; "+
				"frame-ancestors 'none'; "+
				"base-uri 'self'; "+
				"form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Permissions-Policy", "accelerometer=(), camera=(), geolocation=(), gyroscope=(), microphone=(), payment=(), usb=()")
		if usingTLS {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		// Don't cache authenticated console pages by default; individual
		// handlers may opt in to caching for static-y bits (the static
		// asset handler bypasses this middleware entirely).
		if c.Request.Method != http.MethodGet || c.Request.URL.Path != "/-/healthz" {
			h.Set("Cache-Control", "no-store, max-age=0")
		}
		c.Next()
	}
}
