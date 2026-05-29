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

	"github.com/astockwell/pkgmirror/internal/auth"
	"github.com/astockwell/pkgmirror/internal/console/middleware"
	"github.com/astockwell/pkgmirror/internal/tenants"
	"github.com/astockwell/pkgmirror/internal/users"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/csrf"
)

// Deps bundles the dependencies a Console needs.
//
// Authenticator is left nil in PR 1 (no console auth yet); PR 2 wires
// it via NewAuthenticator(Config) which selects password or proxy-header
// mode based on Config.AuthMode.
type Deps struct {
	Config        Config
	Logger        *slog.Logger
	Users         *users.Store
	Tenants       *tenants.Store
	Authenticator auth.Authenticator // nil until PR 2 lands
	AppVersion    string
}

// Console is the web-console DSO. One per process.
type Console struct {
	cfg        Config
	logger     *slog.Logger
	users      *users.Store
	tenants    *tenants.Store
	auth       auth.Authenticator
	templates  *template.Template
	middleware *middleware.Middleware
	appVersion string
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
		auth:       d.Authenticator,
		templates:  tmpl,
		appVersion: d.AppVersion,
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

	// Session + CSRF + security headers + recovery + request-id + access
	// log middleware all apply to the rest of /console, in this order.
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

	g := r.Group("/console",
		middleware.Recover(c.logger),
		middleware.RequestID(),
		middleware.AccessLog(c.logger),
		middleware.SecurityHeaders(c.cfg.UsingTLS),
		sess,
		middleware.CSRF(c.cfg.CSRFKey, c.cfg.UsingTLS),
	)

	// PR 2 wires the authenticator middleware here:
	//   if c.auth != nil { g.Use(auth.Middleware(c.auth)) }
	// PR 1 has no console auth and no RequireAuth gate. /console/_ping
	// is anonymous so the PR can land standalone.

	g.GET("/_ping", c.ping)

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
