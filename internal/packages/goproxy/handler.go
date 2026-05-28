// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2023 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// The HTTP endpoint shape (list / info / mod / zip / @latest), the
// {Version, Time} info response struct, the resolve() pattern with
// "latest" special-casing, and the upload flow are modeled on
// forgejo/routers/api/packages/goproxy/goproxy.go from the Forgejo project,
// which is itself MIT licensed.

package goproxy

import (
	"context"
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

// Handler implements the HTTP endpoints of the Go module proxy protocol plus
// a non-standard upload endpoint for populating the mirror.
type Handler struct {
	Service *pkgsvc.Service
	Models  *models.Store
	Tenants *tenants.Store
	Engine  policy.Engine
}

// NewHandler constructs a Handler. If eng is nil, the no-op engine
// (allow-everything) is used so the rest of the system stays functional.
func NewHandler(svc *pkgsvc.Service, m *models.Store, ts *tenants.Store, eng policy.Engine) *Handler {
	if eng == nil {
		eng = policy.NoopEngine{}
	}
	return &Handler{Service: svc, Models: m, Tenants: ts, Engine: eng}
}

// Register attaches the Go proxy routes to the given group. The group is
// expected to be already scoped to /api/packages/:tenant/go.
//
//	GET  /<module>/@v/list
//	GET  /<module>/@v/<version>.info
//	GET  /<module>/@v/<version>.mod
//	GET  /<module>/@v/<version>.zip
//	GET  /<module>/@latest
//	PUT  /upload                (non-standard, mirror population)
func (h *Handler) Register(g *gin.RouterGroup) {
	g.PUT("/upload", h.upload)
	g.GET("/*path", h.proxy)
}

// tenantFromPath resolves the :tenant gin parameter to a tenant row, writing
// a 404 to the response on failure.
func (h *Handler) tenantFromPath(c *gin.Context) *tenants.Tenant {
	name := c.Param("tenant")
	if name == "" {
		c.String(http.StatusNotFound, "tenant required")
		return nil
	}
	t, err := h.Tenants.GetByName(c.Request.Context(), name)
	if err != nil {
		if errors.Is(err, tenants.ErrNotExist) {
			c.String(http.StatusNotFound, "tenant %q not found", name)
		} else {
			c.String(http.StatusInternalServerError, "lookup tenant: %v", err)
		}
		return nil
	}
	return t
}

// proxy dispatches a single GET into one of the protocol operations.
func (h *Handler) proxy(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}

	raw := strings.TrimPrefix(c.Param("path"), "/")
	if raw == "" {
		c.String(http.StatusNotFound, "not found")
		return
	}

	if i := strings.LastIndex(raw, "/@latest"); i != -1 && i+len("/@latest") == len(raw) {
		h.latest(c, tenant, raw[:i])
		return
	}

	atV := "/@v/"
	i := strings.LastIndex(raw, atV)
	if i == -1 {
		c.String(http.StatusNotFound, "not found")
		return
	}
	module := raw[:i]
	rest := raw[i+len(atV):]
	if module == "" || rest == "" {
		c.String(http.StatusNotFound, "not found")
		return
	}

	switch {
	case rest == "list":
		h.list(c, tenant, module)
	case strings.HasSuffix(rest, ".info"):
		h.info(c, tenant, module, strings.TrimSuffix(rest, ".info"))
	case strings.HasSuffix(rest, ".mod"):
		h.mod(c, tenant, module, strings.TrimSuffix(rest, ".mod"))
	case strings.HasSuffix(rest, ".zip"):
		h.zip(c, tenant, module, strings.TrimSuffix(rest, ".zip"))
	default:
		c.String(http.StatusNotFound, "not found")
	}
}

func (h *Handler) list(c *gin.Context, tenant *tenants.Tenant, module string) {
	pkg, err := h.Models.GetPackage(c.Request.Context(), tenant.ID, models.TypeGo, module)
	if err != nil {
		h.notFoundOrError(c, err)
		return
	}
	versions, err := h.Models.ListVersions(c.Request.Context(), pkg.ID)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	visible := h.filterReadable(c, tenant, pkg, versions)
	sort.Slice(visible, func(i, j int) bool { return visible[i].CreatedUnix < visible[j].CreatedUnix })
	c.Header("Content-Type", "text/plain; charset=utf-8")
	for _, v := range visible {
		fmt.Fprintln(c.Writer, v.Version)
	}
}

