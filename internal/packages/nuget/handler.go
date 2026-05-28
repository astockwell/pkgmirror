// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2021 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// HTTP routes for the NuGet V3 registry. The wire shape (the
// service-index resource list, RegistrationsBaseUrl /
// PackageBaseAddress / SearchQueryService / PackagePublish endpoints,
// the multipart upload convention, the case-insensitive id / version
// lookup) is modeled on
// forgejo/routers/api/packages/nuget/nuget.go (MIT). Deviations:
//
//  1. V2 (OData) is not exposed. The legacy protocol is needed only
//     for very old `nuget.exe` clients; the modern `dotnet` CLI and
//     Visual Studio 2017+ speak V3 exclusively. Skipping it keeps
//     the handler under 500 LOC and the test surface tractable.
//  2. Symbol packages (.snupkg) round-trip the upload path but we do
//     not currently extract portable PDB symbols. The
//     simple-symbol-query endpoint is not implemented.
//  3. Search returns the SearchResultResponse shape but performs a
//     simple substring match against package_id rather than running
//     a real full-text index. dotnet's search UX still works; rich
//     ranking would require a separate Bleve / SQLite FTS5 index.

package nuget

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/astockwell/pkgmirror/internal/auth"
	"github.com/astockwell/pkgmirror/internal/models"
	pkgsvc "github.com/astockwell/pkgmirror/internal/packages"
	"github.com/astockwell/pkgmirror/internal/policy"
	"github.com/astockwell/pkgmirror/internal/syncutil"
	"github.com/astockwell/pkgmirror/internal/tenants"

	"github.com/gin-gonic/gin"
)

// Content types NuGet clients expect.
const (
	contentTypeJSON   = "application/json"
	contentTypeNupkg  = "application/octet-stream"
	contentTypeNuspec = "application/xml"
)

// maxUploadBytes caps the upload size. NuGet packages above this
// limit are unusual (large .nupkgs typically embed many target
// frameworks) but tunable in a future config knob if needed.
const maxUploadBytes = 1 << 30 // 1 GiB

// Handler is the NuGet V3 registry HTTP handler.
type Handler struct {
	Service *pkgsvc.Service
	Models  *models.Store
	Tenants *tenants.Store
	Engine  policy.Engine

	// uploads serializes per-package multi-file uploads (.nupkg +
	// optional .snupkg). Keyed on `<tenantID>|<lowerID>` — matches
	// the pattern Maven uses for groupId:artifactId.
	uploads *syncutil.ExclusivePool
}

// NewHandler constructs a Handler. If eng is nil the no-op engine
// is used.
func NewHandler(svc *pkgsvc.Service, m *models.Store, ts *tenants.Store, eng policy.Engine) *Handler {
	if eng == nil {
		eng = policy.NoopEngine{}
	}
	return &Handler{Service: svc, Models: m, Tenants: ts, Engine: eng, uploads: syncutil.NewExclusivePool()}
}

