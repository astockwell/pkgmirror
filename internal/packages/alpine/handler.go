// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2023 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// HTTP routes for the Alpine (apk) package format. The on-wire endpoint
// shape (per-branch/repository/architecture uploads, the
// APKINDEX.tar.gz served at the arch level, the per-owner public-key
// file) is modeled on forgejo/routers/api/packages/alpine/alpine.go
// (MIT). We deviate in one place: forgejo persists the signed
// APKINDEX.tar.gz as a file row and rebuilds on upload/delete; we build
// it on demand because we don't have forgejo's internal-package
// service and the index is small enough that the extra latency is
// negligible.

package alpine

import (
	"context"
	"crypto/sha1"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
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

// Handler is the Alpine registry HTTP handler.
type Handler struct {
	Service *pkgsvc.Service
	Models  *models.Store
	Tenants *tenants.Store
	Engine  policy.Engine

	// RSAKeyBits overrides the bit-length used when generating a
	// per-tenant signing key. Zero means use the package default
	// (4096). Tests pass 2048 to keep things snappy.
	RSAKeyBits int
}

// NewHandler constructs a Handler. If eng is nil the no-op engine is
// used.
func NewHandler(svc *pkgsvc.Service, m *models.Store, ts *tenants.Store, eng policy.Engine) *Handler {
	if eng == nil {
		eng = policy.NoopEngine{}
	}
	return &Handler{Service: svc, Models: m, Tenants: ts, Engine: eng}
}

// Register mounts alpine routes on g (expected to be scoped to
// /api/packages/:tenant/alpine).
//
// apk's wire layout is:
//
//	/<branch>/<repository>/<architecture>/APKINDEX.tar.gz     index
//	/<branch>/<repository>/<architecture>/<filename>.apk      download
//	PUT /<branch>/<repository>                                upload (arch parsed)
//	DELETE /<branch>/<repository>/<architecture>/<filename>   delete
//
// We also expose `/key` so operators can curl the public key and drop
// it in /etc/apk/keys/ on a client. This isn't part of the official
// apk spec but matches forgejo's convention.
func (h *Handler) Register(g *gin.RouterGroup) {
	g.GET("/key", h.getRepositoryKey)
	g.PUT("/:branch/:repository", h.uploadPackageFile)
	g.GET("/:branch/:repository/:architecture/:filename", h.downloadOrIndex)
	g.DELETE("/:branch/:repository/:architecture/:filename", h.deletePackageFile)
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

// storedFileName encodes the (branch, repo, arch, basename) tuple into
// a single file row name. We don't have forgejo's CompositeKey column,
// so embedding the discriminator in the name is how we keep
// UNIQUE(version_id, name) from blocking multi-arch builds of the same
// package version.
func storedFileName(branch, repo, arch, basename string) string {
	return branch + "|" + repo + "|" + arch + "|" + basename
}

// parseStoredFileName is the inverse. Returns ok=false on malformed
// input (which would be a bug — every file we write goes through
// storedFileName).
func parseStoredFileName(name string) (branch, repo, arch, basename string, ok bool) {
	parts := strings.SplitN(name, "|", 4)
	if len(parts) != 4 {
		return "", "", "", "", false
	}
	return parts[0], parts[1], parts[2], parts[3], true
}

// apkBasename returns the canonical on-wire filename for a parsed
// package. apk's repo layout expects `<pkgname>-<pkgver>.apk`.
func apkBasename(pkgName, pkgVersion string) string {
	return fmt.Sprintf("%s-%s.apk", pkgName, pkgVersion)
}

// --- routes ----------------------------------------------------------------

// getRepositoryKey serves the per-tenant RSA public key as a PEM file
// with the `<owner>@<fingerprint>.rsa.pub` filename apk expects in
// /etc/apk/keys/. We name it after the tenant for parity with forgejo.
func (h *Handler) getRepositoryKey(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}

	_, pubPEM, err := GetOrCreateKeyPair(c.Request.Context(), h.Models, tenant.ID, h.RSAKeyBits)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}

	block, _ := pem.Decode([]byte(pubPEM))
	if block == nil {
		c.String(http.StatusInternalServerError, "decode public key pem")
		return
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		c.String(http.StatusInternalServerError, "parse public key: %v", err)
		return
	}
	fingerprintHex, err := publicKeyFingerprintFromPub(pub)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}

	c.Header("Content-Type", "application/x-pem-file")
	c.Header("Content-Disposition",
		fmt.Sprintf(`attachment; filename=%q`, fmt.Sprintf("%s@%s.rsa.pub", strings.ToLower(tenant.Name), fingerprintHex)))
	_, _ = io.WriteString(c.Writer, pubPEM)
}

