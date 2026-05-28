package server

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/astockwell/pkgmirror/internal/auth"
	"github.com/astockwell/pkgmirror/internal/policy"

	"github.com/gin-gonic/gin"
)

// policyActorMiddleware bridges authenticated identity (set on the gin
// context by auth.Middleware) and per-request metadata into a
// policy.Actor on the request context. The downstream audit-wrapping
// policy engine reads this actor when recording audit events.
//
// A request_id is generated if the X-Request-Id header is absent so
// every audit row has a correlation key.
func policyActorMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		actor := policy.Actor{
			RequestID:  c.GetHeader("X-Request-Id"),
			RemoteAddr: c.ClientIP(),
			UserAgent:  c.GetHeader("User-Agent"),
		}
		if actor.RequestID == "" {
			actor.RequestID = newRequestID()
		}
		if id := auth.FromContext(c); id != nil {
			if id.User != nil {
				actor.UserID = id.User.ID
			}
			if id.Token != nil {
				actor.TokenID = id.Token.ID
			}
		}
		c.Request = c.Request.WithContext(policy.WithActor(c.Request.Context(), actor))
		// Expose the id for outgoing logs / response headers.
		c.Writer.Header().Set("X-Request-Id", actor.RequestID)
		c.Next()
	}
}

// newRequestID returns 16 random hex bytes. We don't need the full UUID
// machinery, just a unique-enough correlation token.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read essentially can't fail; fall back to a constant so
		// the request still has *some* id.
		return "0000000000000000"
	}
	return hex.EncodeToString(b[:])
}
