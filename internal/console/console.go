// Package console serves the pkgmirror operator web UI at /console.
//
// The console is feature-flagged: PKGMIRROR_CONSOLE_ENABLED=false strips
// it from the binary at startup. When enabled it lives at /console/...
// and serves its own static assets under /console/static/...
//
// See plans/web-console.md for architecture and
// plans/web-console-implementation-plan.md for conventions. The auth
// model that powers the console (CredentialKind) lives in
// internal/auth. The two console authenticators (password mode and
// proxy-header mode) live in internal/console/middleware.
package console

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/astockwell/pkgmirror/internal/audit"
	"github.com/astockwell/pkgmirror/internal/auth"
	"github.com/astockwell/pkgmirror/internal/console/middleware"
	"github.com/astockwell/pkgmirror/internal/models"
	"github.com/astockwell/pkgmirror/internal/policy"
	"github.com/astockwell/pkgmirror/internal/tenants"
	"github.com/astockwell/pkgmirror/internal/tokens"
	"github.com/astockwell/pkgmirror/internal/upstream"
	"github.com/astockwell/pkgmirror/internal/users"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/csrf"
)

// Deps bundles the dependencies a Console needs.
//
// Authenticator selects the credential source: in password mode, pass
// a SessionAuthenticator; in proxy-header mode (PR 2c), a
// ProxyHeaderAuthenticator. Leave nil only in tests of the foundation
// chain (PR 1).
type Deps struct {
	Config        Config
	Logger        *slog.Logger
	Users         *users.Store
	Tenants       *tenants.Store
	Models        *models.Store
	Tokens        *tokens.Store
	Audit         audit.Logger
	Rules         *policy.RuleStore
	Authenticator auth.Authenticator
	AppVersion    string
	// Upstreams is the per-(tenant, format) pull-through config store.
	// Optional; nil hides the /console/tenants/:name/upstreams admin
	// page so test suites and ops who built without pull-through don't
	// see broken links.
	Upstreams UpstreamConfigStore
}

// UpstreamConfigStore is the surface the console needs to read+write
// per-tenant pull-through config. Implemented by internal/upstreamstore.
type UpstreamConfigStore interface {
	ListByTenant(ctx context.Context, tenantID int64) (map[string]*upstream.PersistedConfig, error)
	Set(ctx context.Context, tenantID int64, format, mode, upstreamURL string, metadataTTLSec int64) error
	Delete(ctx context.Context, tenantID int64, format string) error
}

// Console is the web-console DSO. One per process.
type Console struct {
	cfg         Config
	logger      *slog.Logger
	users       *users.Store
	tenants     *tenants.Store
	models      *models.Store
	tokens      *tokens.Store
	audit       audit.Logger
	rules       *policy.RuleStore
	upstreams   UpstreamConfigStore
	auth        auth.Authenticator
	templates   *template.Template
	middleware  *middleware.Middleware
	loginLimit  *middleware.LoginRateLimiter
	authedGroup *gin.RouterGroup // populated by Register; consumed by AuthedGroup
	appVersion  string
}

// New constructs a Console from Deps. Returns an error if any of:
//   - the template set fails to parse (embed loader walks the FS)
//   - the config doesn't validate
//   - the session middleware fails to build (bad key length, etc.)
func New(d Deps) (*Console, error) {
	if d.Logger == nil {
		d.Logger = slog.Default().With("component", "console")
	}
	if err := d.Config.Validate(); err != nil {
		return nil, fmt.Errorf("console config: %w", err)
	}
	if d.AppVersion == "" {
		d.AppVersion = "dev"
	}
	tmpl, err := loadTemplates(d.Config.DevDir)
	if err != nil {
		return nil, fmt.Errorf("load templates: %w", err)
	}
	c := &Console{
		cfg:        d.Config,
		logger:     d.Logger,
		users:      d.Users,
		tenants:    d.Tenants,
		models:     d.Models,
		tokens:     d.Tokens,
		audit:      d.Audit,
		rules:      d.Rules,
		upstreams:  d.Upstreams,
		auth:       d.Authenticator,
		templates:  tmpl,
		appVersion: d.AppVersion,
		loginLimit: middleware.NewLoginRateLimiter(10, time.Minute),
	}
	c.middleware = middleware.New(middleware.Deps{
		Logger:   c.logger,
		AuthMode: c.cfg.AuthMode,
	})
	return c, nil
}