// downloadOrIndex dispatches `GET .../:filename` between APKINDEX
// serving and ordinary .apk download. APKINDEX is special-cased because
// we build it on demand rather than storing a file row.
func (h *Handler) downloadOrIndex(c *gin.Context) {
	if c.Param("filename") == IndexArchiveFilename {
		h.getRepositoryFile(c)
		return
	}
	h.downloadPackageFile(c)
}

func (h *Handler) getRepositoryFile(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}
	branch := c.Param("branch")
	repository := c.Param("repository")
	architecture := c.Param("architecture")

	entries, err := h.loadIndexEntries(c.Request.Context(), tenant.ID, branch, repository, architecture)
	if err != nil {
		c.String(http.StatusInternalServerError, "load index: %v", err)
		return
	}
	if len(entries) == 0 {
		c.String(http.StatusNotFound, "no packages in %s/%s/%s", branch, repository, architecture)
		return
	}

	privPEM, _, err := GetOrCreateKeyPair(c.Request.Context(), h.Models, tenant.ID, h.RSAKeyBits)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}

	archive, err := BuildIndexArchive(entries, privPEM, strings.ToLower(tenant.Name))
	if err != nil {
		c.String(http.StatusInternalServerError, "build index: %v", err)
		return
	}
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, IndexArchiveFilename))
	c.Header("Content-Length", fmt.Sprintf("%d", len(archive)))
	_, _ = c.Writer.Write(archive)
}

// uploadPackageFile ingests one .apk. apk-tools doesn't actually push
// (alpine has no upload protocol), but `abuild` and CI publishers do
// over HTTP PUT — we accept the raw `.apk` body and use the parsed
// PKGINFO's arch field to decide where it lives.
func (h *Handler) uploadPackageFile(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireWrite(c, tenant) {
		return
	}
	branch := strings.TrimSpace(c.Param("branch"))
	repository := strings.TrimSpace(c.Param("repository"))
	if branch == "" || repository == "" {
		c.String(http.StatusBadRequest, "branch and repository are required")
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
		if errors.Is(err, ErrMissingPKGINFOFile) ||
			errors.Is(err, ErrInvalidName) ||
			errors.Is(err, ErrInvalidVersion) {
			c.String(http.StatusBadRequest, "%v", err)
			return
		}
		c.String(http.StatusBadRequest, "parse apk: %v", err)
		return
	}
	if _, err := buf.Seek(0, io.SeekStart); err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}

	// noarch fan-out: when the apk advertises arch=noarch the
	// canonical interpretation (per apk's repo layout) is "publish
	// in every architecture this repo carries". Match that here.
	// Falls back to a single x86_64 file if the repo is empty.
	arches := []string{pkg.FileMetadata.Architecture}
	if pkg.FileMetadata.Architecture == ArchNoArch {
		existing, err := h.distinctArchitectures(c.Request.Context(), tenant.ID, branch, repository)
		if err != nil {
			c.String(http.StatusInternalServerError, "%v", err)
			return
		}
		if len(existing) == 0 {
			arches = []string{DefaultArchitecture}
		} else {
			arches = existing
		}
	}

	for _, arch := range arches {
		// Refresh the metadata blob per arch so the persisted JSON
		// matches the arch we're writing.
		fm := pkg.FileMetadata
		fm.Architecture = arch
		fmBytes, err := json.Marshal(fm)
		if err != nil {
			c.String(http.StatusInternalServerError, "%v", err)
			return
		}

		basename := apkBasename(pkg.Name, pkg.Version)
		if !h.checkIngest(c, tenant, pkg.Name, pkg.Version, basename, pkg.VersionMetadata.License) {
			return
		}

		if _, err := buf.Seek(0, io.SeekStart); err != nil {
			c.String(http.StatusInternalServerError, "%v", err)
			return
		}

		versionMetaJSON, err := json.Marshal(pkg.VersionMetadata)
		if err != nil {
			c.String(http.StatusInternalServerError, "%v", err)
			return
		}

		_, ver, file, err := h.Service.CreatePackageOrAddFileToExisting(
			c.Request.Context(),
			pkgsvc.CreationInfo{
				TenantID:            tenant.ID,
				PackageType:         models.TypeAlpine,
				PackageName:         pkg.Name,
				PackageLookupName:   strings.ToLower(pkg.Name),
				Version:             pkg.Version,
				VersionMetadataJSON: string(versionMetaJSON),
				Filename:            storedFileName(branch, repository, arch, basename),
				IsLead:              true,
			}, buf)
		if err != nil {
			if errors.Is(err, models.ErrDuplicatePackageFile) {
				c.String(http.StatusConflict, "file %s already exists in %s/%s/%s", basename, branch, repository, arch)
				return
			}
			c.String(http.StatusInternalServerError, "ingest: %v", err)
			return
		}

		// Attach the file-level properties forgejo uses, plus a
		// json-encoded FileMetadata so the index builder can recreate
		// the APKINDEX without re-reading the .apk blob.
		ctx := c.Request.Context()
		if err := h.Models.SetProperty(ctx, models.PropertyRefFile, file.ID, PropertyBranch, branch); err != nil {
			c.String(http.StatusInternalServerError, "%v", err)
			return
		}
		if err := h.Models.SetProperty(ctx, models.PropertyRefFile, file.ID, PropertyRepository, repository); err != nil {
			c.String(http.StatusInternalServerError, "%v", err)
			return
		}
		if err := h.Models.SetProperty(ctx, models.PropertyRefFile, file.ID, PropertyArchitecture, arch); err != nil {
			c.String(http.StatusInternalServerError, "%v", err)
			return
		}
		if err := h.Models.SetProperty(ctx, models.PropertyRefFile, file.ID, PropertyMetadata, string(fmBytes)); err != nil {
			c.String(http.StatusInternalServerError, "%v", err)
			return
		}
		if pkg.VersionMetadata.License != "" {
			_ = h.Models.SetLicense(ctx, ver.ID, pkg.VersionMetadata.License)
		}
	}
	c.Status(http.StatusCreated)
}

