// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2023 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// HTTP routes for the RPM (yum/dnf) registry. Wire shape is modeled
// on forgejo/routers/api/packages/rpm/rpm.go (MIT):
//
//	GET    /:group/repository.key            armored OpenPGP public key
//	GET    /:group/repository.repo           dnf config snippet
//	GET    /:group/repodata/:filename        repomd.xml + *.xml.gz + repomd.xml.asc
//	HEAD   /:group/repodata/:filename        headers-only existence check
//	GET    /:group/package/:name/:version/:architecture/:filename   download .rpm
//	HEAD   /:group/package/:name/:version/:architecture/:filename
//	PUT    /:group/upload                    upload .rpm
//	DELETE /:group/package/:name/:version/:architecture
//
// `:group` is a single path segment for MVP (e.g. "el9" or
// "default"). Multi-segment groups ("el9/x86_64") are a Forgejo
// nicety that requires a catch-all dispatcher — deferred until an
// operator actually asks for it. Single-segment is enough to mirror
// distinct repositories per tenant.

package rpm

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

const (
	contentTypeRPM       = "application/x-rpm"
	contentTypeXML       = "text/xml; charset=utf-8"
	contentTypeOctet     = "application/octet-stream"
	contentTypeGPGKey    = "application/pgp-keys"
	contentTypeRepoFile  = "text/plain; charset=utf-8"
	contentTypeSignature = "application/pgp-signature"
)

// Handler is the RPM registry HTTP handler.
type Handler struct {
	Service *pkgsvc.Service
	Models  *models.Store
	Tenants *tenants.Store
	Engine  policy.Engine

	// keygen serializes per-tenant first-time GPG key generation.
	// Without it, two concurrent first `dnf makecache` requests for
	// a fresh tenant race in GetOrCreateKeyPair and the loser fails
	// on the UNIQUE property constraint. Same pattern Debian uses.
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

// Register mounts RPM routes on g (expected to be scoped to
// /api/packages/:tenant/rpm).
func (h *Handler) Register(g *gin.RouterGroup) {
	g.GET("/:group/repository.key", h.getRepositoryKey)
	g.HEAD("/:group/repository.key", h.getRepositoryKey)

	g.GET("/:group/repository.repo", h.getRepoConfig)
	g.HEAD("/:group/repository.repo", h.getRepoConfig)

	g.GET("/:group/repodata/:filename", h.getRepoFile)
	g.HEAD("/:group/repodata/:filename", h.getRepoFile)

	g.GET("/:group/package/:name/:version/:architecture/:filename", h.downloadPackage)
	g.HEAD("/:group/package/:name/:version/:architecture/:filename", h.downloadPackage)

	g.PUT("/:group/upload", h.uploadPackage)
	g.DELETE("/:group/package/:name/:version/:architecture", h.deletePackage)
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

// storedFileName encodes (group, arch, basename) into the file row's
// Name. Inverse: parseStoredFileName. Same composite-key-in-name
// pattern Alpine and Debian use.
func storedFileName(group, arch, basename string) string {
	return group + "|" + arch + "|" + basename
}

func parseStoredFileName(name string) (group, arch, basename string, ok bool) {
	parts := strings.SplitN(name, "|", 3)
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// rpmBasename is the on-wire filename `dnf` will request via the
// `<location>` href emitted in primary.xml.
func rpmBasename(name, version, arch string) string {
	return fmt.Sprintf("%s-%s.%s.rpm", name, version, arch)
}

// --- routes: read paths ----------------------------------------------------

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

func (h *Handler) materializeKey(ctx context.Context, tenantID int64) (priv, pub string, err error) {
	key := fmt.Sprintf("%d|rpm-key", tenantID)
	h.keygen.CheckIn(key)
	defer h.keygen.CheckOut(key)
	return GetOrCreateKeyPair(ctx, h.Models, tenantID)
}

func (h *Handler) getRepoConfig(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}
	group := c.Param("group")

	// Reconstruct the baseURL the client used to reach us. We use
	// the Host header + scheme heuristic from r.TLS so the .repo
	// file dnf saves contains an absolute URL it can re-fetch.
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	baseURL := fmt.Sprintf("%s://%s/api/packages/%s/rpm/%s", scheme, c.Request.Host, tenant.Name, group)

	body := BuildRepoConfig(tenant.Name, group, baseURL)
	c.Header("Content-Type", contentTypeRepoFile)
	c.Header("Content-Length", fmt.Sprintf("%d", len(body)))
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	_, _ = c.Writer.Write(body)
}

func (h *Handler) getRepoFile(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}
	group := c.Param("group")
	filename := c.Param("filename")

	bundle, err := h.buildBundle(c.Request.Context(), tenant.ID, group)
	if err != nil {
		c.String(http.StatusInternalServerError, "build metadata: %v", err)
		return
	}
	if bundle == nil {
		c.String(http.StatusNotFound, "no packages in group %q", group)
		return
	}

	var body []byte
	var ctype string
	switch filename {
	case "repomd.xml":
		body, ctype = bundle.Repomd, contentTypeXML
	case "repomd.xml.asc":
		body, ctype = bundle.RepomdAsc, contentTypeSignature
	case "primary.xml.gz":
		body, ctype = bundle.Primary, contentTypeOctet
	case "filelists.xml.gz":
		body, ctype = bundle.Filelists, contentTypeOctet
	case "other.xml.gz":
		body, ctype = bundle.Other, contentTypeOctet
	default:
		c.String(http.StatusNotFound, "unknown repodata file %q", filename)
		return
	}
	c.Header("Content-Type", ctype)
	c.Header("Content-Length", fmt.Sprintf("%d", len(body)))
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	_, _ = c.Writer.Write(body)
}

// buildBundle materializes the full metadata bundle for one group.
// Returns nil if the group has no packages — caller responds 404.
func (h *Handler) buildBundle(ctx context.Context, tenantID int64, group string) (*MetadataBundle, error) {
	entries, releaseTimestamp, err := h.loadEntriesForGroup(ctx, tenantID, group)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	priv, _, err := h.materializeKey(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	return BuildAll(entries, group, releaseTimestamp, priv)
}

// downloadPackage serves the raw .rpm. `:filename` is verified to
// match the canonical `<name>-<version>.<arch>.rpm` shape — primary.xml
// emits exactly that filename and dnf re-requests it.
func (h *Handler) downloadPackage(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	group := c.Param("group")
	name := c.Param("name")
	version := c.Param("version")
	arch := c.Param("architecture")
	filename := c.Param("filename")

	expected := rpmBasename(name, version, arch)
	if filename != expected {
		c.String(http.StatusNotFound, "filename %q does not match canonical %q", filename, expected)
		return
	}

	pkg, ver, file, err := h.lookupFile(c.Request.Context(), tenant.ID, group, arch, name, version)
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

	c.Header("Content-Type", contentTypeRPM)
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filename))
	c.Header("Content-Length", fmt.Sprintf("%d", blob.Size))
	c.Header("Last-Modified", time.Unix(file.CreatedUnix, 0).UTC().Format(http.TimeFormat))
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	_, _ = io.Copy(c.Writer, rc)
}