func (h *Handler) info(c *gin.Context, tenant *tenants.Tenant, module, version string) {
	pkg, ver, err := h.resolve(c.Request.Context(), tenant.ID, module, version)
	if err != nil {
		h.notFoundOrError(c, err)
		return
	}
	if !h.checkRead(c, tenant, pkg, ver, "") {
		return
	}
	c.JSON(http.StatusOK, struct {
		Version string    `json:"Version"`
		Time    time.Time `json:"Time"`
	}{
		Version: ver.Version,
		Time:    time.Unix(ver.CreatedUnix, 0).UTC(),
	})
}

func (h *Handler) mod(c *gin.Context, tenant *tenants.Tenant, module, version string) {
	pkg, ver, err := h.resolve(c.Request.Context(), tenant.ID, module, version)
	if err != nil {
		h.notFoundOrError(c, err)
		return
	}
	if !h.checkRead(c, tenant, pkg, ver, "") {
		return
	}
	goMod, ok, err := h.Models.GetProperty(c.Request.Context(), models.PropertyRefVersion, ver.ID, PropertyGoMod)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	if !ok {
		c.String(http.StatusNotFound, "go.mod not found")
		return
	}
	c.Header("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(c.Writer, goMod)
}

func (h *Handler) zip(c *gin.Context, tenant *tenants.Tenant, module, version string) {
	pkg, ver, err := h.resolve(c.Request.Context(), tenant.ID, module, version)
	if err != nil {
		h.notFoundOrError(c, err)
		return
	}
	files, err := h.Models.ListFilesByVersion(c.Request.Context(), ver.ID)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	if len(files) == 0 {
		c.String(http.StatusNotFound, "no files")
		return
	}
	var f *models.File
	for _, candidate := range files {
		if candidate.IsLead {
			f = candidate
			break
		}
	}
	if f == nil {
		f = files[0]
	}
	if !h.checkRead(c, tenant, pkg, ver, f.Name) {
		return
	}
	rc, _, err := h.Service.OpenFile(c.Request.Context(), f)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	defer rc.Close()
	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, f.Name))
	_, _ = io.Copy(c.Writer, rc)
}

func (h *Handler) latest(c *gin.Context, tenant *tenants.Tenant, module string) {
	pkg, err := h.Models.GetPackage(c.Request.Context(), tenant.ID, models.TypeGo, module)
	if err != nil {
		h.notFoundOrError(c, err)
		return
	}
	versions, err := h.Models.ListVersions(c.Request.Context(), pkg.ID)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	visible := h.filterReadable(c, tenant, pkg, versions)
	if len(visible) == 0 {
		c.String(http.StatusNotFound, "no readable version")
		return
	}
	// Pick the most-recent readable version.
	var ver *models.Version
	for _, v := range visible {
		if ver == nil || v.CreatedUnix > ver.CreatedUnix {
			ver = v
		}
	}
	c.JSON(http.StatusOK, struct {
		Version string    `json:"Version"`
		Time    time.Time `json:"Time"`
	}{
		Version: ver.Version,
		Time:    time.Unix(ver.CreatedUnix, 0).UTC(),
	})
}

func (h *Handler) upload(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireWrite(c, tenant) {
		return
	}

	buf, err := pkgsvc.NewHashedBufferFromReader(c.Request.Body)
	if err != nil {
		c.String(http.StatusInternalServerError, "buffer upload: %v", err)
		return
	}
	defer buf.Close()

	pkg, err := Parse(buf, buf.Size())
	if err != nil {
		if errors.Is(err, ErrInvalidStructure) || errors.Is(err, ErrGoModFileTooLarge) {
			c.String(http.StatusBadRequest, "%v", err)
			return
		}
		c.String(http.StatusBadRequest, "parse zip: %v", err)
		return
	}

	filename := fmt.Sprintf("%s.zip", pkg.Version)
	if !h.checkIngest(c, tenant, pkg.Name, pkg.Version, filename) {
		return
	}

	_, _, _, err = h.Service.CreatePackageAndAddFile(c.Request.Context(), pkgsvc.CreationInfo{
		TenantID:    tenant.ID,
		PackageType: models.TypeGo,
		PackageName: pkg.Name,
		Version:     pkg.Version,
		VersionProperties: map[string]string{
			PropertyGoMod: pkg.GoMod,
		},
		Filename: filename,
		IsLead:   true,
	}, buf)
	if err != nil {
		if errors.Is(err, models.ErrDuplicatePackageVersion) {
			c.String(http.StatusConflict, "version already exists")
			return
		}
		c.String(http.StatusInternalServerError, "ingest: %v", err)
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"tenant":  tenant.Name,
		"module":  pkg.Name,
		"version": pkg.Version,
	})
}

