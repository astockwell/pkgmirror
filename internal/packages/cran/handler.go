// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2023 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// HTTP routes for the CRAN (R) registry. The URL shape
// (`src/contrib/PACKAGES[.gz]`, `src/contrib/<file>.tar.gz`,
// `bin/<platform>/contrib/<rversion>/PACKAGES[.gz]`, and the
// `PUT /src` + `PUT /bin?platform=X&rversion=Y` upload convention)
// is modeled on forgejo/routers/api/packages/cran/cran.go (MIT).
//
// Two deviations:
//
//  1. The PACKAGES index is built on demand from the live file list
//     rather than persisted as a file row. Same posture as the
//     other formats. See docs/adding-a-format.md "On-demand vs
//     cached index generation".
//  2. The composite (cran.type, cran.platform, cran.rvserion) per
//     file is encoded into the file row's Name as
//     `<type>|<platform>|<rversion>|<basename>` (same pattern
//     Alpine + Debian + RPM use) so UNIQUE(version_id, name) holds
//     across multi-platform publishes of the same version without
//     a schema change.

package cran

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/astockwell/pkgmirror/internal/auth"
	"github.com/astockwell/pkgmirror/internal/models"
	pkgsvc "github.com/astockwell/pkgmirror/internal/packages"
	"github.com/astockwell/pkgmirror/internal/policy"
	"github.com/astockwell/pkgmirror/internal/tenants"

	"github.com/gin-gonic/gin"
)

// Content types CRAN clients expect.
const (
	contentTypeTarGz    = "application/x-gzip"
	contentTypePackages = "text/plain; charset=utf-8"
)

// Handler is the CRAN registry HTTP handler.
type Handler struct {
	Service *pkgsvc.Service
	Models  *models.Store
	Tenants *tenants.Store
	Engine  policy.Engine
}

// NewHandler constructs a Handler. If eng is nil the no-op engine
// is used.
func NewHandler(svc *pkgsvc.Service, m *models.Store, ts *tenants.Store, eng policy.Engine) *Handler {
	if eng == nil {
		eng = policy.NoopEngine{}
	}
	return &Handler{Service: svc, Models: m, Tenants: ts, Engine: eng}
}

// Register mounts CRAN routes on g (scoped to
// /api/packages/:tenant/cran).
//
//	PUT    /src                                                 upload source .tar.gz
//	GET    /src/contrib/PACKAGES                                source index (plain)
//	GET    /src/contrib/PACKAGES.gz                             source index (gzip)
//	GET    /src/contrib/:filename                               download source
//	GET    /src/contrib/Archive/:packagename/:filename          download archived source
//	PUT    /bin?platform=X&rversion=Y                           upload binary
//	GET    /bin/:platform/contrib/:rversion/PACKAGES            binary index (plain)
//	GET    /bin/:platform/contrib/:rversion/PACKAGES.gz         binary index (gzip)
//	GET    /bin/:platform/contrib/:rversion/:filename           download binary
//	DELETE /:name/:version                                      hard-delete a version
//
// The `/src/contrib/*tail` and `/bin/:platform/contrib/:rversion/*tail`
// routes use catch-alls because gin can't mix the literal
// `PACKAGES` / `PACKAGES.gz` segments with the `:filename` parameter
// at the same path level (same workaround we use for NuGet).
func (h *Handler) Register(g *gin.RouterGroup) {
	g.PUT("/src", h.uploadSource)
	g.GET("/src/contrib/*tail", h.serveSource)
	g.HEAD("/src/contrib/*tail", h.serveSource)

	g.PUT("/bin", h.uploadBinary)
	g.GET("/bin/:platform/contrib/:rversion/*tail", h.serveBinary)
	g.HEAD("/bin/:platform/contrib/:rversion/*tail", h.serveBinary)

	g.DELETE("/:name/:version", h.delete)
}

// --- helpers ---------------------------------------------------------------

func (h *Handler) tenantFromPath(c *gin.Context) *tenants.Tenant {
	name := c.Param("tenant")
	t, err := h.Tenants.GetByName(c.Request.Context(), name)
	if err != nil {
		if errors.Is(err, tenants.ErrNotExist) {
			c.String(http.StatusNotFound, "tenant %q not found", name)
		} else {
			c.String(http.StatusInternalServerError, "%v", err)
		}
		return nil
	}
	return t
}