// --- routes: write paths ---------------------------------------------------

func (h *Handler) uploadPackage(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireWrite(c, tenant) {
		return
	}
	group := strings.TrimSpace(c.Param("group"))
	if group == "" {
		c.String(http.StatusBadRequest, "group is required")
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
		c.String(http.StatusBadRequest, "parse rpm: %v", err)
		return
	}
	if _, err := buf.Seek(0, io.SeekStart); err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}

	basename := rpmBasename(pkg.Name, pkg.Version, pkg.FileMetadata.Architecture)
	if !h.checkIngest(c, tenant, pkg.Name, pkg.Version, basename, pkg.VersionMetadata.License) {
		return
	}

	versionMetaJSON, _ := json.Marshal(pkg.VersionMetadata)
	fileMetaJSON, _ := json.Marshal(pkg.FileMetadata)

	_, ver, file, err := h.Service.CreatePackageOrAddFileToExisting(
		c.Request.Context(),
		pkgsvc.CreationInfo{
			TenantID:            tenant.ID,
			PackageType:         models.TypeRPM,
			PackageName:         pkg.Name,
			PackageLookupName:   strings.ToLower(pkg.Name),
			Version:             pkg.Version,
			VersionMetadataJSON: string(versionMetaJSON),
			Filename:            storedFileName(group, pkg.FileMetadata.Architecture, basename),
			IsLead:              true,
		}, buf)
	if err != nil {
		if errors.Is(err, models.ErrDuplicatePackageFile) {
			c.String(http.StatusConflict, "file %s already exists in group %q", basename, group)
			return
		}
		c.String(http.StatusInternalServerError, "ingest: %v", err)
		return
	}

	// Persist the file-level properties needed for index rebuild.
	ctx := c.Request.Context()
	if err := h.Models.SetProperty(ctx, models.PropertyRefFile, file.ID, PropertyGroup, group); err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	if err := h.Models.SetProperty(ctx, models.PropertyRefFile, file.ID, PropertyArchitecture, pkg.FileMetadata.Architecture); err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	if err := h.Models.SetProperty(ctx, models.PropertyRefFile, file.ID, PropertyMetadata, string(fileMetaJSON)); err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	if pkg.VersionMetadata.License != "" {
		_ = h.Models.SetLicense(ctx, ver.ID, pkg.VersionMetadata.License)
	}

	c.Status(http.StatusCreated)
}

