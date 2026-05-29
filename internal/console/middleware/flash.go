package middleware

import (
	"log/slog"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

// FlashMessage is the typed payload of a flash. Severity drives the
// rendered color; Text is rendered as plain text (not HTML) by
// partials/flash.tmpl. The "no HTML in flash" rule keeps the threat
// surface tiny — if a flash ever needs a link, add LinkURL/LinkText
// fields rather than allow HTML.
type FlashMessage struct {
	Severity string
	Text     string
}

// Severity constants. Handlers should call FlashSuccess/FlashError/etc.
// rather than typing these strings directly.
const (
	FlashInfo    = "info"
	FlashSuccess = "success"
	FlashWarning = "warning"
	FlashDanger  = "danger"
)

// AddFlash queues a message for the next page render. Called from
// handlers; consumed by Console.baseData on the following request.
//
// A failed session save is logged but not surfaced to the caller —
// flash failure should never block the actual state change a handler
// just succeeded at.
func AddFlash(c *gin.Context, severity, text string) {
	sess := sessions.Default(c)
	sess.AddFlash(FlashMessage{Severity: severity, Text: text})
	if err := sess.Save(); err != nil {
		slog.Default().Warn("flash save failed", "err", err, "path", c.Request.URL.Path)
	}
}

// ReadFlashes returns and clears all queued flashes. Called once per
// request from Console.baseData.
func ReadFlashes(c *gin.Context) []FlashMessage {
	sess := sessions.Default(c)
	raw := sess.Flashes()
	if len(raw) == 0 {
		return nil
	}
	out := make([]FlashMessage, 0, len(raw))
	for _, f := range raw {
		if m, ok := f.(FlashMessage); ok {
			out = append(out, m)
		}
	}
	// Save to commit the flash-consumption to the session cookie.
	if err := sess.Save(); err != nil {
		slog.Default().Warn("flash consume save failed", "err", err)
	}
	return out
}

// FlashSavedFlash is a convenience wrapper for "Foo saved."
func FlashSavedFlash(c *gin.Context, what string) {
	AddFlash(c, FlashSuccess, what+" saved.")
}

// FlashDeletedFlash is a convenience wrapper for "Foo deleted."
func FlashDeletedFlash(c *gin.Context, what string) {
	AddFlash(c, FlashSuccess, what+" deleted.")
}

// FlashErrorFlash is a convenience wrapper for "Couldn't <verb>: <err>".
func FlashErrorFlash(c *gin.Context, verb string, err error) {
	AddFlash(c, FlashDanger, "Couldn't "+verb+": "+err.Error())
}