// Register mounts NuGet routes on g (scoped to
// /api/packages/:tenant/nuget). The two catch-all groups
// (`/registration/*tail` and `/package/*tail`) work around gin's
// inability to mix a literal (`index.json`) with a parameter
// (`:version`) at the same path level. The handler dispatches on
// the tail's segment count.
func (h *Handler) Register(g *gin.RouterGroup) {
	g.GET("/index.json", h.serviceIndex)
	g.HEAD("/index.json", h.serviceIndex)

	g.GET("/query", h.search)
	g.HEAD("/query", h.search)

	g.GET("/registration/*tail", h.registrationDispatch)
	g.HEAD("/registration/*tail", h.registrationDispatch)

	g.GET("/package/*tail", h.packageDispatch)
	g.HEAD("/package/*tail", h.packageDispatch)

	g.PUT("", h.upload)
	g.PUT("/", h.upload)
	g.DELETE("/:id/:version", h.delete)
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

// baseFor reconstructs the absolute URL of the NuGet endpoint root
// for this tenant. Same heuristic the RPM handler uses for its
// .repo file — scheme picked from the request's TLS state, host
// from the inbound Host header.
func (h *Handler) baseFor(c *gin.Context, tenant *tenants.Tenant) string {
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/api/packages/%s/nuget", scheme, c.Request.Host, tenant.Name)
}

// storedNupkgName returns the on-disk filename for a .nupkg.
// NuGet's convention is lowercase id + version, and we honor it so
// the download URL `/package/<id>/<version>/<id>.<version>.nupkg`
// can be served straight from the file table.
func storedNupkgName(id, version string) string {
	return strings.ToLower(id) + "." + strings.ToLower(version) + ".nupkg"
}

// storedSnupkgName mirrors storedNupkgName for symbol packages.
func storedSnupkgName(id, version string) string {
	return strings.ToLower(id) + "." + strings.ToLower(version) + ".snupkg"
}

// storedNuspecName is the stored filename for the extracted nuspec.
func storedNuspecName(id string) string {
	return strings.ToLower(id) + ".nuspec"
}

// trimJSONSuffix lets the registration-leaf and other endpoints
// accept either `1.0.0` or `1.0.0.json`. NuGet's V3 spec emits the
// `.json` suffix in @id fields so clients echo it back; the lookup
// has to be tolerant either way.
func trimJSONSuffix(s string) string { return strings.TrimSuffix(s, ".json") }

// --- routes ----------------------------------------------------------------

func (h *Handler) serviceIndex(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}
	l := &linkBuilder{Base: h.baseFor(c, tenant)}
	writeJSON(c, http.StatusOK, l.ServiceIndex())
}

// registrationDispatch handles the `/registration/...` family:
//
//	/registration/<id>/index.json       → registration index
//	/registration/<id>/<version>[.json] → registration leaf
func (h *Handler) registrationDispatch(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}
	parts := splitTail(c.Param("tail"))
	switch {
	case len(parts) == 2 && parts[1] == "index.json":
		h.registrationIndex(c, tenant, parts[0])
	case len(parts) == 2:
		h.registrationLeaf(c, tenant, parts[0], trimJSONSuffix(parts[1]))
	default:
		c.String(http.StatusNotFound, "unknown registration path")
	}
}

func (h *Handler) registrationIndex(c *gin.Context, tenant *tenants.Tenant, id string) {
	entries, err := h.loadVersionsByID(c.Request.Context(), tenant.ID, id)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	if len(entries) == 0 {
		c.String(http.StatusNotFound, "package %q not found", id)
		return
	}
	l := &linkBuilder{Base: h.baseFor(c, tenant)}
	resp := buildRegistrationIndex(l, entries[0].ID, entries)
	writeJSON(c, http.StatusOK, resp)
}

func (h *Handler) registrationLeaf(c *gin.Context, tenant *tenants.Tenant, id, version string) {
	entries, err := h.loadVersionsByID(c.Request.Context(), tenant.ID, id)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	for _, e := range entries {
		if strings.EqualFold(e.Version, version) {
			l := &linkBuilder{Base: h.baseFor(c, tenant)}
			writeJSON(c, http.StatusOK, buildRegistrationLeaf(l, e.ID, e))
			return
		}
	}
	c.String(http.StatusNotFound, "version %q not found for %q", version, id)
}

// packageDispatch handles the `/package/...` family:
//
//	/package/<id>/index.json                  → versions list
//	/package/<id>/<version>/<filename>        → download nupkg/nuspec
func (h *Handler) packageDispatch(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	parts := splitTail(c.Param("tail"))
	switch {
	case len(parts) == 2 && parts[1] == "index.json":
		if !auth.RequireRead(c, tenant) {
			return
		}
		h.packageVersions(c, tenant, parts[0])
	case len(parts) == 3:
		// download is policy-aware; auth + policy check inside.
		h.download(c, tenant, parts[0], parts[1], parts[2])
	default:
		c.String(http.StatusNotFound, "unknown package path")
	}
}

func (h *Handler) packageVersions(c *gin.Context, tenant *tenants.Tenant, id string) {
	entries, err := h.loadVersionsByID(c.Request.Context(), tenant.ID, id)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	if len(entries) == 0 {
		c.String(http.StatusNotFound, "package %q not found", id)
		return
	}
	writeJSON(c, http.StatusOK, buildPackageVersions(entries))
}

