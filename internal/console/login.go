package console

import (
	"errors"
	"net/http"
	"strings"

	"github.com/astockwell/pkgmirror/internal/audit"
	"github.com/astockwell/pkgmirror/internal/auth"
	"github.com/astockwell/pkgmirror/internal/console/middleware"
	"github.com/astockwell/pkgmirror/internal/tokens"
	"github.com/astockwell/pkgmirror/internal/users"

	"github.com/gin-gonic/gin"
)

// loginFormData renders pages/login. Errors map is populated on a
// validation-failure re-render so the template can show inline messages.
type loginFormData struct {
	Username string
	Errors   map[string]string
}

// setPasswordFormData renders pages/setpassword. Carries the verified
// username through from the GET that validated the user's PAT.
type setPasswordFormData struct {
	Username string
	Token    string // round-tripped through the form so the POST can re-verify
	Errors   map[string]string
}

// loginPage renders the login form. If the caller already has a valid
// session, bounce them to the post-login destination instead of showing
// the form (no double-login UX).
func (c *Console) loginPage(gc *gin.Context) {
	if c.cfg.AuthMode == "proxy-header" {
		c.RenderForbidden(gc, "Login form is not available in proxy-header mode. Configure your upstream auth proxy and reload.")
		return
	}
	if id := auth.FromContext(gc); id != nil && id.User != nil {
		gc.Redirect(http.StatusSeeOther, "/console/")
		return
	}
	c.RenderWithLayout(gc, "layouts/auth", "pages/login", loginFormData{Errors: map[string]string{}})
}

// loginSubmit consumes the POST. Two flow branches:
//
//  1. Password is set on the user -> standard password auth. Session
//     issued on success; re-render with inline error on failure.
//  2. User exists but has no password (PR 7 set-password flow first
//     time) -> bounce to /console/set-password preserving the username
//     so the user only types it once.
//
// All failures map to the same "Sign-in failed." inline error so the
// form never leaks whether the user exists, whether they have a
// password, or which field was wrong.
func (c *Console) loginSubmit(gc *gin.Context) {
	if c.cfg.AuthMode == "proxy-header" {
		c.RenderForbidden(gc, "Login is not available in proxy-header mode.")
		return
	}
	username := strings.TrimSpace(gc.PostForm("username"))
	password := gc.PostForm("password")
	ctx := gc.Request.Context()

	data := loginFormData{Username: username, Errors: map[string]string{}}
	failGeneric := func() {
		data.Errors["form"] = "Sign-in failed. Check your username and password."
		c.RenderWithLayout(gc, "layouts/auth", "pages/login", data)
	}

	if username == "" || password == "" {
		failGeneric()
		return
	}

	u, err := c.users.VerifyPassword(ctx, username, password)
	switch {
	case err == nil:
		// success path below.
	case errors.Is(err, users.ErrNoPassword):
		// First-time login: bounce to set-password flow. Preserve
		// username so the user doesn't retype.
		c.auditLogin(gc, nil, "console.login.no_password", "user has no password set")
		gc.Redirect(http.StatusSeeOther, "/console/set-password?username="+username)
		return
	case errors.Is(err, users.ErrPasswordMismatch), errors.Is(err, users.ErrNotExist):
		c.auditLogin(gc, nil, "console.login.deny", "bad credentials")
		failGeneric()
		return
	default:
		c.logger.Error("login: verify password", "err", err, "username", username, "request_id", middleware.RequestIDFrom(gc))
		c.RenderError(gc, "sign in", err)
		return
	}

	if err := middleware.LoginSession(gc, u.ID); err != nil {
		c.RenderError(gc, "save session", err)
		return
	}
	c.loginLimit.Forget(middleware.ClientIPFor(gc.Request, c.cfg.TrustedProxies))
	c.auditLogin(gc, u, "console.login", "")
	middleware.AddFlash(gc, middleware.FlashSuccess, "Signed in.")
	target := middleware.PopReturnTo(gc)
	if target == "" || !strings.HasPrefix(target, "/console") {
		target = "/console/"
	}
	gc.Redirect(http.StatusSeeOther, target)
}

// logout clears the session and bounces to the login page. POST-only
// (a GET-driven logout would let a malicious image tag log you out).
func (c *Console) logout(gc *gin.Context) {
	var u *users.User
	if id := auth.FromContext(gc); id != nil {
		u = id.User
	}
	if err := middleware.LogoutSession(gc); err != nil {
		c.logger.Warn("logout: clear session", "err", err)
	}
	c.auditLogin(gc, u, "console.logout", "")
	middleware.AddFlash(gc, middleware.FlashInfo, "Signed out.")
	if c.cfg.AuthMode == "proxy-header" {
		// Proxy-header mode: there's no /console/login page; show a
		// friendly stop page.
		gc.Redirect(http.StatusSeeOther, "/console/_ping")
		return
	}
	gc.Redirect(http.StatusSeeOther, "/console/login")
}

