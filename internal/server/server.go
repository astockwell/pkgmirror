// Package server wires the Gin engine, templates, and route handlers
// together.
package server

import (
	"fmt"
	"html/template"
	"io/fs"
	"net/http"

	"github.com/astockwell/pkgmirror/internal/models"
	pkgsvc "github.com/astockwell/pkgmirror/internal/packages"
	"github.com/astockwell/pkgmirror/internal/packages/goproxy"
	"github.com/astockwell/pkgmirror/internal/ui"

	"github.com/gin-gonic/gin"
)

// New constructs a configured *gin.Engine.
//
// templates is a filesystem (typically embed.FS via fs.Sub) containing the
// HTML templates relative to its root.
func New(svc *pkgsvc.Service, m *models.Store, templates fs.FS) (*gin.Engine, error) {
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	tmpl, err := template.New("").Funcs(template.FuncMap{
		"humanBytes": humanBytes,
	}).ParseFS(templates, "*.html")
	if err != nil {
		return nil, err
	}
	r.SetHTMLTemplate(tmpl)

	// API routes — Go module proxy
	goGroup := r.Group("/api/packages/go")
	goproxy.NewHandler(svc, m).Register(goGroup)

	// UI
	ui.New(svc, m).Register(r)

	r.NoRoute(func(c *gin.Context) {
		c.String(http.StatusNotFound, "not found")
	})

	return r, nil
}

// humanBytes formats a byte size for display (e.g. 1.4 MiB).
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
	// Two decimal places, trimmed.
	s := fmt.Sprintf("%.2f", v)
	for len(s) > 0 && s[len(s)-1] == '0' {
		s = s[:len(s)-1]
	}
	if len(s) > 0 && s[len(s)-1] == '.' {
		s = s[:len(s)-1]
	}
	return s + " " + suffix
}
