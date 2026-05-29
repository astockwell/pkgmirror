package console

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/astockwell/pkgmirror/internal/audit"
	"github.com/astockwell/pkgmirror/internal/auth"
	"github.com/astockwell/pkgmirror/internal/console/middleware"
	"github.com/astockwell/pkgmirror/internal/tokens"
	"github.com/astockwell/pkgmirror/internal/users"

	"github.com/gin-gonic/gin"
)

// ---- profile ----

type profileData struct {
	UserID       int64
	UserName     string
	Email        string
	IsAdmin      bool
	AuthKind     string
	PasswordSet  bool
	LastSeenUnix int64
	Memberships  []profileMembership
}

type profileMembership struct {
	TenantID   int64
	TenantName string
	RoleName   string
}

func (c *Console) profilePage(gc *gin.Context) {
	ctx := gc.Request.Context()
	id := auth.FromContext(gc)
	if id == nil || id.User == nil {
		c.RenderError(gc, "load profile", errors.New("no identity"))
		return
	}
	email := ""
	if id.User.Email.Valid {
		email = id.User.Email.String
	}
	// We don't load the hash; just check the password_set_unix column
	// to tell "has a password set" from "no password set yet". Skip the
	// extra query for proxy-header mode where password is irrelevant.
	pwSet := false
	if c.cfg.AuthMode == "password" {
		var setUnix int64
		_ = c.users.DB.QueryRowContext(ctx,
			`SELECT COALESCE(password_set_unix, 0) FROM users WHERE id = ?`, id.User.ID).Scan(&setUnix)
		pwSet = setUnix > 0
	}

	// Memberships: build list with tenant names.
	mems := make([]profileMembership, 0, len(id.Memberships))
	for tid, role := range id.Memberships {
		t, err := c.tenants.GetByID(ctx, tid)
		if err != nil {
			continue
		}
		mems = append(mems, profileMembership{
			TenantID:   tid,
			TenantName: t.Name,
			RoleName:   roleString(role),
		})
	}

	c.Render(gc, "pages/profile", profileData{
		UserID:       id.User.ID,
		UserName:     id.User.Name,
		Email:        email,
		IsAdmin:      id.User.IsAdmin,
		AuthKind:     credentialKindString(id.Kind),
		PasswordSet:  pwSet,
		LastSeenUnix: id.User.LastSeenUnix,
		Memberships:  mems,
	})
}

func credentialKindString(k auth.CredentialKind) string {
	switch k {
	case auth.CredentialToken:
		return "token (PAT)"
	case auth.CredentialSession:
		return "session (password)"
	case auth.CredentialProxy:
		return "proxy-header"
	default:
		return "unknown"
	}
}

// ---- profile: change password ----

type changePasswordForm struct {
	Errors map[string]string
}

func (c *Console) profileChangePassword(gc *gin.Context) {
	if c.cfg.AuthMode != "password" {
		c.RenderForbidden(gc, "Password changes aren't available in proxy-header mode.")
		return
	}
	ctx := gc.Request.Context()
	id := auth.FromContext(gc)
	if id == nil || id.User == nil {
		c.RenderError(gc, "change password", errors.New("no identity"))
		return
	}
	current := gc.PostForm("current_password")
	newPw := gc.PostForm("new_password")
	confirm := gc.PostForm("confirm_password")

	failFlash := func(text string) {
		middleware.AddFlash(gc, middleware.FlashDanger, text)
		gc.Redirect(http.StatusSeeOther, "/console/profile")
	}

	if current == "" || newPw == "" || confirm == "" {
		failFlash("All three password fields are required.")
		return
	}
	if newPw != confirm {
		failFlash("New password and confirmation don't match.")
		return
	}
	if len(newPw) < 12 {
		failFlash("New password must be at least 12 characters.")
		return
	}
	// Verify the current password.
	if _, err := c.users.VerifyPassword(ctx, id.User.Name, current); err != nil {
		failFlash("Current password is incorrect.")
		return
	}
	// Hash the new password and store.
	newHash, herr := users.HashPassword(newPw)
	if herr != nil {
		c.RenderError(gc, "hash password", herr)
		return
	}
	if err := c.users.SetPasswordHash(ctx, id.User.ID, newHash); err != nil {
		c.RenderError(gc, "save password", err)
		return
	}
	c.auditTokenOrProfile(gc, id, "console.password.set", "")
	middleware.AddFlash(gc, middleware.FlashSuccess, "Password changed.")
	gc.Redirect(http.StatusSeeOther, "/console/profile")
}

