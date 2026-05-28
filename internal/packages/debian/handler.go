// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2023 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// HTTP routes for the Debian repository format. The wire shape
// (`dists/<dist>/Release|Release.gpg|InRelease`,
// `dists/<dist>/<component>/binary-<arch>/Packages[.gz|.xz]`,
// `pool/<dist>/<component>/<file>.deb`, the `PUT pool/.../upload`
// upload convention) is modeled on
// forgejo/routers/api/packages/debian/debian.go (MIT). We deviate in
// two places:
//
//  1. Repository index files (Packages, Release, etc.) are built on
//     demand from the live file list rather than persisted as file
//     rows. See docs/adding-a-format.md "On-demand vs cached index
//     generation".
//  2. The composite `(distribution, component, architecture)` per
//     file is encoded into the file row's name as
//     `<dist>|<comp>|<arch>|<basename>` (same pattern Alpine uses) so
//     UNIQUE(version_id, name) holds across multi-arch publishes
//     without a schema change.

package debian

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
	"github.com/astockwell/pkgmirror/internal/syncutil"
	"github.com/astockwell/pkgmirror/internal/tenants"

	"github.com/gin-gonic/gin"
)

// Content types apt expects.
const (
	contentTypeDeb       = "application/vnd.debian.binary-package"
	contentTypeGPGKey    = "application/pgp-keys"
	contentTypeRelease   = "text/plain; charset=utf-8"
	contentTypePackages  = "text/plain; charset=utf-8"
	contentTypeSignature = "application/pgp-signature"
)

// Handler is the Debian registry HTTP handler.
type Handler struct {
	Service *pkgsvc.Service
	Models  *models.Store
	Tenants *tenants.Store
	Engine  policy.Engine

	// keygen serializes per-tenant first-time GPG key generation.
	// Without this, concurrent `apt update` requests for a new
	// tenant race in GetOrCreateKeyPair and the second one fails on
	// the UNIQUE property constraint. Same pattern Maven uses for
	// per-package upload locks.
	keygen *syncutil.ExclusivePool
}

// NewHandler constructs a Handler. If eng is nil the no-op engine is
// used.
func NewHandler(svc *pkgsvc.Service, m *models.Store, ts *tenants.Store, eng policy.Engine) *Handler {
	if eng == nil {
		eng = policy.NoopEngine{}
	}
	return &Handler{Service: svc, Models: m, Tenants: ts, Engine: eng, keygen: syncutil.NewExclusivePool()}
}

