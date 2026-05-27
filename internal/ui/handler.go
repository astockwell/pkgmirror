// Package ui provides server-rendered Bootstrap-based pages for browsing
// packages stored in the mirror.
package ui

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/astockwell/pkgmirror/internal/models"
	pkgsvc "github.com/astockwell/pkgmirror/internal/packages"

	"github.com/gin-gonic/gin"
)

// Handler renders the HTML UI.
type Handler struct {
	Service *pkgsvc.Service
	Models  *models.Store
}

// New constructs a UI handler.
func New(svc *pkgsvc.Service, m *models.Store) *Handler {
	return &Handler{Service: svc, Models: m}
}

// Register attaches UI routes to the engine.
func (h *Handler) Register(r *gin.Engine) {
	r.GET("/", h.index)
	r.GET("/-/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	// /p/<type>/*name — package detail
	r.GET("/p/:type/*name", h.packageOrVersion)
}

type packageRow struct {
	Type     string
	Name     string
	URL      string
	Created  string
	VerCount int
}

func (h *Handler) index(c *gin.Context) {
	pkgs, err := h.Models.ListPackages(c.Request.Context(), "")
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	rows := make([]packageRow, 0, len(pkgs))
	for _, p := range pkgs {
		vs, err := h.Models.ListVersions(c.Request.Context(), p.ID)
		if err != nil {
			c.String(http.StatusInternalServerError, "%v", err)
			return
		}
		rows = append(rows, packageRow{
			Type:     string(p.Type),
			Name:     p.Name,
			URL:      "/p/" + string(p.Type) + "/" + p.Name,
			Created:  time.Unix(p.CreatedUnix, 0).UTC().Format(time.RFC3339),
			VerCount: len(vs),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Type != rows[j].Type {
			return rows[i].Type < rows[j].Type
		}
		return rows[i].Name < rows[j].Name
	})
	c.HTML(http.StatusOK, "index.html", gin.H{
		"Title":    "pkgmirror — packages",
		"Packages": rows,
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
	typ := models.Type(c.Param("type"))
	name := strings.TrimPrefix(c.Param("name"), "/")

	// Optional version selector via query string for simplicity: /p/<type>/<name>?v=<version>
	wantVersion := c.Query("v")

	pkg, err := h.Models.GetPackage(c.Request.Context(), typ, name)
	if err != nil {
		if errors.Is(err, models.ErrPackageNotExist) {
			c.HTML(http.StatusNotFound, "not_found.html", gin.H{
				"Title":   "Package not found",
				"Message": "No package " + string(typ) + "/" + name,
			})
			return
		}
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	versions, err := h.Models.ListVersions(c.Request.Context(), pkg.ID)
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
			URL:     "/p/" + string(pkg.Type) + "/" + pkg.Name + "?v=" + v.Version,
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
		files, err := h.Models.ListFilesByVersion(c.Request.Context(), selected.ID)
		if err != nil {
			c.String(http.StatusInternalServerError, "%v", err)
			return
		}
		for _, f := range files {
			size, sha, _ := h.fetchBlobMeta(c, f.BlobID)
			fileRows = append(fileRows, fileRow{Name: f.Name, Size: size, SHA: sha})
		}
		if v, ok, err := h.Models.GetProperty(c.Request.Context(), models.PropertyRefVersion, selected.ID, "go.mod"); err == nil && ok {
			goMod = v
		}
	}

	c.HTML(http.StatusOK, "package.html", gin.H{
		"Title":    pkg.Name + " — pkgmirror",
		"Package":  pkg,
		"Versions": verRows,
		"Selected": selected,
		"Files":    fileRows,
		"GoMod":    goMod,
	})
}

// fetchBlobMeta returns (size, sha256_hex) for a blob ID. Inline helper to
// keep the UI handler self-contained.
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