func (h *Handler) downloadPackageFile(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	branch := c.Param("branch")
	repository := c.Param("repository")
	architecture := c.Param("architecture")
	filename := c.Param("filename")
	if !strings.HasSuffix(filename, ".apk") {
		c.String(http.StatusNotFound, "not found")
		return
	}

	pkg, ver, file, err := h.lookupFile(c.Request.Context(), tenant.ID, branch, repository, architecture, filename)
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

	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filename))
	if blob != nil {
		c.Header("Content-Length", fmt.Sprintf("%d", blob.Size))
	}
	_, _ = io.Copy(c.Writer, rc)
}

func (h *Handler) deletePackageFile(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireWrite(c, tenant) {
		return
	}
	branch := c.Param("branch")
	repository := c.Param("repository")
	architecture := c.Param("architecture")
	filename := c.Param("filename")
	if !strings.HasSuffix(filename, ".apk") {
		c.String(http.StatusNotFound, "not found")
		return
	}

	_, ver, file, err := h.lookupFile(c.Request.Context(), tenant.ID, branch, repository, architecture, filename)
	if err != nil {
		c.String(http.StatusNotFound, "%v", err)
		return
	}

	// Match generic's "last file removes the version" semantics so
	// we don't accumulate orphan version rows.
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

// lookupFile finds the (package, version, file) row for one
// (branch, repo, arch, filename) coordinate.
func (h *Handler) lookupFile(ctx context.Context, tenantID int64, branch, repo, arch, basename string) (*models.Package, *models.Version, *models.File, error) {
	name := storedFileName(branch, repo, arch, basename)

	row := h.Models.DB.QueryRowContext(ctx, `
		SELECT f.id, f.version_id, f.blob_id, f.name, f.is_lead, f.created_unix,
		       v.id, v.package_id, v.version, v.metadata_json, v.created_unix,
		       v.license, v.quarantine_reason, v.quarantined_by_rule_id,
		       p.id, p.tenant_id, p.type, p.name, p.lower_name, p.created_unix
		FROM package_files f
		JOIN package_versions v ON v.id = f.version_id
		JOIN packages p ON p.id = v.package_id
		WHERE p.tenant_id = ? AND p.type = 'alpine' AND f.name = ?
	`, tenantID, name)

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
			return nil, nil, nil, fmt.Errorf("file %s not found in %s/%s/%s", basename, branch, repo, arch)
		}
		return nil, nil, nil, err
	}
	return &p, &v, &f, nil
}