// setPasswordPage renders the set-password form. Requires a verified
// username (from the query string set on the loginSubmit redirect) +
// the user's PAT for the POST to verify. We don't pre-verify the PAT
// here -- the form just collects it and posts it.
func (c *Console) setPasswordPage(gc *gin.Context) {
	if c.cfg.AuthMode == "proxy-header" {
		c.RenderForbidden(gc, "Set-password is not available in proxy-header mode.")
		return
	}
	username := strings.TrimSpace(gc.Query("username"))
	c.RenderWithLayout(gc, "layouts/auth", "pages/setpassword", setPasswordFormData{
		Username: username,
		Errors:   map[string]string{},
	})
}

// setPasswordSubmit verifies the user + their PAT, then writes the new
// password hash + issues a session. On any failure renders the same
// generic message — never leaks whether the user exists, whether the
// token matched, etc.
func (c *Console) setPasswordSubmit(gc *gin.Context) {
	if c.cfg.AuthMode == "proxy-header" {
		c.RenderForbidden(gc, "Set-password is not available in proxy-header mode.")
		return
	}
	ctx := gc.Request.Context()
	username := strings.TrimSpace(gc.PostForm("username"))
	plaintextToken := strings.TrimSpace(gc.PostForm("token"))
	newPw := gc.PostForm("new_password")
	confirm := gc.PostForm("confirm_password")

	data := setPasswordFormData{Username: username, Errors: map[string]string{}}
	failGeneric := func() {
		data.Errors["form"] = "Couldn't set password. Check your username, personal access token, and that both password fields match."
		c.RenderWithLayout(gc, "layouts/auth", "pages/setpassword", data)
	}

	if username == "" || plaintextToken == "" || newPw == "" || confirm == "" {
		failGeneric()
		return
	}
	if newPw != confirm {
		data.Errors["confirm_password"] = "Passwords don't match."
		c.RenderWithLayout(gc, "layouts/auth", "pages/setpassword", data)
		return
	}
	if len(newPw) < 12 {
		data.Errors["new_password"] = "Use at least 12 characters."
		c.RenderWithLayout(gc, "layouts/auth", "pages/setpassword", data)
		return
	}

	// Validate user + token. Both wrong -> same generic error.
	u, err := c.users.GetByName(ctx, username)
	if err != nil {
		if errors.Is(err, users.ErrNotExist) {
			c.auditLogin(gc, nil, "console.password.set.deny", "no such user")
			failGeneric()
			return
		}
		c.RenderError(gc, "look up user", err)
		return
	}
	tok, err := c.tokens.Lookup(ctx, plaintextToken)
	if err != nil {
		if errors.Is(err, tokens.ErrNotExist) || errors.Is(err, tokens.ErrExpired) {
			c.auditLogin(gc, u, "console.password.set.deny", "bad token")
			failGeneric()
			return
		}
		c.RenderError(gc, "verify token", err)
		return
	}
	if tok.UserID != u.ID {
		c.auditLogin(gc, u, "console.password.set.deny", "token belongs to different user")
		failGeneric()
		return
	}

	// All checks passed; hash + set + sign in.
	hashed, err := users.HashPassword(newPw)
	if err != nil {
		c.RenderError(gc, "hash password", err)
		return
	}
	if err := c.users.SetPasswordHash(ctx, u.ID, hashed); err != nil {
		c.RenderError(gc, "save password", err)
		return
	}
	if err := middleware.LoginSession(gc, u.ID); err != nil {
		c.RenderError(gc, "save session", err)
		return
	}
	c.loginLimit.Forget(middleware.ClientIPFor(gc.Request, c.cfg.TrustedProxies))
	c.auditLogin(gc, u, "console.password.set", "")
	middleware.AddFlash(gc, middleware.FlashSuccess, "Password set. You're signed in.")
	gc.Redirect(http.StatusSeeOther, "/console/")
}

// auditLogin emits a console.* audit row. Safe to call with u==nil for
// failed-login cases (actor_user_id stays NULL).
func (c *Console) auditLogin(gc *gin.Context, u *users.User, action, reason string) {
	if c.audit == nil {
		return
	}
	ev := audit.Event{
		Action:     action,
		ActorKind:  "session",
		RequestID:  middleware.RequestIDFrom(gc),
		RemoteAddr: middleware.ClientIPFor(gc.Request, c.cfg.TrustedProxies),
		UserAgent:  gc.Request.UserAgent(),
		Reason:     reason,
	}
	if u != nil {
		ev.UserID = u.ID
	}
	c.audit.Log(ev)
}
