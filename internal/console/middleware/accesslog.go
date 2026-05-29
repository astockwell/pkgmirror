package middleware

import (
	"log/slog"
	"time"

	"github.com/astockwell/pkgmirror/internal/auth"

	"github.com/gin-gonic/gin"
)

// AccessLog emits one Info-level slog line per request. Fields match
// the convention in plans/web-console-implementation-plan.md §3.2 so
// log search has stable key names across packages.
//
// Sensitive headers (Authorization, Cookie, the configured proxy auth
// headers) are NOT logged. Bodies are NOT logged.
func AccessLog(logger *slog.Logger) gin.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		latency := time.Since(start)

		var actorID int64
		var actorName string
		if id := auth.FromContext(c); id != nil && id.User != nil {
			actorID = id.User.ID
			actorName = id.User.Name
		}

		logger.Info("console request",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"latency_ms", latency.Milliseconds(),
			"bytes", c.Writer.Size(),
			"actor_user_id", actorID,
			"actor_username", actorName,
			"request_id", RequestIDFrom(c),
		)
	}
}
