package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/gin-gonic/gin"
)

// ginRequestIDKey is where RequestID stashes the generated id.
const ginRequestIDKey = "pkgmirror.console.request_id"

// RequestID generates a request-id for every console request and stashes
// it in the gin context for downstream middleware + handlers to log.
//
// The id is short (16 hex chars / 64 bits) — collisions across a log
// search window are vanishingly unlikely for a console with tens of
// operators. If the caller supplied an X-Request-ID header, honor it
// (lets upstream tracing systems propagate ids).
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-ID")
		if id == "" {
			id = newRequestID()
		}
		c.Set(ginRequestIDKey, id)
		c.Header("X-Request-ID", id)
		c.Next()
	}
}

// RequestIDFrom returns the request-id stashed by RequestID, or "" if
// RequestID wasn't in the chain.
func RequestIDFrom(c *gin.Context) string {
	v, ok := c.Get(ginRequestIDKey)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand should never fail; if it does, returning a
		// fixed sentinel is better than panicking on every request.
		return fmt.Sprintf("rid-err-%x", b)
	}
	return hex.EncodeToString(b[:])
}