// storedFileName encodes the (cran.type, platform, rversion,
// basename) tuple into the file row's Name so multi-platform
// publishes of the same version can coexist under
// UNIQUE(version_id, name). Inverse: parseStoredFileName.
func storedFileName(cranType, platform, rversion, basename string) string {
	return cranType + "|" + platform + "|" + rversion + "|" + basename
}

func parseStoredFileName(s string) (cranType, platform, rversion, basename string, ok bool) {
	parts := strings.SplitN(s, "|", 4)
	if len(parts) != 4 {
		return "", "", "", "", false
	}
	return parts[0], parts[1], parts[2], parts[3], true
}

func sourceBasename(name, version string) string {
	return name + "_" + version + ".tar.gz"
}

func binaryBasename(name, version, ext string) string {
	return name + "_" + version + ext
}

// splitTail trims gin's leading `/` and splits on `/`. Used by the
// two catch-all routes to dispatch on tail shape.
func splitTail(s string) []string {
	s = strings.TrimPrefix(s, "/")
	if s == "" {
		return nil
	}
	return strings.Split(s, "/")
}

// openUploadBody returns a reader over the uploaded archive bytes
// regardless of whether the client used multipart/form-data (e.g.
// scripted browser uploads, devtools::release) or raw octet-stream
// (curl, Hadley's drat-style scripts). For multipart, the first
// file field wins. Same shape as the NuGet handler's helper.
func openUploadBody(c *gin.Context) (io.Reader, func(), error) {
	ct := c.GetHeader("Content-Type")
	if !strings.HasPrefix(strings.ToLower(ct), "multipart/form-data") {
		return c.Request.Body, nil, nil
	}
	if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
		return nil, nil, fmt.Errorf("parse multipart: %w", err)
	}
	if c.Request.MultipartForm == nil || c.Request.MultipartForm.File == nil {
		return nil, nil, fmt.Errorf("multipart upload missing file part")
	}
	for _, headers := range c.Request.MultipartForm.File {
		if len(headers) == 0 {
			continue
		}
		f, err := headers[0].Open()
		if err != nil {
			return nil, nil, fmt.Errorf("open part: %w", err)
		}
		return f, func() { _ = f.Close() }, nil
	}
	return nil, nil, fmt.Errorf("multipart upload missing file part")
}

// --- upload ----------------------------------------------------------------

func (h *Handler) uploadSource(c *gin.Context) {
	h.ingest(c, "", "")
}

func (h *Handler) uploadBinary(c *gin.Context) {
	platform := strings.TrimSpace(c.Query("platform"))
	rversion := strings.TrimSpace(c.Query("rversion"))
	if platform == "" || rversion == "" {
		c.String(http.StatusBadRequest, "platform and rversion query params are required")
		return
	}
	h.ingest(c, platform, rversion)
}