// Register mounts Debian routes on g (scoped to
// /api/packages/:tenant/debian).
//
//	GET    /key.gpg                                              public GPG key
//	GET    /dists/:dist/Release                                  generated release file
//	GET    /dists/:dist/Release.gpg                              detached signature
//	GET    /dists/:dist/InRelease                                clearsigned release
//	GET    /dists/:dist/:comp/:archseg/:filename                 Packages[.gz|.xz]
//	GET    /pool/:dist/:comp/:filename                           download a .deb
//	HEAD                                                          headers-only mirror of all of the above
//	PUT    /pool/:dist/:comp/upload                              upload a .deb
//	DELETE /pool/:dist/:comp/:name/:version/:architecture        delete a .deb
//
// `:archseg` matches `binary-<arch>` literally; the handler strips
// the `binary-` prefix. gin can't express that pattern with a
// param-in-the-middle so we receive the whole segment and parse it.
func (h *Handler) Register(g *gin.RouterGroup) {
	g.GET("/key.gpg", h.getRepositoryKey)
	g.HEAD("/key.gpg", h.getRepositoryKey)

	g.GET("/dists/:distribution/Release", h.getRelease)
	g.HEAD("/dists/:distribution/Release", h.getRelease)
	g.GET("/dists/:distribution/Release.gpg", h.getReleaseGpg)
	g.HEAD("/dists/:distribution/Release.gpg", h.getReleaseGpg)
	g.GET("/dists/:distribution/InRelease", h.getInRelease)
	g.HEAD("/dists/:distribution/InRelease", h.getInRelease)

	g.GET("/dists/:distribution/:component/:archseg/:filename", h.getPackages)
	g.HEAD("/dists/:distribution/:component/:archseg/:filename", h.getPackages)

	g.PUT("/pool/:distribution/:component/upload", h.upload)
	g.GET("/pool/:distribution/:component/:filename", h.download)
	g.HEAD("/pool/:distribution/:component/:filename", h.download)
	g.DELETE("/pool/:distribution/:component/:name/:version/:architecture", h.delete)
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

// storedFileName encodes (dist, component, arch, basename) into the
// file row's Name. Inverse: parseStoredFileName.
func storedFileName(dist, component, arch, basename string) string {
	return dist + "|" + component + "|" + arch + "|" + basename
}

func parseStoredFileName(name string) (dist, component, arch, basename string, ok bool) {
	parts := strings.SplitN(name, "|", 4)
	if len(parts) != 4 {
		return "", "", "", "", false
	}
	return parts[0], parts[1], parts[2], parts[3], true
}

// debBasename is the on-wire filename apt requests.
func debBasename(name, version, arch string) string {
	return name + "_" + version + "_" + arch + ".deb"
}

// parseDebBasename inverts debBasename. Returns ok=false on a
// malformed input (e.g. fewer than 3 underscore-separated segments).
// The Debian name + version + arch regexes all forbid underscores so
// a simple 3-way split is unambiguous.
func parseDebBasename(filename string) (name, version, arch string, ok bool) {
	if !strings.HasSuffix(filename, ".deb") {
		return "", "", "", false
	}
	core := strings.TrimSuffix(filename, ".deb")
	parts := strings.SplitN(core, "_", 3)
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// --- routes ----------------------------------------------------------------

func (h *Handler) getRepositoryKey(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}
	_, pub, err := h.materializeKey(c.Request.Context(), tenant.ID)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	c.Header("Content-Type", contentTypeGPGKey)
	c.Header("Content-Disposition", `attachment; filename="repository.key"`)
	c.Header("Content-Length", fmt.Sprintf("%d", len(pub)))
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	_, _ = io.WriteString(c.Writer, pub)
}

// materializeKey wraps GetOrCreateKeyPair in the per-tenant
// generation lock. The lock matters only on first call for a tenant;
// once the key exists, all subsequent calls return immediately.
func (h *Handler) materializeKey(ctx context.Context, tenantID int64) (priv, pub string, err error) {
	key := fmt.Sprintf("%d|debian-key", tenantID)
	h.keygen.CheckIn(key)
	defer h.keygen.CheckOut(key)
	return GetOrCreateKeyPair(ctx, h.Models, tenantID)
}

func (h *Handler) getPackages(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}
	dist := c.Param("distribution")
	comp := c.Param("component")
	archseg := c.Param("archseg")
	filename := c.Param("filename")
	if !strings.HasPrefix(archseg, "binary-") {
		c.String(http.StatusNotFound, "expected binary-<arch>")
		return
	}
	arch := strings.TrimPrefix(archseg, "binary-")

	entries, err := h.loadEntriesByCoord(c.Request.Context(), tenant.ID, dist, comp, arch)
	if err != nil {
		c.String(http.StatusInternalServerError, "load entries: %v", err)
		return
	}
	if len(entries) == 0 {
		c.String(http.StatusNotFound, "no packages for %s/%s/%s", dist, comp, arch)
		return
	}
	indices, err := BuildPackagesIndices(dist, comp, entries)
	if err != nil {
		c.String(http.StatusInternalServerError, "build: %v", err)
		return
	}

	var body []byte
	switch filename {
	case "Packages":
		body = indices.Plain
	case "Packages.gz":
		body = indices.Gzip
	case "Packages.xz":
		body = indices.Xz
	default:
		c.String(http.StatusNotFound, "unknown index variant %q", filename)
		return
	}
	c.Header("Content-Type", contentTypePackages)
	c.Header("Content-Length", fmt.Sprintf("%d", len(body)))
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	_, _ = c.Writer.Write(body)
}

func (h *Handler) getRelease(c *gin.Context)    { h.serveReleaseFamily(c, "release") }
func (h *Handler) getReleaseGpg(c *gin.Context) { h.serveReleaseFamily(c, "gpg") }
func (h *Handler) getInRelease(c *gin.Context)  { h.serveReleaseFamily(c, "inrelease") }

