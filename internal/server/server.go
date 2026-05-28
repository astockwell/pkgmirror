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
	"github.com/astockwell/pkgmirror/internal/packages/goproxy"
	"github.com/astockwell/pkgmirror/internal/packages/npm"
	"github.com/astockwell/pkgmirror/internal/packages/pypi"
	"github.com/astockwell/pkgmirror/internal/packages/rubygems"
	"github.com/astockwell/pkgmirror/internal/policy"
	"github.com/astockwell/pkgmirror/internal/tenants"
	"github.com/astockwell/pkgmirror/internal/ui"

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
}

// New constructs a configured *gin.Engine.
func New(d Deps) (*gin.Engine, error) {
	if d.Engine == nil {
		d.Engine = policy.NoopEngine{}
	}

	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())
	r.Use(auth.Middleware(d.Authenticator))
	r.Use(policyActorMiddleware())

	tmpl, err := template.New("").Funcs(template.FuncMap{
		"humanBytes": humanBytes,
	}).ParseFS(d.Templates, "*.html")
	if err != nil {
		return nil, err
	}
	r.SetHTMLTemplate(tmpl)

	// Format-specific API groups: /api/packages/:tenant/<format>/...
	goGroup := r.Group("/api/packages/:tenant/go")
	goproxy.NewHandler(d.Service, d.Models, d.Tenants, d.Engine).Register(goGroup)

	pypiGroup := r.Group("/api/packages/:tenant/pypi")
	pypi.NewHandler(d.Service, d.Models, d.Tenants, d.Engine).Register(pypiGroup)

	npmGroup := r.Group("/api/packages/:tenant/npm")
	npm.NewHandler(d.Service, d.Models, d.Tenants, d.Engine).Register(npmGroup)

	rubygemsGroup := r.Group("/api/packages/:tenant/rubygems")
	rubygems.NewHandler(d.Service, d.Models, d.Tenants, d.Engine).Register(rubygemsGroup)

	if d.Rules != nil {
		(&admin.Handler{
			Models: d.Models,
			Rules:  d.Rules,
			Audit:  d.Audit,
		}).Register(r)
	}

	ui.New(d.Service, d.Models, d.Tenants).Register(r)

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