func (h *Handler) resolve(ctx context.Context, tenantID int64, module, version string) (*models.Package, *models.Version, error) {
	pkg, err := h.Models.GetPackage(ctx, tenantID, models.TypeGo, module)
	if err != nil {
		return nil, nil, err
	}
	if version == "latest" {
		ver, err := h.Models.GetLatestVersion(ctx, pkg.ID)
		if err != nil {
			return pkg, nil, err
		}
		return pkg, ver, nil
	}
	ver, err := h.Models.GetVersion(ctx, pkg.ID, version)
	if err != nil {
		return pkg, nil, err
	}
	return pkg, ver, nil
}

func (h *Handler) notFoundOrError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, models.ErrPackageNotExist),
		errors.Is(err, models.ErrVersionNotExist),
		errors.Is(err, models.ErrFileNotExist):
		c.String(http.StatusNotFound, "%v", err)
	default:
		c.String(http.StatusInternalServerError, "%v", err)
	}
}

// --- policy hooks ---

// subjectFor builds a policy Subject for a (package, version, filename) triple.
func (h *Handler) subjectFor(tenant *tenants.Tenant, pkg *models.Package, ver *models.Version, filename string) policy.Subject {
	s := policy.Subject{
		TenantID: tenant.ID,
		Format:   string(models.TypeGo),
		Package:  pkg.LowerName,
		Filename: filename,
	}
	if ver != nil {
		s.Version = ver.Version
		s.Attrs = map[string]any{
			"created_unix":       ver.CreatedUnix,
			"ingest_age_seconds": time.Now().Unix() - ver.CreatedUnix,
		}
	}
	return s
}

// checkRead applies the policy engine to a single-version read. Returns
// false (and writes a 403) if the engine returns Quarantine or Deny.
func (h *Handler) checkRead(c *gin.Context, tenant *tenants.Tenant, pkg *models.Package, ver *models.Version, filename string) bool {
	r := h.Engine.Evaluate(c.Request.Context(), h.subjectFor(tenant, pkg, ver, filename), policy.ActionRead)
	if r.IsBlocked() {
		c.String(http.StatusForbidden, "%s", policyReason(r))
		return false
	}
	return true
}

// checkIngest applies the policy engine before storing an uploaded artifact.
// Returns false (and writes a 403) on Deny. Quarantine handling is
// deferred until storage-side support lands (see plan §11 step 4).
func (h *Handler) checkIngest(c *gin.Context, tenant *tenants.Tenant, packageName, version, filename string) bool {
	subj := policy.Subject{
		TenantID: tenant.ID,
		Format:   string(models.TypeGo),
		Package:  strings.ToLower(packageName),
		Version:  version,
		Filename: filename,
		Attrs: map[string]any{
			"created_unix":       time.Now().Unix(),
			"ingest_age_seconds": int64(0),
		},
	}
	r := h.Engine.Evaluate(c.Request.Context(), subj, policy.ActionIngest)
	if r.Decision >= policy.Deny {
		c.String(http.StatusForbidden, "%s", policyReason(r))
		return false
	}
	return true
}

// filterReadable returns the subset of versions the engine permits for
// reading. Quarantined / denied versions are silently omitted.
func (h *Handler) filterReadable(c *gin.Context, tenant *tenants.Tenant, pkg *models.Package, versions []*models.Version) []*models.Version {
	out := versions[:0]
	for _, v := range versions {
		r := h.Engine.Evaluate(c.Request.Context(), h.subjectFor(tenant, pkg, v, ""), policy.ActionRead)
		if r.IsBlocked() {
			continue
		}
		out = append(out, v)
	}
	return out
}

func policyReason(r policy.Result) string {
	if r.Reason == "" {
		return r.Decision.String()
	}
	return r.Reason
}

