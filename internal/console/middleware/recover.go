package middleware

import (
	"log/slog"
	"runtime/debug"

	"github.com/gin-gonic/gin"
)

// Recover wraps gin.Recovery with structured logging via the console's
// slog logger. Logs the panic, the stack, and the request-id; renders a
// minimal 500 response so the client never sees gin's default text dump.
func Recover(logger *slog.Logger) gin.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(c *gin.Context) {
		defer func() {
			if rec := recover(); rec != nil {
				rid := RequestIDFrom(c)
				logger.Error("panic recovered in console",
					"panic", rec,
					"request_id", rid,
					"path", c.Request.URL.Path,
					"method", c.Request.Method,
					"stack", string(debug.Stack()),
				)
				if !c.Writer.Written() {
					c.AbortWithStatusJSON(500, map[string]any{
						"error":      "internal server error",
						"request_id": rid,
					})
				} else {
					c.Abort()
				}
			}
		}()
		c.Next()
	}
}
