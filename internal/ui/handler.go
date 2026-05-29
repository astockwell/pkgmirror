// Package ui renders Bootstrap-based HTML pages for browsing tenants and
// the packages they contain.
package ui

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/astockwell/pkgmirror/internal/auth"
	"github.com/astockwell/pkgmirror/internal/models"
	pkgsvc "github.com/astockwell/pkgmirror/internal/packages"
	"github.com/astockwell/pkgmirror/internal/tenants"

	"github.com/gin-gonic/gin"
)

// Handler renders the HTML UI.
type Handler struct {
	Service *pkgsvc.Service
	Models  *models.Store
	Tenants *tenants.Store
}

// New constructs a UI handler.
func New(svc *pkgsvc.Service, m *models.Store, ts *tenants.Store) *Handler {
	return &Handler{Service: svc, Models: m, Tenants: ts}
}

// Register attaches UI routes to the engine.
//
// Optional middleware (typically the auth.Middleware that populates
// *auth.Identity into the gin context) is applied to every UI route
// except /-/healthz. Healthz stays anonymous so liveness probes don't
// hit the auth layer.
func (h *Handler) Register(r *gin.Engine, middleware ...gin.HandlerFunc) {
	// Healthz is always anonymous; register before the authed group.
	r.GET("/-/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	g := r.Group("", middleware...)
	g.GET("/", h.index)
	g.GET("/t/:tenant", h.tenantPage)
	g.GET("/t/:tenant/p/:type/*name", h.packageOrVersion)
}

type tenantRow struct {
	Name       string
	URL        string
	Visibility string
	PkgCount   int
}

func (h *Handler) index(c *gin.Context) {
	ctx := c.Request.Context()
	allTenants, err := h.Tenants.List(ctx)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	id := auth.FromContext(c)

	rows := make([]tenantRow, 0, len(allTenants))
	for _, t := range allTenants {
		visible := t.Visibility == tenants.VisibilityPublic ||
			(id != nil && id.CanRead(t.ID))
		if !visible {
			continue
		}
		pkgs, err := h.Models.ListPackages(ctx, t.ID, "")
		if err != nil {
			c.String(http.StatusInternalServerError, "%v", err)
			return
		}
		rows = append(rows, tenantRow{
			Name:       t.Name,
			URL:        "/t/" + t.Name,
			Visibility: visibilityLabel(t.Visibility),
			PkgCount:   len(pkgs),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })

	c.HTML(http.StatusOK, "index.html", gin.H{
		"Title":   "pkgmirror — tenants",
		"Tenants": rows,
		"IsAuthed": id != nil,
	})
}

type packageRow struct {
	Type    string
	Name    string
	URL     string
	Created string
}

func (h *Handler) tenantPage(c *gin.Context) {
	ctx := c.Request.Context()
	tenant, err := h.Tenants.GetByName(ctx, c.Param("tenant"))
	if err != nil {
		h.handleTenantError(c, err)
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}
	pkgs, err := h.Models.ListPackages(ctx, tenant.ID, "")
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	rows := make([]packageRow, 0, len(pkgs))
	for _, p := range pkgs {
		rows = append(rows, packageRow{
			Type:    string(p.Type),
			Name:    p.Name,
			URL:     fmt.Sprintf("/t/%s/p/%s/%s", tenant.Name, p.Type, p.Name),
			Created: time.Unix(p.CreatedUnix, 0).UTC().Format(time.RFC3339),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Type != rows[j].Type {
			return rows[i].Type < rows[j].Type
		}
		return rows[i].Name < rows[j].Name
	})
	c.HTML(http.StatusOK, "tenant.html", gin.H{
		"Title":      tenant.Name + " — pkgmirror",
		"Tenant":     tenant,
		"Visibility": visibilityLabel(tenant.Visibility),
		"Packages":   rows,
	})
}

type versionRow struct {
	Version string
	Created string
	URL     string
}

type fileRow struct {
	Name string
	Size int64
	SHA  string
}

func (h *Handler) packageOrVersion(c *gin.Context) {
	ctx := c.Request.Context()
	tenant, err := h.Tenants.GetByName(ctx, c.Param("tenant"))
	if err != nil {
		h.handleTenantError(c, err)
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}
	typ := models.Type(c.Param("type"))
	name := strings.TrimPrefix(c.Param("name"), "/")
	wantVersion := c.Query("v")

	pkg, err := h.Models.GetPackage(ctx, tenant.ID, typ, name)
	if err != nil {
		if errors.Is(err, models.ErrPackageNotExist) {
			c.HTML(http.StatusNotFound, "not_found.html", gin.H{
				"Title":   "Package not found",
				"Message": fmt.Sprintf("No package %s/%s in tenant %s", typ, name, tenant.Name),
			})
			return
		}
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	versions, err := h.Models.ListVersions(ctx, pkg.ID)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].CreatedUnix > versions[j].CreatedUnix })

	verRows := make([]versionRow, 0, len(versions))
	for _, v := range versions {
		verRows = append(verRows, versionRow{
			Version: v.Version,
			Created: time.Unix(v.CreatedUnix, 0).UTC().Format(time.RFC3339),
			URL:     fmt.Sprintf("/t/%s/p/%s/%s?v=%s", tenant.Name, pkg.Type, pkg.Name, v.Version),
		})
	}

	var selected *models.Version
	if wantVersion != "" {
		for _, v := range versions {
			if strings.EqualFold(v.Version, wantVersion) {
				selected = v
				break
			}
		}
	} else if len(versions) > 0 {
		selected = versions[0]
	}

	var fileRows []fileRow
	var goMod string
	if selected != nil {
		files, err := h.Models.ListFilesByVersion(ctx, selected.ID)
		if err != nil {
			c.String(http.StatusInternalServerError, "%v", err)
			return
		}
		for _, f := range files {
			size, sha, _ := h.fetchBlobMeta(c, f.BlobID)
			fileRows = append(fileRows, fileRow{Name: f.Name, Size: size, SHA: sha})
		}
		if v, ok, err := h.Models.GetProperty(ctx, models.PropertyRefVersion, selected.ID, "go.mod"); err == nil && ok {
			goMod = v
		}
	}

	c.HTML(http.StatusOK, "package.html", gin.H{
		"Title":    pkg.Name + " — pkgmirror",
		"Tenant":   tenant,
		"Package":  pkg,
		"Versions": verRows,
		"Selected": selected,
		"Files":    fileRows,
		"GoMod":    goMod,
	})
}

func (h *Handler) handleTenantError(c *gin.Context, err error) {
	if errors.Is(err, tenants.ErrNotExist) {
		c.HTML(http.StatusNotFound, "not_found.html", gin.H{
			"Title":   "Tenant not found",
			"Message": "No such tenant.",
		})
		return
	}
	c.String(http.StatusInternalServerError, "%v", err)
}

func (h *Handler) fetchBlobMeta(c *gin.Context, blobID int64) (int64, string, error) {
	row := h.Models.DB.QueryRowContext(c.Request.Context(),
		`SELECT size, hash_sha256 FROM package_blobs WHERE id = ?`, blobID)
	var (
		size int64
		sha  string
	)
	if err := row.Scan(&size, &sha); err != nil {
		return 0, "", err
	}
	return size, sha, nil
}

func visibilityLabel(v tenants.Visibility) string {
	if v == tenants.VisibilityPublic {
		return "public"
	}
	return "private"
}
