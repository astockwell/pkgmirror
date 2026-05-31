// Package server wires the Gin engine, templates, auth middleware, and
// route handlers together.
package server

import (
	"fmt"
	"html/template"
	"io/fs"
	"net/http"

	"github.com/astockwell/pkgmirror/internal/admin"
	"github.com/astockwell/pkgmirror/internal/audit"
	"github.com/astockwell/pkgmirror/internal/auth"
	"github.com/astockwell/pkgmirror/internal/models"
	pkgsvc "github.com/astockwell/pkgmirror/internal/packages"
	"github.com/astockwell/pkgmirror/internal/packages/alpine"
	"github.com/astockwell/pkgmirror/internal/packages/container"
	"github.com/astockwell/pkgmirror/internal/packages/cran"
	"github.com/astockwell/pkgmirror/internal/packages/debian"
	"github.com/astockwell/pkgmirror/internal/packages/generic"
	"github.com/astockwell/pkgmirror/internal/packages/goproxy"
	"github.com/astockwell/pkgmirror/internal/packages/maven"
	"github.com/astockwell/pkgmirror/internal/packages/npm"
	"github.com/astockwell/pkgmirror/internal/packages/nuget"
	"github.com/astockwell/pkgmirror/internal/packages/pypi"
	"github.com/astockwell/pkgmirror/internal/packages/rpm"
	"github.com/astockwell/pkgmirror/internal/packages/rubygems"
	"github.com/astockwell/pkgmirror/internal/policy"
	"github.com/astockwell/pkgmirror/internal/tenants"
	"github.com/astockwell/pkgmirror/internal/ui"
	"github.com/astockwell/pkgmirror/internal/upstream"

	"github.com/gin-gonic/gin"
)

// Deps bundles the dependencies needed to construct an engine.
type Deps struct {
	Service       *pkgsvc.Service
	Models        *models.Store
	Tenants       *tenants.Store
	Authenticator auth.Authenticator
	// Engine is the supply-chain policy engine. If nil, defaults to
	// policy.NoopEngine{} (allow-everything).
	Engine policy.Engine
	// Rules + Audit power the /admin endpoints. If both are nil, no
	// admin routes are mounted.
	Rules     *policy.RuleStore
	Audit     audit.Logger
	Templates fs.FS
	// Upstream is the per-(tenant,format) pull-through fetcher. Per-format
	// handlers that support pull-through (PRs F-P) read this from the
	// gin context they're handed; nil means pull-through is disabled
	// process-wide (tests, ops that explicitly opted out).
	Upstream upstream.Fetcher
}

// New constructs a configured *gin.Engine.
//
// The auth middleware is mounted per-route-group rather than globally,
// so the registry, admin REST, public UI, and (in the future) the web
// console can each be wired to their own Authenticator. Today every
// non-healthz group runs the same TokenAuthenticator passed in via
// Deps.Authenticator; the per-group seam is what lets the web console
// arrive with a SessionAuthenticator / ProxyHeaderAuthenticator on its
// own /console subtree without touching the registry's auth at all.
func New(d Deps) (*gin.Engine, error) {
	if d.Engine == nil {
		d.Engine = policy.NoopEngine{}
	}

	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())
	r.Use(policyActorMiddleware())

	tmpl, err := template.New("").Funcs(template.FuncMap{
		"humanBytes": humanBytes,
	}).ParseFS(d.Templates, "*.html")
	if err != nil {
		return nil, err
	}
	r.SetHTMLTemplate(tmpl)

	// Single shared instance of the registry/admin/UI auth middleware.
	// auth.Middleware only populates *auth.Identity into the gin context
	// on success; enforcement (RequireRead / RequireWrite / IsSystemAdmin)
	// happens in the handlers themselves.
	tokenAuth := auth.Middleware(d.Authenticator)

	// Format-specific API groups: /api/packages/:tenant/<format>/...
	// All gated by tokenAuth. The :tenant path param is resolved inside
	// each handler before applying RequireRead / RequireWrite.
	apiBase := r.Group("/api/packages/:tenant", tokenAuth)
	goproxy.NewHandler(d.Service, d.Models, d.Tenants, d.Engine).WithUpstream(d.Upstream).Register(apiBase.Group("/go"))
	pypi.NewHandler(d.Service, d.Models, d.Tenants, d.Engine).WithUpstream(d.Upstream).Register(apiBase.Group("/pypi"))
	npm.NewHandler(d.Service, d.Models, d.Tenants, d.Engine).Register(apiBase.Group("/npm"))
	rubygems.NewHandler(d.Service, d.Models, d.Tenants, d.Engine).Register(apiBase.Group("/rubygems"))
	generic.NewHandler(d.Service, d.Models, d.Tenants, d.Engine).Register(apiBase.Group("/generic"))
	alpine.NewHandler(d.Service, d.Models, d.Tenants, d.Engine).Register(apiBase.Group("/alpine"))
	maven.NewHandler(d.Service, d.Models, d.Tenants, d.Engine).Register(apiBase.Group("/maven"))
	debian.NewHandler(d.Service, d.Models, d.Tenants, d.Engine).Register(apiBase.Group("/debian"))
	rpm.NewHandler(d.Service, d.Models, d.Tenants, d.Engine).Register(apiBase.Group("/rpm"))
	nuget.NewHandler(d.Service, d.Models, d.Tenants, d.Engine).Register(apiBase.Group("/nuget"))
	cran.NewHandler(d.Service, d.Models, d.Tenants, d.Engine).Register(apiBase.Group("/cran"))

	// Container (OCI) lives at the root /v2/... per the OCI distribution
	// spec; clients don't tolerate a path prefix. Tenant is the first
	// path segment after /v2. Auth is applied per-route via the variadic
	// middleware Register accepts (gin can't Group on a wildcard path).
	container.NewHandler(d.Service, d.Models, d.Tenants, d.Engine).Register(r, tokenAuth)

	// Admin REST API. Token auth + admin-scope gate; the latter lives
	// inside admin.Handler.requireSystemAdmin and runs after tokenAuth.
	if d.Rules != nil {
		(&admin.Handler{
			Models: d.Models,
			Rules:  d.Rules,
			Audit:  d.Audit,
		}).Register(r, tokenAuth)
	}

	// Public UI. Registers /-/healthz top-level (anonymous) plus the
	// authed `/` and `/t/...` routes via the supplied middleware.
	// auth.FromContext returns nil for unauthenticated callers; the
	// UI handlers rely on that to filter tenant visibility.
	ui.New(d.Service, d.Models, d.Tenants).Register(r, tokenAuth)

	r.NoRoute(func(c *gin.Context) {
		c.String(http.StatusNotFound, "not found")
	})

	return r, nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return formatBytes(float64(n), "B")
	}
	div, exp := float64(unit), 0
	for x := float64(n) / unit; x >= unit && exp < 4; x /= unit {
		div *= unit
		exp++
	}
	suffix := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}[exp]
	return formatBytes(float64(n)/div, suffix)
}

func formatBytes(v float64, suffix string) string {
	s := fmt.Sprintf("%.2f", v)
	for len(s) > 0 && s[len(s)-1] == '0' {
		s = s[:len(s)-1]
	}
	if len(s) > 0 && s[len(s)-1] == '.' {
		s = s[:len(s)-1]
	}
	return s + " " + suffix
}