// ingest is the common upload path. platform + rversion are empty
// for source uploads and required for binary uploads.
func (h *Handler) ingest(c *gin.Context, platform, rversion string) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireWrite(c, tenant) {
		return
	}

	body, closer, err := openUploadBody(c)
	if err != nil {
		c.String(http.StatusBadRequest, "%v", err)
		return
	}
	if closer != nil {
		defer closer()
	}

	buf, err := h.Service.NewHashedBuffer(body)
	if err != nil {
		c.String(http.StatusInternalServerError, "buffer: %v", err)
		return
	}
	defer buf.Close()

	// NewHashedBuffer drains the body to compute hashes; the
	// underlying file is positioned at the end. Rewind before
	// handing to gzip.NewReader inside ParsePackage.
	if _, err := buf.Seek(0, io.SeekStart); err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	pkg, err := ParsePackage(buf, buf.Size())
	if err != nil {
		switch {
		case errors.Is(err, ErrMissingDescriptionFile),
			errors.Is(err, ErrInvalidName),
			errors.Is(err, ErrInvalidVersion):
			c.String(http.StatusBadRequest, "%v", err)
		default:
			c.String(http.StatusBadRequest, "parse CRAN archive: %v", err)
		}
		return
	}
	if _, err := buf.Seek(0, io.SeekStart); err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}

	cranType := TypeSource
	basename := sourceBasename(pkg.Name, pkg.Version)
	if platform != "" {
		cranType = TypeBinary
		basename = binaryBasename(pkg.Name, pkg.Version, pkg.FileExtension)
	}

	if !h.checkIngest(c, tenant, pkg.Name, pkg.Version, basename, pkg.Metadata.License) {
		return
	}

	metaJSON, _ := json.Marshal(pkg.Metadata)
	storedName := storedFileName(cranType, platform, rversion, basename)

	_, ver, file, err := h.Service.CreatePackageOrAddFileToExisting(
		c.Request.Context(),
		pkgsvc.CreationInfo{
			TenantID:            tenant.ID,
			PackageType:         models.TypeCRAN,
			PackageName:         pkg.Name,
			PackageLookupName:   strings.ToLower(pkg.Name),
			Version:             pkg.Version,
			VersionMetadataJSON: string(metaJSON),
			Filename:            storedName,
			IsLead:              true,
		}, buf)
	if err != nil {
		if errors.Is(err, models.ErrDuplicatePackageFile) {
			c.String(http.StatusConflict, "%s already exists", basename)
			return
		}
		c.String(http.StatusInternalServerError, "ingest: %v", err)
		return
	}

	if pkg.Metadata.License != "" {
		_ = h.Models.SetLicense(c.Request.Context(), ver.ID, pkg.Metadata.License)
	}

	ctx := c.Request.Context()
	props := map[string]string{PropertyType: cranType}
	if cranType == TypeBinary {
		props[PropertyPlatform] = platform
		props[PropertyRVersion] = rversion
	}
	for k, v := range props {
		if err := h.Models.SetProperty(ctx, models.PropertyRefFile, file.ID, k, v); err != nil {
			c.String(http.StatusInternalServerError, "%v", err)
			return
		}
	}

	c.Status(http.StatusCreated)
}

// --- serve (read) ----------------------------------------------------------

func (h *Handler) serveSource(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}
	parts := splitTail(c.Param("tail"))
	switch {
	case len(parts) == 1 && parts[0] == "PACKAGES":
		h.servePackagesIndex(c, tenant, TypeSource, "", "", false)
	case len(parts) == 1 && parts[0] == "PACKAGES.gz":
		h.servePackagesIndex(c, tenant, TypeSource, "", "", true)
	case len(parts) == 1:
		h.serveFile(c, tenant, TypeSource, "", "", parts[0])
	case len(parts) == 3 && parts[0] == "Archive":
		// /Archive/<packagename>/<filename> — we don't currently
		// distinguish "archived" from "current" since pkgmirror keeps
		// every uploaded version. <packagename> is informational; the
		// filename is unique enough on its own.
		h.serveFile(c, tenant, TypeSource, "", "", parts[2])
	default:
		c.String(http.StatusNotFound, "unknown src path")
	}
}

func (h *Handler) serveBinary(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}
	platform := c.Param("platform")
	rversion := c.Param("rversion")
	parts := splitTail(c.Param("tail"))
	switch {
	case len(parts) == 1 && parts[0] == "PACKAGES":
		h.servePackagesIndex(c, tenant, TypeBinary, platform, rversion, false)
	case len(parts) == 1 && parts[0] == "PACKAGES.gz":
		h.servePackagesIndex(c, tenant, TypeBinary, platform, rversion, true)
	case len(parts) == 1:
		h.serveFile(c, tenant, TypeBinary, platform, rversion, parts[0])
	default:
		c.String(http.StatusNotFound, "unknown bin path")
	}
}

func (h *Handler) servePackagesIndex(c *gin.Context, tenant *tenants.Tenant, cranType, platform, rversion string, asGzip bool) {
	entries, err := h.loadLatestEntries(c.Request.Context(), tenant.ID, cranType, platform, rversion)
	if err != nil {
		c.String(http.StatusInternalServerError, "load entries: %v", err)
		return
	}
	body := buildPackagesIndex(entries)
	if asGzip {
		body = gzipBytes(body)
		c.Header("Content-Type", contentTypeTarGz)
	} else {
		c.Header("Content-Type", contentTypePackages)
	}
	c.Header("Content-Length", fmt.Sprintf("%d", len(body)))
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	_, _ = c.Writer.Write(body)
}