// Register mounts the console on r. Returns nil without mounting if
// Config.Enabled is false (handled by the caller deciding whether to
// invoke Register at all, but this is a defensive belt-and-braces check).
func (c *Console) Register(r *gin.Engine) error {
	if !c.cfg.Enabled {
		return nil
	}

	// Static assets first, under /console/static/, anonymous-readable.
	// Either from disk (DevDir set) or from the embedded FS.
	if c.cfg.DevDir != "" {
		r.Static("/console/static", c.cfg.DevDir+"/static")
	} else {
		staticFS, err := embeddedStaticFS()
		if err != nil {
			return fmt.Errorf("static fs: %w", err)
		}
		r.StaticFS("/console/static", http.FS(staticFS))
	}

	// Build the per-console middleware chain. Order matters and is
	// documented in plans/web-console-implementation-plan.md §9.
	sess, err := middleware.NewSessionMiddleware(middleware.SessionConfig{
		AuthKey: c.cfg.Session.AuthKey,
		EncKey:  c.cfg.Session.EncKey,
		TTL:     c.cfg.Session.TTL,
		Secure:  c.cfg.Session.Secure,
		Path:    c.cfg.Session.Path,
	})
	if err != nil {
		return fmt.Errorf("session middleware: %w", err)
	}

	chain := []gin.HandlerFunc{
		middleware.Recover(c.logger),
		middleware.RequestID(),
		middleware.AccessLog(c.logger),
		middleware.SecurityHeaders(c.cfg.UsingTLS),
		middleware.WithGinContext(), // must precede session/auth so they can read *gin.Context out of ctx
		sess,
		middleware.CSRF(c.cfg.CSRFKey, c.cfg.UsingTLS),
	}
	if c.auth != nil {
		chain = append(chain, auth.Middleware(c.auth))
	}

	g := r.Group("/console", chain...)

	// Anonymous routes (login, logout, set-password, ping).
	g.GET("/_ping", c.ping)
	g.GET("/login", c.loginPage)
	g.POST("/login", middleware.LoginRateLimitMiddleware(c.loginLimit, c.cfg.TrustedProxies), c.loginSubmit)
	g.POST("/logout", c.logout)
	g.GET("/set-password", c.setPasswordPage)
	g.POST("/set-password", middleware.LoginRateLimitMiddleware(c.loginLimit, c.cfg.TrustedProxies), c.setPasswordSubmit)

	// Authenticated routes go in a child group with RequireAuth.
	// Subsequent PRs (3-8) hang their handlers off `authed`.
	authed := g.Group("", c.middleware.RequireAuth())
	c.authedGroup = authed

	// Dashboard.
	authed.GET("/", c.dashboard)

	// Tenants. List + detail are member-readable (the handlers filter
	// by CanRead per row); state-changing routes are system-admin only.
	authed.GET("/tenants", c.tenantsList)
	authed.GET("/tenants/:name", c.tenantDetail)
	authed.GET("/tenants/:name/packages", c.packagesByTenant)
	authed.GET("/tenants/:name/packages/:type/:pkgname", c.packageDetail)
	adminOnly := authed.Group("", c.middleware.RequireSystemAdmin())
	adminOnly.GET("/tenants/new", c.tenantNew)
	adminOnly.POST("/tenants", c.tenantCreate)
	adminOnly.POST("/tenants/:name/visibility", c.tenantSetVisibility)
	adminOnly.POST("/tenants/:name/members", c.tenantMemberAdd)
	adminOnly.POST("/tenants/:name/members/:user_id/delete", c.tenantMemberRemove)

	// Upstream pull-through (system admin only; the host allowlist is
	// NOT editable here on purpose - see plans/upstream-pull-through.md
	// S2 and S5). Only mounted when an UpstreamConfigStore was supplied.
	if c.upstreams != nil {
		adminOnly.GET("/tenants/:name/upstreams", c.tenantUpstreams)
		adminOnly.POST("/tenants/:name/upstreams/:format", c.tenantUpstreamUpsert)
		adminOnly.POST("/tenants/:name/upstreams/:format/delete", c.tenantUpstreamReset)
	}

	// Quarantine (system admin only - cross-tenant view + actions).
	adminOnly.GET("/quarantine", c.quarantineList)
	adminOnly.POST("/quarantine/:version_id/promote", c.quarantinePromote)
	adminOnly.POST("/quarantine/:version_id/reject", c.quarantineReject)

	// Audit (system admin only).
	adminOnly.GET("/audit", c.auditList)
	adminOnly.GET("/audit.csv", c.auditExportCSV)

	// Rules (system admin only).
	adminOnly.GET("/rules", c.rulesList)
	adminOnly.GET("/rules/new", c.ruleNew)
	adminOnly.GET("/rules/:id", c.ruleEdit)
	adminOnly.POST("/rules", c.ruleUpsert)
	adminOnly.POST("/rules/:id/enabled", c.ruleSetEnabled)
	adminOnly.POST("/rules/:id/delete", c.ruleDelete)

	// Tokens + profile (any signed-in user; ownership checks in handlers).
	authed.GET("/profile", c.profilePage)
	authed.POST("/profile/password", c.profileChangePassword)
	authed.GET("/tokens", c.tokensList)
	authed.POST("/tokens", c.tokenMint)
	authed.POST("/tokens/:id/delete", c.tokenRevoke)

	return nil
}