func (h *Handler) serveReleaseFamily(c *gin.Context, which string) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}
	dist := c.Param("distribution")

	files, err := h.buildReleaseFamily(c.Request.Context(), tenant.ID, dist)
	if err != nil {
		c.String(http.StatusInternalServerError, "build release: %v", err)
		return
	}
	if files == nil {
		c.String(http.StatusNotFound, "no packages in distribution %q", dist)
		return
	}

	var body []byte
	var ctype string
	switch which {
	case "release":
		body, ctype = files.Release, contentTypeRelease
	case "gpg":
		body, ctype = files.GPG, contentTypeSignature
	case "inrelease":
		body, ctype = files.InRelease, contentTypeRelease
	}
	c.Header("Content-Type", ctype)
	c.Header("Content-Length", fmt.Sprintf("%d", len(body)))
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	_, _ = c.Writer.Write(body)
}

func (h *Handler) buildReleaseFamily(ctx context.Context, tenantID int64, dist string) (*ReleaseFiles, error) {
	components, arches, err := h.distinctCompsAndArches(ctx, tenantID, dist)
	if err != nil {
		return nil, err
	}
	if len(components) == 0 || len(arches) == 0 {
		return nil, nil
	}

	indices := make([]PerArchPackagesIndex, 0, len(components)*len(arches))
	var newestUnix int64
	for _, comp := range components {
		for _, arch := range arches {
			entries, err := h.loadEntriesByCoord(ctx, tenantID, dist, comp, arch)
			if err != nil {
				return nil, err
			}
			if len(entries) == 0 {
				continue
			}
			for _, e := range entries {
				if e.File.CreatedUnix > newestUnix {
					newestUnix = e.File.CreatedUnix
				}
			}
			built, err := BuildPackagesIndices(dist, comp, entries)
			if err != nil {
				return nil, err
			}
			indices = append(indices, PerArchPackagesIndex{Component: comp, Architecture: arch, Indices: built})
		}
	}
	if len(indices) == 0 {
		return nil, nil
	}

	priv, _, err := h.materializeKey(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	// Derive Date from the newest file in the distribution so the
	// bytes are deterministic across the /Release vs /Release.gpg
	// request pair. See BuildReleaseFiles for the full rationale.
	return BuildReleaseFiles(dist, components, arches, indices, priv, time.Unix(newestUnix, 0))
}

// --- upload / download / delete --------------------------------------------

func (h *Handler) upload(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireWrite(c, tenant) {
		return
	}
	dist := strings.TrimSpace(c.Param("distribution"))
	comp := strings.TrimSpace(c.Param("component"))
	if dist == "" || comp == "" {
		c.String(http.StatusBadRequest, "distribution and component are required")
		return
	}

	buf, err := h.Service.NewHashedBuffer(c.Request.Body)
	if err != nil {
		c.String(http.StatusInternalServerError, "buffer: %v", err)
		return
	}
	defer buf.Close()

	if _, err := buf.Seek(0, io.SeekStart); err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	pkg, err := ParsePackage(buf)
	if err != nil {
		switch {
		case errors.Is(err, ErrMissingControlFile),
			errors.Is(err, ErrUnsupportedCompression),
			errors.Is(err, ErrInvalidName),
			errors.Is(err, ErrInvalidVersion),
			errors.Is(err, ErrInvalidArchitecture):
			c.String(http.StatusBadRequest, "%v", err)
		default:
			c.String(http.StatusBadRequest, "parse deb: %v", err)
		}
		return
	}
	if _, err := buf.Seek(0, io.SeekStart); err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}

	if !h.checkIngest(c, tenant, pkg.Name, pkg.Version, debBasename(pkg.Name, pkg.Version, pkg.Architecture), "") {
		return
	}

	metaJSON, _ := json.Marshal(pkg.Metadata)
	basename := debBasename(pkg.Name, pkg.Version, pkg.Architecture)

	_, _, file, err := h.Service.CreatePackageOrAddFileToExisting(
		c.Request.Context(),
		pkgsvc.CreationInfo{
			TenantID:            tenant.ID,
			PackageType:         models.TypeDebian,
			PackageName:         pkg.Name,
			PackageLookupName:   strings.ToLower(pkg.Name),
			Version:             pkg.Version,
			VersionMetadataJSON: string(metaJSON),
			Filename:            storedFileName(dist, comp, pkg.Architecture, basename),
			IsLead:              true,
		}, buf)
	if err != nil {
		if errors.Is(err, models.ErrDuplicatePackageFile) {
			c.String(http.StatusConflict, "file %s already exists in %s/%s", basename, dist, comp)
			return
		}
		c.String(http.StatusInternalServerError, "ingest: %v", err)
		return
	}

	// Persist the file-level properties needed for index rebuild.
	// We store the verbatim control text so the Packages index can
	// reproduce it byte-for-byte without re-parsing the .deb blob.
	ctx := c.Request.Context()
	if err := h.Models.SetProperty(ctx, models.PropertyRefFile, file.ID, PropertyDistribution, dist); err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	if err := h.Models.SetProperty(ctx, models.PropertyRefFile, file.ID, PropertyComponent, comp); err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	if err := h.Models.SetProperty(ctx, models.PropertyRefFile, file.ID, PropertyArchitecture, pkg.Architecture); err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	if err := h.Models.SetProperty(ctx, models.PropertyRefFile, file.ID, PropertyControl, pkg.Control); err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}

	c.Status(http.StatusCreated)
}