func (h *Handler) serveFile(c *gin.Context, tenant *tenants.Tenant, cranType, platform, rversion, basename string) {
	storedName := storedFileName(cranType, platform, rversion, basename)
	pkg, ver, file, err := h.lookupFile(c.Request.Context(), tenant.ID, storedName)
	if err != nil {
		c.String(http.StatusNotFound, "%v", err)
		return
	}
	if !h.checkRead(c, tenant, pkg, ver, basename) {
		return
	}
	rc, blob, err := h.Service.OpenFile(c.Request.Context(), file)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	defer rc.Close()
	c.Header("Content-Type", contentTypeTarGz)
	c.Header("Content-Length", fmt.Sprintf("%d", blob.Size))
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, basename))
	c.Header("Last-Modified", time.Unix(file.CreatedUnix, 0).UTC().Format(http.TimeFormat))
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	_, _ = io.Copy(c.Writer, rc)
}

// --- delete ----------------------------------------------------------------

func (h *Handler) delete(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireWrite(c, tenant) {
		return
	}
	name := c.Param("name")
	version := c.Param("version")

	ctx := c.Request.Context()
	row := h.Models.DB.QueryRowContext(ctx, `
		SELECT v.id
		FROM package_versions v
		JOIN packages p ON p.id = v.package_id
		WHERE p.tenant_id = ? AND p.type = 'cran' AND p.lower_name = ? AND v.version = ?
	`, tenant.ID, strings.ToLower(name), version)
	var versionID int64
	if err := row.Scan(&versionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.String(http.StatusNotFound, "%s %s not found", name, version)
			return
		}
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	if err := h.Models.DeleteVersion(ctx, versionID); err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	c.Status(http.StatusNoContent)
}

// --- DB lookups ------------------------------------------------------------

// indexEntry is what the PACKAGES builder needs per file.
type indexEntry struct {
	Name     string
	Version  string
	Metadata *Metadata
	MD5      string
}