func (h *Handler) download(c *gin.Context, tenant *tenants.Tenant, id, version, filename string) {
	// nuspec and nupkg downloads share this path. Resolve the file
	// by name within the version.
	if !auth.RequireRead(c, tenant) {
		return
	}
	want := strings.ToLower(filename)
	wantNupkg := storedNupkgName(id, version)
	wantSnupkg := storedSnupkgName(id, version)
	wantNuspec := storedNuspecName(id)
	if want != wantNupkg && want != wantSnupkg && want != wantNuspec {
		c.String(http.StatusNotFound, "unsupported filename %q", filename)
		return
	}

	pkg, ver, file, err := h.lookupFile(c.Request.Context(), tenant.ID, id, version, want)
	if err != nil {
		c.String(http.StatusNotFound, "%v", err)
		return
	}
	if !h.checkRead(c, tenant, pkg, ver, want) {
		return
	}

	rc, blob, err := h.Service.OpenFile(c.Request.Context(), file)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	defer rc.Close()

	ctype := contentTypeNupkg
	if strings.HasSuffix(want, ".nuspec") {
		ctype = contentTypeNuspec
	}
	c.Header("Content-Type", ctype)
	c.Header("Content-Length", fmt.Sprintf("%d", blob.Size))
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, want))
	c.Header("Last-Modified", time.Unix(file.CreatedUnix, 0).UTC().Format(http.TimeFormat))
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	_, _ = io.Copy(c.Writer, rc)
}

// search implements the V3 SearchQueryService. q is matched as a
// case-insensitive substring on package name; `prerelease=true` is
// accepted but not filtered against (we don't currently tag
// pre-release versions distinctly).
func (h *Handler) search(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}
	q := strings.TrimSpace(c.Query("q"))
	grouped, total, err := h.searchVersions(c.Request.Context(), tenant.ID, q)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	l := &linkBuilder{Base: h.baseFor(c, tenant)}
	writeJSON(c, http.StatusOK, buildSearchResults(l, total, grouped))
}

// upload accepts a PUT against the PackagePublish root.
// dotnet nuget push sends `multipart/form-data` with a single file
// field (any name); nuget.exe with --DirectPush variants and curl
// users send raw application/octet-stream. We accept both.
func (h *Handler) upload(c *gin.Context) {
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

	buf, err := h.Service.NewHashedBuffer(io.LimitReader(body, maxUploadBytes))
	if err != nil {
		c.String(http.StatusInternalServerError, "buffer: %v", err)
		return
	}
	defer buf.Close()

	pkg, err := ParsePackage(buf, buf.Size())
	if err != nil {
		switch {
		case errors.Is(err, ErrMissingNuspecFile),
			errors.Is(err, ErrNuspecFileTooLarge),
			errors.Is(err, ErrNuspecInvalidID),
			errors.Is(err, ErrNuspecInvalidVersion):
			c.String(http.StatusBadRequest, "%v", err)
		default:
			c.String(http.StatusBadRequest, "parse nupkg: %v", err)
		}
		return
	}
	if pkg.PackageType != DependencyPackage {
		c.String(http.StatusBadRequest, "symbols-package endpoint is /symbolpackage; got DependencyPackage expected")
		return
	}
	if _, err := buf.Seek(0, io.SeekStart); err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}

	// Serialize per-package multi-file uploads. Two PUTs for the
	// same id from different connections would otherwise race in
	// CreatePackageOrAddFileToExisting.
	lockKey := fmt.Sprintf("%d|%s", tenant.ID, strings.ToLower(pkg.ID))
	h.uploads.CheckIn(lockKey)
	defer h.uploads.CheckOut(lockKey)

	if !h.checkIngest(c, tenant, pkg.ID, pkg.Version, storedNupkgName(pkg.ID, pkg.Version)) {
		return
	}

	metaJSON, _ := json.Marshal(pkg.Metadata)
	_, _, _, err = h.Service.CreatePackageOrAddFileToExisting(
		c.Request.Context(),
		pkgsvc.CreationInfo{
			TenantID:            tenant.ID,
			PackageType:         models.TypeNuGet,
			PackageName:         pkg.ID,
			PackageLookupName:   strings.ToLower(pkg.ID),
			Version:             pkg.Version,
			VersionMetadataJSON: string(metaJSON),
			Filename:            storedNupkgName(pkg.ID, pkg.Version),
			IsLead:              true,
		}, buf)
	if err != nil {
		if errors.Is(err, models.ErrDuplicatePackageFile) {
			c.String(http.StatusConflict, "%s.%s already exists", pkg.ID, pkg.Version)
			return
		}
		c.String(http.StatusInternalServerError, "ingest: %v", err)
		return
	}

	// Persist the extracted nuspec as a sibling file row so
	// /package/<id>/<version>/<id>.nuspec can serve it without
	// re-opening the zip on every download.
	nuspecBytes := pkg.NuspecContent.Bytes()
	nuspecBuf, err := h.Service.NewHashedBuffer(strings.NewReader(string(nuspecBytes)))
	if err == nil {
		_, _, _, err = h.Service.CreatePackageOrAddFileToExisting(
			c.Request.Context(),
			pkgsvc.CreationInfo{
				TenantID:            tenant.ID,
				PackageType:         models.TypeNuGet,
				PackageName:         pkg.ID,
				PackageLookupName:   strings.ToLower(pkg.ID),
				Version:             pkg.Version,
				VersionMetadataJSON: string(metaJSON),
				Filename:            storedNuspecName(pkg.ID),
				IsLead:              false,
			}, nuspecBuf)
		_ = nuspecBuf.Close()
		// A duplicate nuspec on a re-upload (which we already
		// rejected above) shouldn't fail. Any other error here is
		// silent — the .nupkg is the source of truth.
		if err != nil && !errors.Is(err, models.ErrDuplicatePackageFile) {
			c.String(http.StatusInternalServerError, "store nuspec: %v", err)
			return
		}
	}

	c.Status(http.StatusCreated)
}