// ---- tokens: list ----

type tokensListData struct {
	Tokens     []tokenRow
	NewPlain   string // populated on the request just after mint
	NewName    string
}

type tokenRow struct {
	ID           int64
	Name         string
	Scopes       string
	TenantScope  string
	CreatedUnix  int64
	LastUsedUnix int64
	ExpiresUnix  int64
}

func (c *Console) tokensList(gc *gin.Context) {
	ctx := gc.Request.Context()
	id := auth.FromContext(gc)
	if id == nil || id.User == nil {
		c.RenderError(gc, "list tokens", errors.New("no identity"))
		return
	}
	toks, err := c.tokens.ListByUser(ctx, id.User.ID)
	if err != nil {
		c.RenderError(gc, "list tokens", err)
		return
	}
	rows := make([]tokenRow, 0, len(toks))
	for _, t := range toks {
		ts := ""
		if t.TenantScope.Valid {
			ts = fmt.Sprintf("#%d", t.TenantScope.Int64)
		}
		scopeNames := make([]string, 0, len(t.Scopes))
		for _, s := range t.Scopes {
			scopeNames = append(scopeNames, string(s))
		}
		rows = append(rows, tokenRow{
			ID: t.ID, Name: t.Name,
			Scopes:       strings.Join(scopeNames, ", "),
			TenantScope:  ts,
			CreatedUnix:  t.CreatedUnix,
			LastUsedUnix: t.LastUsedUnix,
			ExpiresUnix:  t.ExpiresUnix,
		})
	}

	// One-shot reveal of a freshly-minted token. We stash it in the
	// session (path-scoped, encrypted) and consume on the next render.
	data := tokensListData{Tokens: rows}
	if raw := middleware.PopOneShotToken(gc); raw != "" {
		data.NewPlain = raw
		data.NewName = middleware.PopOneShotTokenName(gc)
	}
	c.Render(gc, "pages/tokens/list", data)
}

// ---- tokens: mint ----

