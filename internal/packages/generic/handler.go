// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2021 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// The HTTP endpoint shape (PUT/GET/DELETE under
// /:packagename/:packageversion/:filename), the name and filename
// validation regexes, and the "delete file; if last file, delete
// version" cascade behavior are modeled on
// forgejo/routers/api/packages/generic/generic.go (MIT). The
// implementation is rewritten on top of pkgmirror's service / models /
// storage layers; the spec compliance bits (status codes, allowed name
// characters, trim semantics on version) are byte-faithful to upstream.

// Package generic implements the "generic" package format: a
// pass-through registry that stores arbitrary blobs by
// (tenant, name, version, filename) without any ecosystem-specific
// metadata parsing.
package generic

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/astockwell/pkgmirror/internal/auth"
	"github.com/astockwell/pkgmirror/internal/models"
	pkgsvc "github.com/astockwell/pkgmirror/internal/packages"
	"github.com/astockwell/pkgmirror/internal/policy"
	"github.com/astockwell/pkgmirror/internal/tenants"

	"github.com/gin-gonic/gin"
)

// Validation regexes ported verbatim from
// forgejo/routers/api/packages/generic/generic.go.
var (
	packageNameRegex = regexp.MustCompile(`\A[-_+.\w]+\z`)
	filenameRegex    = regexp.MustCompile(`\A[-_+=:;.()\[\]{}~!@#$%^& \w]+\z`)
)

// Sentinel error messages mirror upstream's plain-text body. Tests look
// for the substring so changing these is a breaking change.
const (
	errInvalidPackageName    = "invalid package name"
	errInvalidPackageVersion = "invalid package version"
	errInvalidFilename       = "invalid filename"
)

// isValidPackageName reports whether name is acceptable. A single-
// character name must be alphanumeric; longer names match the regex and
// must not be the literal "..".
func isValidPackageName(name string) bool {
	if len(name) == 0 {
		return false
	}
	if len(name) == 1 && !unicode.IsLetter(rune(name[0])) && !unicode.IsNumber(rune(name[0])) {
		return false
	}
	return packageNameRegex.MatchString(name) && name != ".."
}

// isValidFilename reports whether filename is acceptable. Trailing/
// leading whitespace and the special names "." / ".." are rejected
// even when they would match the regex.
func isValidFilename(filename string) bool {
	return filenameRegex.MatchString(filename) &&
		strings.TrimSpace(filename) == filename &&
		filename != "." && filename != ".."
}

// Handler is the generic-format HTTP handler.
type Handler struct {
	Service *pkgsvc.Service
	Models  *models.Store
	Tenants *tenants.Store
	Engine  policy.Engine
}

// NewHandler constructs a Handler. eng=nil falls back to the no-op
// policy engine.
func NewHandler(svc *pkgsvc.Service, m *models.Store, ts *tenants.Store, eng policy.Engine) *Handler {
	if eng == nil {
		eng = policy.NoopEngine{}
	}
	return &Handler{Service: svc, Models: m, Tenants: ts, Engine: eng}
}

// Register mounts generic routes on g (expected to be scoped to
// /api/packages/:tenant/generic).
//
//	PUT    /:name/:version/:filename   upload (auth, 201, 409 on dup)
//	GET    /:name/:version/:filename   download
//	DELETE /:name/:version/:filename   delete a single file; if it was
//	                                   the last file in the version,
//	                                   the version is removed too
//	DELETE /:name/:version             delete the version + every file
func (h *Handler) Register(g *gin.RouterGroup) {
	g.PUT("/:name/:version/:filename", h.upload)
	g.GET("/:name/:version/:filename", h.download)
	g.DELETE("/:name/:version/:filename", h.deleteFile)
	g.DELETE("/:name/:version", h.deleteVersion)
}

// --- helpers ----------------------------------------------------------------

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

// readParams validates the three path params and returns them stripped.
// Returns ok=false after writing the appropriate 400 response.
func readParams(c *gin.Context, withFilename bool) (name, version, filename string, ok bool) {
	name = c.Param("name")
	version = c.Param("version")
	if withFilename {
		filename = c.Param("filename")
	}
	if !isValidPackageName(name) {
		c.String(http.StatusBadRequest, errInvalidPackageName)
		return "", "", "", false
	}
	if version == "" || version != strings.TrimSpace(version) {
		c.String(http.StatusBadRequest, errInvalidPackageVersion)
		return "", "", "", false
	}
	if withFilename && !isValidFilename(filename) {
		c.String(http.StatusBadRequest, errInvalidFilename)
		return "", "", "", false
	}
	return name, version, filename, true
}

// --- handlers ---------------------------------------------------------------