func (h *Handler) delete(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireWrite(c, tenant) {
		return
	}
	id := c.Param("id")
	version := c.Param("version")

	ctx := c.Request.Context()
	row := h.Models.DB.QueryRowContext(ctx, `
		SELECT v.id
		FROM package_versions v
		JOIN packages p ON p.id = v.package_id
		WHERE p.tenant_id = ? AND p.type = 'nuget' AND p.lower_name = ? AND lower(v.version) = ?
	`, tenant.ID, strings.ToLower(id), strings.ToLower(version))
	var versionID int64
	if err := row.Scan(&versionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.String(http.StatusNotFound, "package %q version %q not found", id, version)
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

// lookupFile resolves a single file row by (tenant, lowercase id,
// lowercase version, lowercase filename).
func (h *Handler) lookupFile(ctx context.Context, tenantID int64, id, version, filename string) (*models.Package, *models.Version, *models.File, error) {
	row := h.Models.DB.QueryRowContext(ctx, `
		SELECT f.id, f.version_id, f.blob_id, f.name, f.is_lead, f.created_unix,
		       v.id, v.package_id, v.version, v.metadata_json, v.created_unix,
		       v.license, v.quarantine_reason, v.quarantined_by_rule_id,
		       p.id, p.tenant_id, p.type, p.name, p.lower_name, p.created_unix
		FROM package_files f
		JOIN package_versions v ON v.id = f.version_id
		JOIN packages p ON p.id = v.package_id
		WHERE p.tenant_id = ? AND p.type = 'nuget' AND p.lower_name = ? AND lower(v.version) = ? AND f.name = ?
	`, tenantID, strings.ToLower(id), strings.ToLower(version), strings.ToLower(filename))

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
			return nil, nil, nil, fmt.Errorf("file %q not found for %s.%s", filename, id, version)
		}
		return nil, nil, nil, err
	}
	return &p, &v, &f, nil
}