func (c *Console) tokenMint(gc *gin.Context) {
	ctx := gc.Request.Context()
	id := auth.FromContext(gc)
	if id == nil || id.User == nil {
		c.RenderError(gc, "mint token", errors.New("no identity"))
		return
	}
	name := strings.TrimSpace(gc.PostForm("name"))
	scopeRaw := gc.PostFormArray("scope")
	tenantScopeStr := strings.TrimSpace(gc.PostForm("tenant_scope"))
	expiresInDays := strings.TrimSpace(gc.PostForm("expires_in_days"))

	if name == "" {
		middleware.AddFlash(gc, middleware.FlashDanger, "Token name is required.")
		gc.Redirect(http.StatusSeeOther, "/console/tokens")
		return
	}
	if len(name) > 80 {
		middleware.AddFlash(gc, middleware.FlashDanger, "Token name max 80 chars.")
		gc.Redirect(http.StatusSeeOther, "/console/tokens")
		return
	}
	scopes := make([]tokens.Scope, 0, len(scopeRaw))
	for _, s := range scopeRaw {
		switch tokens.Scope(s) {
		case tokens.ScopeRead, tokens.ScopeWrite, tokens.ScopeAdmin:
			scopes = append(scopes, tokens.Scope(s))
		}
	}
	if len(scopes) == 0 {
		middleware.AddFlash(gc, middleware.FlashDanger, "Pick at least one scope.")
		gc.Redirect(http.StatusSeeOther, "/console/tokens")
		return
	}
	if !id.User.IsAdmin {
		// Strip ScopeAdmin from non-admins. Defense in depth: the
		// underlying CanRead/CanWrite would never honor it anyway, but
		// the UI shouldn't let it through.
		var filtered []tokens.Scope
		for _, s := range scopes {
			if s != tokens.ScopeAdmin {
				filtered = append(filtered, s)
			}
		}
		scopes = filtered
	}

	opts := tokens.CreateOptions{
		UserID: id.User.ID,
		Name:   name,
		Scopes: scopes,
	}
	if tenantScopeStr != "" {
		if tid, err := strconv.ParseInt(tenantScopeStr, 10, 64); err == nil && tid > 0 {
			if !id.CanRead(tid) {
				middleware.AddFlash(gc, middleware.FlashDanger, "You can't scope a token to a tenant you don't have access to.")
				gc.Redirect(http.StatusSeeOther, "/console/tokens")
				return
			}
			opts.TenantScope = tid
		}
	}
	if expiresInDays != "" {
		if d, err := strconv.Atoi(expiresInDays); err == nil && d > 0 {
			opts.ExpiresUnix = time.Now().Add(time.Duration(d) * 24 * time.Hour).Unix()
		}
	}

	plain, tok, err := c.tokens.Issue(ctx, opts)
	if err != nil {
		c.RenderError(gc, "mint token", err)
		return
	}
	// One-shot reveal: stash the plaintext in the session so the next
	// GET /console/tokens shows it once, then clears it.
	_ = middleware.SetOneShotToken(gc, plain, name)
	c.auditTokenOrProfile(gc, id, "tokens.mint", fmt.Sprintf("token #%d (%s)", tok.ID, name))
	gc.Redirect(http.StatusSeeOther, "/console/tokens")
}

// ---- tokens: revoke ----

func (c *Console) tokenRevoke(gc *gin.Context) {
	ctx := gc.Request.Context()
	id := auth.FromContext(gc)
	if id == nil || id.User == nil {
		c.RenderError(gc, "revoke token", errors.New("no identity"))
		return
	}
	tid, err := strconv.ParseInt(gc.Param("id"), 10, 64)
	if err != nil || tid <= 0 {
		c.RenderNotFound(gc, "token not specified")
		return
	}
	tok, err := c.tokens.GetByID(ctx, tid)
	if err != nil {
		if errors.Is(err, tokens.ErrNotExist) {
			c.RenderNotFound(gc, "token not found")
			return
		}
		c.RenderError(gc, "look up token", err)
		return
	}
	// Ownership check: regular users can only revoke their own tokens.
	// System admins can revoke anyones.
	if tok.UserID != id.User.ID && !id.IsSystemAdmin() {
		c.RenderForbidden(gc, "You can only revoke your own tokens.")
		return
	}
	if err := c.tokens.Revoke(ctx, tid); err != nil {
		c.RenderError(gc, "revoke token", err)
		return
	}
	c.auditTokenOrProfile(gc, id, "tokens.revoke", fmt.Sprintf("token #%d (%s)", tid, tok.Name))
	middleware.AddFlash(gc, middleware.FlashSuccess, fmt.Sprintf("Token %q revoked.", tok.Name))
	gc.Redirect(http.StatusSeeOther, "/console/tokens")
}

// auditTokenOrProfile records an audit row for the token/profile
// surface. Safe to call without an Audit logger configured.
func (c *Console) auditTokenOrProfile(gc *gin.Context, id *auth.Identity, action, reason string) {
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
	if id != nil && id.User != nil {
		ev.UserID = id.User.ID
	}
	c.audit.Log(ev)
}