func (h *Handler) download(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	dist := c.Param("distribution")
	comp := c.Param("component")
	filename := c.Param("filename")

	name, version, arch, ok := parseDebBasename(filename)
	if !ok {
		c.String(http.StatusNotFound, "not a .deb filename: %q", filename)
		return
	}

	pkg, ver, file, err := h.lookupFile(c.Request.Context(), tenant.ID, dist, comp, arch, name, version)
	if err != nil {
		c.String(http.StatusNotFound, "%v", err)
		return
	}
	if !auth.RequireRead(c, tenant) {
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

	c.Header("Content-Type", contentTypeDeb)
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filename))
	c.Header("Content-Length", fmt.Sprintf("%d", blob.Size))
	c.Header("Last-Modified", time.Unix(file.CreatedUnix, 0).UTC().Format(http.TimeFormat))
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	_, _ = io.Copy(c.Writer, rc)
}

func (h *Handler) delete(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireWrite(c, tenant) {
		return
	}
	dist := c.Param("distribution")
	comp := c.Param("component")
	name := c.Param("name")
	version := c.Param("version")
	arch := c.Param("architecture")

	_, ver, file, err := h.lookupFile(c.Request.Context(), tenant.ID, dist, comp, arch, name, version)
	if err != nil {
		c.String(http.StatusNotFound, "%v", err)
		return
	}

	// Match the "last file removes the version" pattern other
	// formats use to keep version rows free of orphans.
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

// --- DB lookups ------------------------------------------------------------

func (h *Handler) lookupFile(ctx context.Context, tenantID int64, dist, comp, arch, name, version string) (*models.Package, *models.Version, *models.File, error) {
	storedName := storedFileName(dist, comp, arch, debBasename(name, version, arch))
	row := h.Models.DB.QueryRowContext(ctx, `
		SELECT f.id, f.version_id, f.blob_id, f.name, f.is_lead, f.created_unix,
		       v.id, v.package_id, v.version, v.metadata_json, v.created_unix,
		       v.license, v.quarantine_reason, v.quarantined_by_rule_id,
		       p.id, p.tenant_id, p.type, p.name, p.lower_name, p.created_unix
		FROM package_files f
		JOIN package_versions v ON v.id = f.version_id
		JOIN packages p ON p.id = v.package_id
		WHERE p.tenant_id = ? AND p.type = 'debian' AND f.name = ?
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
			return nil, nil, nil, fmt.Errorf("file not found in %s/%s/%s", dist, comp, arch)
		}
		return nil, nil, nil, err
	}
	return &p, &v, &f, nil
}

// loadEntriesByCoord pulls the IndexEntry rows for one (dist, comp,
// arch). Drains the cursor before issuing per-row property lookups
// to dodge the SQLITE_BUSY pattern documented in
// docs/adding-a-format.md.
func (h *Handler) loadEntriesByCoord(ctx context.Context, tenantID int64, dist, comp, arch string) ([]*IndexEntry, error) {
	prefix := dist + "|" + comp + "|" + arch + "|"
	rows, err := h.Models.DB.QueryContext(ctx, `
		SELECT f.id, f.version_id, f.blob_id, f.name, f.is_lead, f.created_unix,
		       p.id, p.name,
		       v.id, v.version,
		       b.id, b.size, b.hash_md5, b.hash_sha1, b.hash_sha256, b.hash_sha512
		FROM package_files f
		JOIN package_versions v ON v.id = f.version_id
		JOIN packages p ON p.id = v.package_id
		JOIN package_blobs b ON b.id = f.blob_id
		WHERE p.tenant_id = ? AND p.type = 'debian' AND p.lower_name != ?
		  AND v.quarantine_reason IS NULL
		  AND f.name LIKE ?
		ORDER BY p.lower_name, v.version, f.name
	`, tenantID, RepositoryPackage, prefix+"%")
	if err != nil {
		return nil, err
	}

	type pending struct {
		fileID int64
		entry  *IndexEntry
	}
	var pendings []pending
	for rows.Next() {
		var (
			f models.File
			p models.Package
			v models.Version
			b models.Blob
		)
		if err := rows.Scan(
			&f.ID, &f.VersionID, &f.BlobID, &f.Name, &f.IsLead, &f.CreatedUnix,
			&p.ID, &p.Name,
			&v.ID, &v.Version,
			&b.ID, &b.Size, &b.HashMD5, &b.HashSHA1, &b.HashSHA256, &b.HashSHA512,
		); err != nil {
			rows.Close()
			return nil, err
		}
		pendings = append(pendings, pending{
			fileID: f.ID,
			entry: &IndexEntry{
				Pkg:  &p,
				Ver:  &v,
				Blob: &b,
				File: &f,
			},
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	out := make([]*IndexEntry, 0, len(pendings))
	for _, p := range pendings {
		// Restore the on-wire basename from the stored composite
		// name (`dist|comp|arch|basename`) — the Packages index's
		// `Filename:` field uses the basename, not our internal
		// representation.
		_, _, _, basename, ok := parseStoredFileName(p.entry.File.Name)
		if ok {
			// Mutate File.Name in-place for index emission. The
			// File row we pass back is short-lived (lives only for
			// the duration of this Packages request) so this
			// rewrite is safe.
			p.entry.File.Name = basename
		}
		ctrl, hasCtrl, err := h.Models.GetProperty(ctx, models.PropertyRefFile, p.fileID, PropertyControl)
		if err != nil {
			return nil, err
		}
		if hasCtrl {
			p.entry.Control = ctrl
		}
		out = append(out, p.entry)
	}
	return out, nil
}

// distinctCompsAndArches lists the components + architectures
// currently published to (tenant, distribution). Used to populate
// the Release file's Components/Architectures lines and to drive
// the per-(component, arch) Packages index loop.
func (h *Handler) distinctCompsAndArches(ctx context.Context, tenantID int64, dist string) ([]string, []string, error) {
	prefix := dist + "|"
	rows, err := h.Models.DB.QueryContext(ctx, `
		SELECT DISTINCT f.name
		FROM package_files f
		JOIN package_versions v ON v.id = f.version_id
		JOIN packages p ON p.id = v.package_id
		WHERE p.tenant_id = ? AND p.type = 'debian' AND p.lower_name != ?
		  AND v.quarantine_reason IS NULL
		  AND f.name LIKE ?
	`, tenantID, RepositoryPackage, prefix+"%")
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	comps := map[string]struct{}{}
	arches := map[string]struct{}{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, nil, err
		}
		_, c, a, _, ok := parseStoredFileName(name)
		if !ok {
			continue
		}
		comps[c] = struct{}{}
		arches[a] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	out := func(set map[string]struct{}) []string {
		s := make([]string, 0, len(set))
		for k := range set {
			s = append(s, k)
		}
		sort.Strings(s)
		return s
	}
	return out(comps), out(arches), nil
}

// --- policy hooks ----------------------------------------------------------

func (h *Handler) subjectFor(tenant *tenants.Tenant, pkg *models.Package, ver *models.Version, filename string) policy.Subject {
	s := policy.Subject{
		TenantID: tenant.ID,
		Format:   string(models.TypeDebian),
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
		Format:   string(models.TypeDebian),
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