// upload implements PUT /:name/:version/:filename. The request body IS
// the blob — there's no multipart or JSON envelope, so the upload path
// is the simplest of any format. We funnel through
// CreatePackageOrAddFileToExisting so a generic "package" can carry
// many files at the same version (matches Forgejo).
func (h *Handler) upload(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireWrite(c, tenant) {
		return
	}
	name, version, filename, ok := readParams(c, true)
	if !ok {
		return
	}
	if !h.checkIngest(c, tenant, name, version, filename) {
		return
	}

	buf, err := pkgsvc.NewHashedBufferFromReader(c.Request.Body)
	if err != nil {
		c.String(http.StatusInternalServerError, "buffer: %v", err)
		return
	}
	defer buf.Close()

	_, _, _, err = h.Service.CreatePackageOrAddFileToExisting(c.Request.Context(), pkgsvc.CreationInfo{
		TenantID:          tenant.ID,
		PackageType:       models.TypeGeneric,
		PackageName:       name,
		PackageLookupName: strings.ToLower(name),
		Version:           version,
		Filename:          filename,
		IsLead:            true,
	}, buf)
	if err != nil {
		if errors.Is(err, models.ErrDuplicatePackageFile) {
			c.String(http.StatusConflict, "file %s already exists in %s@%s", filename, name, version)
			return
		}
		c.String(http.StatusInternalServerError, "ingest: %v", err)
		return
	}
	c.Status(http.StatusCreated)
}

// download implements GET /:name/:version/:filename.
func (h *Handler) download(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}
	name, version, filename, ok := readParams(c, true)
	if !ok {
		return
	}

	pkg, ver, file, err := h.lookup(c, tenant, name, version, filename)
	if err != nil {
		c.String(http.StatusNotFound, "%v", err)
		return
	}
	if !h.checkRead(c, tenant, pkg, ver, filename) {
		return
	}

	rc, blob, err := h.Service.OpenFile(c.Request.Context(), file)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	defer rc.Close()

	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, file.Name))
	if blob != nil {
		c.Header("Content-Length", fmt.Sprintf("%d", blob.Size))
	}
	_, _ = io.Copy(c.Writer, rc)
}

// deleteFile implements DELETE /:name/:version/:filename. If this was
// the last remaining file in the version, the version row is removed
// too — matches Forgejo's DeletePackageFile.
func (h *Handler) deleteFile(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireWrite(c, tenant) {
		return
	}
	name, version, filename, ok := readParams(c, true)
	if !ok {
		return
	}

	_, ver, file, err := h.lookup(c, tenant, name, version, filename)
	if err != nil {
		c.String(http.StatusNotFound, "%v", err)
		return
	}

	// Count siblings before deleting. If this is the only file on the
	// version, drop the version row (CASCADE removes the file).
	files, err := h.Models.ListFilesByVersion(c.Request.Context(), ver.ID)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	if len(files) == 1 {
		if err := h.Models.DeleteVersion(c.Request.Context(), ver.ID); err != nil {
			c.String(http.StatusInternalServerError, "%v", err)
			return
		}
	} else {
		if err := h.Models.DeleteFile(c.Request.Context(), file.ID); err != nil {
			c.String(http.StatusInternalServerError, "%v", err)
			return
		}
	}
	c.Status(http.StatusNoContent)
}

// deleteVersion implements DELETE /:name/:version (no filename).
// CASCADE on the package_files FK removes every file in one step.
func (h *Handler) deleteVersion(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireWrite(c, tenant) {
		return
	}
	name, version, _, ok := readParams(c, false)
	if !ok {
		return
	}

	pkg, err := h.Models.GetPackageByLookup(c.Request.Context(), tenant.ID, models.TypeGeneric, strings.ToLower(name))
	if err != nil {
		c.String(http.StatusNotFound, "package %s not found", name)
		return
	}
	ver, err := h.Models.GetVersion(c.Request.Context(), pkg.ID, version)
	if err != nil {
		c.String(http.StatusNotFound, "version %s@%s not found", name, version)
		return
	}
	if err := h.Models.DeleteVersion(c.Request.Context(), ver.ID); err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	c.Status(http.StatusNoContent)
}

// lookup resolves (tenant, name, version, filename) to the underlying
// package + version + file rows.
func (h *Handler) lookup(c *gin.Context, tenant *tenants.Tenant, name, version, filename string) (*models.Package, *models.Version, *models.File, error) {
	pkg, err := h.Models.GetPackageByLookup(c.Request.Context(), tenant.ID, models.TypeGeneric, strings.ToLower(name))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("package %s not found", name)
	}
	ver, err := h.Models.GetVersion(c.Request.Context(), pkg.ID, version)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("version %s@%s not found", name, version)
	}
	file, err := h.Models.GetFileByVersionAndName(c.Request.Context(), ver.ID, filename)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("file %s not found in %s@%s", filename, name, version)
	}
	return pkg, ver, file, nil
}

// --- policy hooks ----------------------------------------------------------

func (h *Handler) subjectFor(tenant *tenants.Tenant, pkg *models.Package, ver *models.Version, filename string) policy.Subject {
	s := policy.Subject{
		TenantID: tenant.ID,
		Format:   string(models.TypeGeneric),
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

func (h *Handler) checkRead(c *gin.Context, tenant *tenants.Tenant, pkg *models.Package, ver *models.Version, filename string) bool {
	r := h.Engine.Evaluate(c.Request.Context(), h.subjectFor(tenant, pkg, ver, filename), policy.ActionRead)
	if r.IsBlocked() {
		c.String(http.StatusForbidden, "%s", policyReason(r))
		return false
	}
	return true
}

func (h *Handler) checkIngest(c *gin.Context, tenant *tenants.Tenant, name, version, filename string) bool {
	subj := policy.Subject{
		TenantID: tenant.ID,
		Format:   string(models.TypeGeneric),
		Package:  strings.ToLower(name),
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

func policyReason(r policy.Result) string {
	if r.Reason == "" {
		return r.Decision.String()
	}
	return r.Reason
}