// Render executes the layout against a page template. The page must be
// the name passed to `{{ define "..." }}` in one of the .tmpl files
// loaded into the console's template set (e.g. "pages/home/dashboard").
//
// body is the page-specific data struct; baseData() builds the request-
// scoped chrome (identity, flashes, CSRF, etc.).
func (c *Console) Render(gc *gin.Context, page string, body any) {
	base, err := c.baseData(gc)
	if err != nil {
		c.RenderError(gc, "build page chrome", err)
		return
	}
	wrapper := pageWrapper{
		Page: page,
		Base: base,
		Body: body,
	}
	gc.Status(http.StatusOK)
	gc.Header("Content-Type", "text/html; charset=utf-8")
	if err := c.templates.ExecuteTemplate(gc.Writer, "layouts/base", wrapper); err != nil {
		// Recovery middleware catches this; logging here makes the
		// failure mode easier to diagnose ("which template failed").
		c.logger.Error("render", "page", page, "err", err)
		_ = gc.Error(err)
	}
}

// RenderWithLayout is like Render but uses the named layout instead of
// layouts/base. Used by the login page (PR 2) which has its own layout
// without the sidebar/navigation.
func (c *Console) RenderWithLayout(gc *gin.Context, layout, page string, body any) {
	base, err := c.baseData(gc)
	if err != nil {
		c.RenderError(gc, "build page chrome", err)
		return
	}
	wrapper := pageWrapper{Page: page, Base: base, Body: body}
	gc.Status(http.StatusOK)
	gc.Header("Content-Type", "text/html; charset=utf-8")
	if err := c.templates.ExecuteTemplate(gc.Writer, layout, wrapper); err != nil {
		c.logger.Error("render", "layout", layout, "page", page, "err", err)
		_ = gc.Error(err)
	}
}

// RenderError emits a friendly 500 page. The supplied verb is shown to
// the user (e.g. "create tenant"); the underlying err is logged via the
// console's slog logger with request-id + actor for ops to correlate.
func (c *Console) RenderError(gc *gin.Context, verb string, err error) {
	rid := middleware.RequestIDFrom(gc)
	id := auth.FromContext(gc)
	var actorID int64
	if id != nil && id.User != nil {
		actorID = id.User.ID
	}
	c.logger.Error("console handler error",
		"verb", verb,
		"err", err,
		"request_id", rid,
		"actor_user_id", actorID,
		"path", gc.Request.URL.Path,
	)
	gc.Status(http.StatusInternalServerError)
	gc.Header("Content-Type", "text/html; charset=utf-8")
	// Minimal inline fallback if the error page template itself fails.
	body := errorPageData{Verb: verb, RequestID: rid}
	base, _ := c.baseData(gc) // best effort
	wrapper := pageWrapper{Page: "pages/_error", Base: base, Body: body}
	if execErr := c.templates.ExecuteTemplate(gc.Writer, "layouts/base", wrapper); execErr != nil {
		_, _ = io.WriteString(gc.Writer,
			"Something went wrong serving this page. Request ID: "+rid)
	}
}

// RenderNotFound emits a friendly 404 page.
func (c *Console) RenderNotFound(gc *gin.Context, format string, args ...any) {
	gc.Status(http.StatusNotFound)
	gc.Header("Content-Type", "text/html; charset=utf-8")
	base, _ := c.baseData(gc)
	wrapper := pageWrapper{
		Page: "pages/_notfound",
		Base: base,
		Body: notFoundPageData{Message: fmt.Sprintf(format, args...)},
	}
	if err := c.templates.ExecuteTemplate(gc.Writer, "layouts/base", wrapper); err != nil {
		_, _ = io.WriteString(gc.Writer, "Not found.")
	}
}

// RenderForbidden emits a friendly 403 page.
func (c *Console) RenderForbidden(gc *gin.Context, format string, args ...any) {
	gc.Status(http.StatusForbidden)
	gc.Header("Content-Type", "text/html; charset=utf-8")
	base, _ := c.baseData(gc)
	wrapper := pageWrapper{
		Page: "pages/_forbidden",
		Base: base,
		Body: forbiddenPageData{Message: fmt.Sprintf(format, args...)},
	}
	if err := c.templates.ExecuteTemplate(gc.Writer, "layouts/base", wrapper); err != nil {
		_, _ = io.WriteString(gc.Writer, "Forbidden.")
	}
}