// loadIndexEntries gathers the index input rows for one coordinate.
// Returns nil (not error) when the repo is empty so callers can
// distinguish "no signing required" from a true DB failure.
func (h *Handler) loadIndexEntries(ctx context.Context, tenantID int64, branch, repo, arch string) ([]*indexEntry, error) {
	prefix := branch + "|" + repo + "|" + arch + "|"
	rows, err := h.Models.DB.QueryContext(ctx, `
		SELECT f.id, f.name,
		       p.id, p.name,
		       v.id, v.version, v.metadata_json,
		       b.id, b.size, b.hash_md5, b.hash_sha1, b.hash_sha256, b.hash_sha512
		FROM package_files f
		JOIN package_versions v ON v.id = f.version_id
		JOIN packages p ON p.id = v.package_id
		JOIN package_blobs b ON b.id = f.blob_id
		WHERE p.tenant_id = ? AND p.type = 'alpine' AND p.lower_name != ?
		  AND v.quarantine_reason IS NULL
		  AND f.name LIKE ?
		ORDER BY p.lower_name, v.version, f.name
	`, tenantID, RepositoryPackage, prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*indexEntry
	for rows.Next() {
		var (
			f models.File
			p models.Package
			v models.Version
			b models.Blob
		)
		if err := rows.Scan(
			&f.ID, &f.Name,
			&p.ID, &p.Name,
			&v.ID, &v.Version, &v.MetadataJSON,
			&b.ID, &b.Size, &b.HashMD5, &b.HashSHA1, &b.HashSHA256, &b.HashSHA512,
		); err != nil {
			return nil, err
		}

		entry := &indexEntry{
			pkg:  &p,
			ver:  &v,
			blob: &b,
		}
		if v.MetadataJSON != "" {
			_ = json.Unmarshal([]byte(v.MetadataJSON), &entry.verMeta)
		}

		raw, hasMeta, err := h.Models.GetProperty(ctx, models.PropertyRefFile, f.ID, PropertyMetadata)
		if err != nil {
			return nil, err
		}
		if hasMeta && raw != "" {
			_ = json.Unmarshal([]byte(raw), &entry.fileMd)
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// distinctArchitectures returns every architecture currently published
// to (tenant, branch, repository). Used to fan out noarch uploads.
func (h *Handler) distinctArchitectures(ctx context.Context, tenantID int64, branch, repo string) ([]string, error) {
	prefix := branch + "|" + repo + "|"
	rows, err := h.Models.DB.QueryContext(ctx, `
		SELECT DISTINCT f.name
		FROM package_files f
		JOIN package_versions v ON v.id = f.version_id
		JOIN packages p ON p.id = v.package_id
		WHERE p.tenant_id = ? AND p.type = 'alpine' AND p.lower_name != ?
		  AND f.name LIKE ?
	`, tenantID, RepositoryPackage, prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	seen := map[string]struct{}{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		_, _, arch, _, ok := parseStoredFileName(name)
		if !ok {
			continue
		}
		seen[arch] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Strings(out)
	return out, nil
}

// --- policy hooks ----------------------------------------------------------

func (h *Handler) subjectFor(tenant *tenants.Tenant, pkg *models.Package, ver *models.Version, filename string) policy.Subject {
	s := policy.Subject{
		TenantID: tenant.ID,
		Format:   string(models.TypeAlpine),
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
		Format:   string(models.TypeAlpine),
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

// publicKeyFingerprintFromPub is the *rsa.PublicKey form used by the
// key endpoint, kept separate from publicKeyFingerprint (which takes
// *rsa.PublicKey directly via package-internal call) so we can detect
// non-RSA keys at the http layer.
func publicKeyFingerprintFromPub(pub any) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	// Same SHA1 fingerprint as publicKeyFingerprint.
	s := sha1.Sum(der)
	return hex.EncodeToString(s[:]), nil
}