// loadLatestEntries returns one entry per package — the newest
// version among files matching (tenantID, cranType, platform,
// rversion). The "latest" picked here is `max(created_unix)` per
// package; that matches install.packages's behavior of preferring
// the most recently uploaded version when versions are not strictly
// ordered. Drains the cursor before per-row metadata unmarshaling
// to dodge the SQLITE_BUSY pattern.
func (h *Handler) loadLatestEntries(ctx context.Context, tenantID int64, cranType, platform, rversion string) ([]*indexEntry, error) {
	prefix := cranType + "|" + platform + "|" + rversion + "|"
	rows, err := h.Models.DB.QueryContext(ctx, `
		SELECT p.name, v.id, v.version, v.metadata_json, v.created_unix,
		       b.hash_md5, f.name, f.created_unix
		FROM package_files f
		JOIN package_versions v ON v.id = f.version_id
		JOIN packages p ON p.id = v.package_id
		JOIN package_blobs b ON b.id = f.blob_id
		WHERE p.tenant_id = ? AND p.type = 'cran'
		  AND v.quarantine_reason IS NULL
		  AND f.name LIKE ?
		ORDER BY p.lower_name, v.created_unix DESC
	`, tenantID, prefix+"%")
	if err != nil {
		return nil, err
	}
	type row struct {
		name, ver, metaJSON, md5 string
		created                  int64
	}
	var rs []row
	for rows.Next() {
		var (
			name, ver, metaJSON, md5, fname string
			vcreated, fcreated              int64
			vid                             int64
		)
		if err := rows.Scan(&name, &vid, &ver, &metaJSON, &vcreated, &md5, &fname, &fcreated); err != nil {
			rows.Close()
			return nil, err
		}
		rs = append(rs, row{name: name, ver: ver, metaJSON: metaJSON, md5: md5, created: vcreated})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	// Reduce to the newest-per-package. The ORDER BY ensures the
	// first row per package_lower_name is the newest by
	// created_unix; iterate and keep the first.
	seen := map[string]struct{}{}
	out := make([]*indexEntry, 0, len(rs))
	for _, r := range rs {
		key := strings.ToLower(r.name)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		m := &Metadata{}
		if r.metaJSON != "" {
			_ = json.Unmarshal([]byte(r.metaJSON), m)
		}
		out = append(out, &indexEntry{Name: r.name, Version: r.ver, Metadata: m, MD5: r.md5})
	}
	// Sort by package name for stable index output.
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

func (h *Handler) lookupFile(ctx context.Context, tenantID int64, storedName string) (*models.Package, *models.Version, *models.File, error) {
	row := h.Models.DB.QueryRowContext(ctx, `
		SELECT f.id, f.version_id, f.blob_id, f.name, f.is_lead, f.created_unix,
		       v.id, v.package_id, v.version, v.metadata_json, v.created_unix,
		       v.license, v.quarantine_reason, v.quarantined_by_rule_id,
		       p.id, p.tenant_id, p.type, p.name, p.lower_name, p.created_unix
		FROM package_files f
		JOIN package_versions v ON v.id = f.version_id
		JOIN packages p ON p.id = v.package_id
		WHERE p.tenant_id = ? AND p.type = 'cran' AND f.name = ?
	`, tenantID, storedName)

	var (
		f models.File
		v models.Version
		p models.Package
	)
	if err := row.Scan(
		&f.ID, &f.VersionID, &f.BlobID, &f.Name, &f.IsLead, &f.CreatedUnix,
		&v.ID, &v.PackageID, &v.Version, &v.MetadataJSON, &v.CreatedUnix,
		&v.License, &v.QuarantineReason, &v.QuarantinedByRuleID,
		&p.ID, &p.TenantID, &p.Type, &p.Name, &p.LowerName, &p.CreatedUnix,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, nil, fmt.Errorf("file %q not found", storedName)
		}
		return nil, nil, nil, err
	}
	return &p, &v, &f, nil
}

// --- policy hooks ----------------------------------------------------------

func (h *Handler) subjectFor(tenant *tenants.Tenant, pkg *models.Package, ver *models.Version, filename string) policy.Subject {
	s := policy.Subject{
		TenantID: tenant.ID,
		Format:   string(models.TypeCRAN),
		Package:  pkg.LowerName,
		Filename: filename,
	}
	if ver != nil {
		s.Version = ver.Version
		s.Attrs = map[string]any{
			"created_unix":       ver.CreatedUnix,
			"ingest_age_seconds": time.Now().Unix() - ver.CreatedUnix,
		}
		if ver.License.Valid && ver.License.String != "" {
			s.Attrs["license"] = ver.License.String
		}
	}
	return s
}

func (h *Handler) checkRead(c *gin.Context, tenant *tenants.Tenant, pkg *models.Package, ver *models.Version, filename string) bool {
	r := h.Engine.Evaluate(c.Request.Context(), h.subjectFor(tenant, pkg, ver, filename), policy.ActionRead)
	if r.IsBlocked() {
		c.String(http.StatusForbidden, "%s", policyReason(r))
		return false
	}
	return true
}

func (h *Handler) checkIngest(c *gin.Context, tenant *tenants.Tenant, name, version, filename, license string) bool {
	subj := policy.Subject{
		TenantID: tenant.ID,
		Format:   string(models.TypeCRAN),
		Package:  strings.ToLower(name),
		Version:  version,
		Filename: filename,
		Attrs: map[string]any{
			"created_unix":       time.Now().Unix(),
			"ingest_age_seconds": int64(0),
		},
	}
	if license != "" {
		subj.Attrs["license"] = license
	}
	r := h.Engine.Evaluate(c.Request.Context(), subj, policy.ActionIngest)
	if r.Decision >= policy.Deny {
		c.String(http.StatusForbidden, "%s", policyReason(r))
		return false
	}
	return true
}

func policyReason(r policy.Result) string {
	if r.Reason == "" {
		return r.Decision.String()
	}
	return r.Reason
}