// loadVersionsByID returns all (non-quarantined) versions of a
// package, with parsed Metadata, suitable for handing to the V3
// builders. Lookup is case-insensitive on id.
func (h *Handler) loadVersionsByID(ctx context.Context, tenantID int64, id string) ([]*versionEntry, error) {
	rows, err := h.Models.DB.QueryContext(ctx, `
		SELECT p.name, v.id, v.package_id, v.version, v.metadata_json, v.created_unix,
		       v.license, v.quarantine_reason, v.quarantined_by_rule_id
		FROM package_versions v
		JOIN packages p ON p.id = v.package_id
		WHERE p.tenant_id = ? AND p.type = 'nuget' AND p.lower_name = ?
		  AND v.quarantine_reason IS NULL
		ORDER BY v.created_unix
	`, tenantID, strings.ToLower(id))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*versionEntry
	for rows.Next() {
		var (
			name string
			v    models.Version
		)
		if err := rows.Scan(&name, &v.ID, &v.PackageID, &v.Version, &v.MetadataJSON, &v.CreatedUnix,
			&v.License, &v.QuarantineReason, &v.QuarantinedByRuleID); err != nil {
			return nil, err
		}
		m := &Metadata{}
		if v.MetadataJSON != "" {
			_ = json.Unmarshal([]byte(v.MetadataJSON), m)
		}
		vv := v
		out = append(out, &versionEntry{ID: name, Version: v.Version, Metadata: m, Ver: &vv})
	}
	return out, rows.Err()
}

// searchVersions returns versions grouped by id whose name contains
// q (case-insensitive); empty q returns everything. The grouped map
// is keyed on the lowercase id; each group is emitted with the
// original-case id pulled from one of its members.
func (h *Handler) searchVersions(ctx context.Context, tenantID int64, q string) (map[string][]*versionEntry, int64, error) {
	like := "%" + strings.ToLower(q) + "%"
	rows, err := h.Models.DB.QueryContext(ctx, `
		SELECT p.name, p.lower_name,
		       v.id, v.package_id, v.version, v.metadata_json, v.created_unix,
		       v.license, v.quarantine_reason, v.quarantined_by_rule_id
		FROM package_versions v
		JOIN packages p ON p.id = v.package_id
		WHERE p.tenant_id = ? AND p.type = 'nuget'
		  AND p.lower_name LIKE ?
		  AND v.quarantine_reason IS NULL
		ORDER BY p.lower_name, v.created_unix
	`, tenantID, like)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	grouped := map[string][]*versionEntry{}
	for rows.Next() {
		var (
			name, lower string
			v           models.Version
		)
		if err := rows.Scan(&name, &lower, &v.ID, &v.PackageID, &v.Version, &v.MetadataJSON, &v.CreatedUnix,
			&v.License, &v.QuarantineReason, &v.QuarantinedByRuleID); err != nil {
			return nil, 0, err
		}
		m := &Metadata{}
		if v.MetadataJSON != "" {
			_ = json.Unmarshal([]byte(v.MetadataJSON), m)
		}
		vv := v
		grouped[lower] = append(grouped[lower], &versionEntry{ID: name, Version: v.Version, Metadata: m, Ver: &vv})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return grouped, int64(len(grouped)), nil
}

// --- policy hooks ----------------------------------------------------------

func (h *Handler) subjectFor(tenant *tenants.Tenant, pkg *models.Package, ver *models.Version, filename string) policy.Subject {
	s := policy.Subject{
		TenantID: tenant.ID,
		Format:   string(models.TypeNuGet),
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

func (h *Handler) checkIngest(c *gin.Context, tenant *tenants.Tenant, name, version, filename string) bool {
	subj := policy.Subject{
		TenantID: tenant.ID,
		Format:   string(models.TypeNuGet),
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

// --- response + body helpers -----------------------------------------------

func writeJSON(c *gin.Context, status int, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		c.String(http.StatusInternalServerError, "marshal: %v", err)
		return
	}
	c.Header("Content-Type", contentTypeJSON)
	c.Header("Content-Length", fmt.Sprintf("%d", len(b)))
	if c.Request.Method == http.MethodHead {
		c.Status(status)
		return
	}
	c.Data(status, contentTypeJSON, b)
}

// splitTail trims gin's leading `/` and splits on `/`. A bare `/`
// returns an empty slice. Used to dispatch on segment count for the
// `/registration/...` and `/package/...` catch-alls.
func splitTail(s string) []string {
	s = strings.TrimPrefix(s, "/")
	if s == "" {
		return nil
	}
	return strings.Split(s, "/")
}

// openUploadBody returns a reader over the uploaded .nupkg bytes
// regardless of whether the client used multipart/form-data
// (`dotnet nuget push` and Visual Studio) or raw octet-stream (curl,
// power users). For multipart, the first file field wins.
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