func (h *Handler) deletePackage(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireWrite(c, tenant) {
		return
	}
	group := c.Param("group")
	name := c.Param("name")
	version := c.Param("version")
	arch := c.Param("architecture")

	_, ver, file, err := h.lookupFile(c.Request.Context(), tenant.ID, group, arch, name, version)
	if err != nil {
		c.String(http.StatusNotFound, "%v", err)
		return
	}

	// Last file removes the version (matches every other multi-file
	// format we ship).
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

func (h *Handler) lookupFile(ctx context.Context, tenantID int64, group, arch, name, version string) (*models.Package, *models.Version, *models.File, error) {
	storedName := storedFileName(group, arch, rpmBasename(name, version, arch))
	row := h.Models.DB.QueryRowContext(ctx, `
		SELECT f.id, f.version_id, f.blob_id, f.name, f.is_lead, f.created_unix,
		       v.id, v.package_id, v.version, v.metadata_json, v.created_unix,
		       v.license, v.quarantine_reason, v.quarantined_by_rule_id,
		       p.id, p.tenant_id, p.type, p.name, p.lower_name, p.created_unix
		FROM package_files f
		JOIN package_versions v ON v.id = f.version_id
		JOIN packages p ON p.id = v.package_id
		WHERE p.tenant_id = ? AND p.type = 'rpm' AND f.name = ?
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
			return nil, nil, nil, fmt.Errorf("file %s/%s/%s not found", group, arch, rpmBasename(name, version, arch))
		}
		return nil, nil, nil, err
	}
	return &p, &v, &f, nil
}

// loadEntriesForGroup pulls IndexEntry rows for one group. Returns
// the entries + the deterministic timestamp to stamp into repomd.xml.
// Drains the join cursor before fetching per-row property values,
// per docs/adding-a-format.md "Nested DB query inside an open
// rows.Next() cursor."
func (h *Handler) loadEntriesForGroup(ctx context.Context, tenantID int64, group string) ([]*IndexEntry, int64, error) {
	prefix := group + "|"
	rows, err := h.Models.DB.QueryContext(ctx, `
		SELECT f.id, f.version_id, f.blob_id, f.name, f.is_lead, f.created_unix,
		       p.id, p.name,
		       v.id, v.version, v.metadata_json,
		       b.id, b.size, b.hash_md5, b.hash_sha1, b.hash_sha256, b.hash_sha512
		FROM package_files f
		JOIN package_versions v ON v.id = f.version_id
		JOIN packages p ON p.id = v.package_id
		JOIN package_blobs b ON b.id = f.blob_id
		WHERE p.tenant_id = ? AND p.type = 'rpm' AND p.lower_name != ?
		  AND v.quarantine_reason IS NULL
		  AND f.name LIKE ?
		ORDER BY p.lower_name, v.version, f.name
	`, tenantID, RepositoryPackage, prefix+"%")
	if err != nil {
		return nil, 0, err
	}

	type pending struct {
		fileID int64
		entry  *IndexEntry
	}
	var pendings []pending
	var newestUnix int64
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
			&v.ID, &v.Version, &v.MetadataJSON,
			&b.ID, &b.Size, &b.HashMD5, &b.HashSHA1, &b.HashSHA256, &b.HashSHA512,
		); err != nil {
			rows.Close()
			return nil, 0, err
		}
		if f.CreatedUnix > newestUnix {
			newestUnix = f.CreatedUnix
		}
		entry := &IndexEntry{Pkg: &p, Ver: &v, Blob: &b, File: &f}
		if v.MetadataJSON != "" {
			_ = json.Unmarshal([]byte(v.MetadataJSON), &entry.VerMeta)
		}
		pendings = append(pendings, pending{fileID: f.ID, entry: entry})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, 0, err
	}
	if err := rows.Close(); err != nil {
		return nil, 0, err
	}

	out := make([]*IndexEntry, 0, len(pendings))
	for _, p := range pendings {
		raw, hasMeta, err := h.Models.GetProperty(ctx, models.PropertyRefFile, p.fileID, PropertyMetadata)
		if err != nil {
			return nil, 0, err
		}
		if hasMeta && raw != "" {
			_ = json.Unmarshal([]byte(raw), &p.entry.FileMd)
		}
		out = append(out, p.entry)
	}
	return out, newestUnix, nil
}

// --- policy hooks ----------------------------------------------------------

func (h *Handler) subjectFor(tenant *tenants.Tenant, pkg *models.Package, ver *models.Version, filename string) policy.Subject {
	s := policy.Subject{
		TenantID: tenant.ID,
		Format:   string(models.TypeRPM),
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
		Format:   string(models.TypeRPM),
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