// baseData populates the per-request page chrome (identity, flashes,
// CSRF nonce, etc.). Errors here should be rare and propagate to the
// caller as a 500 (something is fundamentally wrong with the request
// context).
func (c *Console) baseData(gc *gin.Context) (BaseData, error) {
	flashes := middleware.ReadFlashes(gc)
	base := BaseData{
		Identity:    auth.FromContext(gc),
		Flash:       flashes,
		CSRFField:   csrf.TemplateField(gc.Request),
		AppVersion:  c.appVersion,
		ConsolePath: "/console",
		DarkMode:    c.resolveDarkMode(gc),
		Now:         time.Now(),
	}
	if base.Identity != nil && base.Identity.User != nil && c.tenants != nil {
		all, err := c.tenants.List(gc.Request.Context())
		if err != nil {
			return base, fmt.Errorf("list tenants: %w", err)
		}
		for _, t := range all {
			if base.Identity.CanRead(t.ID) {
				base.Memberships = append(base.Memberships, t)
			}
		}
	}
	return base, nil
}

// resolveDarkMode picks dark/light/auto for the current request. Cookie
// wins; falls back to Config.DarkModeDefault.
func (c *Console) resolveDarkMode(gc *gin.Context) bool {
	if v, err := gc.Cookie("pkgmirror_theme"); err == nil {
		switch v {
		case "dark":
			return true
		case "light":
			return false
		}
	}
	return c.cfg.DarkModeDefault == "dark"
}

// ping is the minimal proof-of-life page that PR 1 ships. It exercises
// the layout, a sample flash, and a sample UI partial so the foundation
// can be validated end-to-end before any real pages land.
func (c *Console) ping(gc *gin.Context) {
	middleware.AddFlash(gc, middleware.FlashInfo, "Console foundation is up.")
	c.Render(gc, "pages/_ping", pingPageData{
		Title:   "pkgmirror console",
		Message: "If you're seeing this, the layout + static assets + middleware chain are wired.",
	})
}

// Logger exposes the console's logger to handlers that need to emit
// structured log lines under the same component label.
func (c *Console) Logger() *slog.Logger { return c.logger }

// Tenants exposes the underlying tenants.Store. Page handlers use it.
func (c *Console) Tenants() *tenants.Store { return c.tenants }

// Users exposes the underlying users.Store. Page handlers use it.
func (c *Console) Users() *users.Store { return c.users }

// Config exposes a copy of the validated config.
func (c *Console) Config() Config { return c.cfg }

// Middleware exposes the gate helpers (RequireAuth, RequireSystemAdmin,
// etc.). PR 2 starts populating them.
func (c *Console) Middleware() *middleware.Middleware { return c.middleware }

// AuthedGroup returns the gin.RouterGroup that has RequireAuth applied.
// Page handlers in subsequent PRs (3-8) hang their routes off this group
// so every page automatically inherits the auth gate.
//
// Returns nil before Register has been called.
func (c *Console) AuthedGroup() *gin.RouterGroup { return c.authedGroup }

// Audit returns the underlying audit.Logger. Console handlers use it to
// emit console.* audit rows (login, logout, password set, etc.).
func (c *Console) Audit() audit.Logger { return c.audit }

// Tokens returns the underlying tokens.Store. Used by the set-password
// flow to verify a user's PAT before letting them set a password.
func (c *Console) Tokens() *tokens.Store { return c.tokens }

// renderToBuffer is the implementation that the "tmpl" template func
// (registered in templates.go) calls. Kept on Console so it can log
// failures with the console's logger.
func (c *Console) renderToBuffer(name string, data any) (template.HTML, error) {
	var buf bytes.Buffer
	if err := c.templates.ExecuteTemplate(&buf, name, data); err != nil {
		return "", fmt.Errorf("tmpl %q: %w", name, err)
	}
	return template.HTML(buf.String()), nil //nolint:gosec // output is template-rendered
}

// SessionGet is a small helper for handlers that need to read a session
// value without importing gin-contrib/sessions. PR 2 uses it for the
// login set-password flow.
func SessionGet(gc *gin.Context, key string) any {
	return sessions.Default(gc).Get(key)
}

// SessionSet stores a key in the session and persists it. Returns any
// underlying save error so handlers can log/handle as they see fit.
func SessionSet(gc *gin.Context, key string, value any) error {
	s := sessions.Default(gc)
	s.Set(key, value)
	return s.Save()
}

// Compile-time check the package builds against the dependencies we
// actually need; remove once the real handlers land.
var _ = context.Background
