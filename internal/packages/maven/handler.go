// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2021 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// HTTP routes for the Maven repository format. The wire shape (path
// layout `<groupId-as-path>/<artifactId>/<version>/<filename>`,
// per-artifact checksum sidecar files at .md5/.sha1/.sha256/.sha512,
// generated maven-metadata.xml at the per-(group,artifact) level, the
// pom-triggers-metadata-extraction upload flow) is modeled on
// forgejo/routers/api/packages/maven/maven.go (MIT). We deviate only in
// the per-package locking story: Forgejo uses a global ExclusivePool;
// we serialize per-package writes via a sync.Map of mutexes scoped to
// the handler.

package maven

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
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

const (
	mavenMetadataFile = "maven-metadata.xml"
	extensionMD5      = ".md5"
	extensionSHA1     = ".sha1"
	extensionSHA256   = ".sha256"
	extensionSHA512   = ".sha512"
	extensionPom      = ".pom"
	extensionJar      = ".jar"
	contentTypeJar    = "application/java-archive"
	contentTypeXML    = "text/xml"
)

// illegalCharacters is the same character class Forgejo and other
// Maven implementations use to reject ambiguous path traversal.
var illegalCharacters = regexp.MustCompile(`[\\/:"<>|?\*]`)

// errInvalidParameters is returned when the catch-all path doesn't
// match the expected `groupId.../artifactId/version/filename` shape.
var errInvalidParameters = errors.New("maven: request path parameters are invalid")

// Handler is the Maven registry HTTP handler.
type Handler struct {
	Service *pkgsvc.Service
	Models  *models.Store
	Tenants *tenants.Store
	Engine  policy.Engine

	// uploads serializes write traffic per
	// (tenant, groupId:artifactId) so concurrent PUTs of jar +
	// pom + sources.jar against the same coordinate don't race on
	// version row creation. Forgejo's ExclusivePool — refcount-
	// driven map of mutexes — frees map entries when the last
	// holder checks out, so memory stays bounded by *concurrent*
	// uploads rather than *unique coordinates ever seen*. See
	// internal/syncutil for details.
	uploads *syncutil.ExclusivePool
}

// NewHandler constructs a Handler. If eng is nil the no-op engine is
// used.
func NewHandler(svc *pkgsvc.Service, m *models.Store, ts *tenants.Store, eng policy.Engine) *Handler {
	if eng == nil {
		eng = policy.NoopEngine{}
	}
	return &Handler{Service: svc, Models: m, Tenants: ts, Engine: eng, uploads: syncutil.NewExclusivePool()}
}

// Register mounts maven routes on g (expected to be scoped to
// /api/packages/:tenant/maven). The catch-all `*path` captures the
// hierarchical groupId/artifactId/version/filename URL Maven clients
// build from a repo root + GAV coordinate.
//
//	GET    /*path   download artifact, checksum sidecar, or generated maven-metadata.xml
//	HEAD   /*path   existence check (Gradle caches use this aggressively)
//	PUT    /*path   upload artifact, verify checksum, or accept-and-discard a client-pushed maven-metadata.xml
func (h *Handler) Register(g *gin.RouterGroup) {
	g.GET("/*path", h.handleDownload)
	g.HEAD("/*path", h.handleHead)
	g.PUT("/*path", h.handleUpload)
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

// parameters is the decoded path. IsMeta means the filename is
// maven-metadata.xml (possibly with a checksum suffix).
type parameters struct {
	GroupID    string
	ArtifactID string
	Version    string
	Filename   string
	IsMeta     bool
}

// extractPathParameters parses the catch-all path into a parameters
// struct. Ported from forgejo/routers/api/packages/maven/maven.go's
// extractPathParameters with one adjustment: gin's catch-all params
// start with a leading `/`, which we strip.
func extractPathParameters(rawPath string) (parameters, error) {
	rawPath = strings.TrimPrefix(rawPath, "/")
	parts := strings.Split(rawPath, "/")
	if len(parts) == 0 || parts[0] == "" {
		return parameters{}, errInvalidParameters
	}

	p := parameters{
		Filename: parts[len(parts)-1],
	}

	p.IsMeta = p.Filename == mavenMetadataFile ||
		p.Filename == mavenMetadataFile+extensionMD5 ||
		p.Filename == mavenMetadataFile+extensionSHA1 ||
		p.Filename == mavenMetadataFile+extensionSHA256 ||
		p.Filename == mavenMetadataFile+extensionSHA512

	parts = parts[:len(parts)-1]
	if len(parts) == 0 {
		return p, errInvalidParameters
	}

	// Forgejo's quirk: the per-version maven-metadata.xml only
	// applies to SNAPSHOT versions (it pins build numbers). For
	// release versions, the maven-metadata.xml lives directly
	// under /<groupId>/<artifactId>/ (no version directory). Detect
	// that by checking whether the would-be-version segment looks
	// like a SNAPSHOT.
	p.Version = parts[len(parts)-1]
	if p.IsMeta && !strings.HasSuffix(p.Version, "-SNAPSHOT") {
		p.Version = ""
	} else {
		parts = parts[:len(parts)-1]
	}

	if illegalCharacters.MatchString(p.Version) {
		return p, errInvalidParameters
	}

	if len(parts) < 2 {
		return p, errInvalidParameters
	}

	p.ArtifactID = parts[len(parts)-1]
	p.GroupID = strings.Join(parts[:len(parts)-1], ".")

	if illegalCharacters.MatchString(p.GroupID) || illegalCharacters.MatchString(p.ArtifactID) {
		return p, errInvalidParameters
	}

	return p, nil
}

// buildPackageID is the Maven-coordinate-style identifier we use as
// the package name. See https://maven.apache.org/pom.html#Maven_Coordinates.
func buildPackageID(groupID, artifactID string) string {
	return groupID + ":" + artifactID
}

func isChecksumExtension(ext string) bool {
	return ext == extensionMD5 || ext == extensionSHA1 || ext == extensionSHA256 || ext == extensionSHA512
}

// --- routes ----------------------------------------------------------------

func (h *Handler) handleDownload(c *gin.Context) {
	h.handlePackageFile(c, true)
}

func (h *Handler) handleHead(c *gin.Context) {
	h.handlePackageFile(c, false)
}

// handlePackageFile is the GET/HEAD dispatcher. The serveContent flag
// flips between sending the body (GET) vs only the headers (HEAD).
func (h *Handler) handlePackageFile(c *gin.Context, serveContent bool) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}

	params, err := extractPathParameters(c.Param("path"))
	if err != nil {
		c.String(http.StatusBadRequest, "%v", err)
		return
	}

	if params.IsMeta && params.Version == "" {
		h.serveMavenMetadata(c, tenant, params, serveContent)
		return
	}
	h.servePackageFile(c, tenant, params, serveContent)
}

// serveMavenMetadata generates and returns the per-(group,artifact)
// maven-metadata.xml. We rebuild on every request rather than caching;
// the XML is small and querying versions is cheap. Sidecar checksum
// files (.md5/.sha1/.sha256/.sha512) hash the generated XML in place.
func (h *Handler) serveMavenMetadata(c *gin.Context, tenant *tenants.Tenant, params parameters, serveContent bool) {
	packageName := buildPackageID(params.GroupID, params.ArtifactID)
	pkg, err := h.Models.GetPackageByLookup(c.Request.Context(), tenant.ID, models.TypeMaven, strings.ToLower(packageName))
	if err != nil {
		c.String(http.StatusNotFound, "package %s not found", packageName)
		return
	}
	versions, err := h.Models.ListVersions(c.Request.Context(), pkg.ID)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	if len(versions) == 0 {
		c.String(http.StatusNotFound, "no versions for %s", packageName)
		return
	}
	// Drop quarantined versions from the public index so policy
	// decisions actually hide the artifacts from Maven clients.
	visible := versions[:0]
	for _, v := range versions {
		if !v.IsQuarantined() {
			visible = append(visible, v)
		}
	}
	if len(visible) == 0 {
		c.String(http.StatusNotFound, "no readable versions for %s", packageName)
		return
	}
	// ListVersions returns ascending by created_unix already.
	xmlBody := buildMavenMetadataXML(params.GroupID, params.ArtifactID, visible)

	ext := strings.ToLower(filepath.Ext(params.Filename))
	if isChecksumExtension(ext) {
		hash := hashChecksum(ext, xmlBody)
		c.Header("Content-Type", "text/plain; charset=utf-8")
		c.String(http.StatusOK, hash)
		return
	}

	c.Header("Content-Type", contentTypeXML)
	c.Header("Content-Length", fmt.Sprintf("%d", len(xmlBody)))
	c.Header("Last-Modified", time.Unix(visible[len(visible)-1].CreatedUnix, 0).UTC().Format(http.TimeFormat))
	if !serveContent {
		c.Status(http.StatusOK)
		return
	}
	_, _ = c.Writer.Write(xmlBody)
}

// servePackageFile downloads a single artifact file or its checksum
// sidecar. Checksum requests synthesize from the blob's stored hash;
// we don't store separate .md5 / .sha1 / etc files.
func (h *Handler) servePackageFile(c *gin.Context, tenant *tenants.Tenant, params parameters, serveContent bool) {
	packageName := buildPackageID(params.GroupID, params.ArtifactID)
	pkg, err := h.Models.GetPackageByLookup(c.Request.Context(), tenant.ID, models.TypeMaven, strings.ToLower(packageName))
	if err != nil {
		c.String(http.StatusNotFound, "package %s not found", packageName)
		return
	}
	ver, err := h.Models.GetVersion(c.Request.Context(), pkg.ID, params.Version)
	if err != nil {
		c.String(http.StatusNotFound, "version %s@%s not found", packageName, params.Version)
		return
	}
	if ver.IsQuarantined() {
		c.String(http.StatusNotFound, "version %s@%s not found", packageName, params.Version)
		return
	}
	if !h.checkRead(c, tenant, pkg, ver, params.Filename) {
		return
	}

	filename := params.Filename
	ext := strings.ToLower(filepath.Ext(filename))
	if isChecksumExtension(ext) {
		filename = filename[:len(filename)-len(ext)]
	}

	file, err := h.Models.GetFileByVersionAndName(c.Request.Context(), ver.ID, filename)
	if err != nil {
		c.String(http.StatusNotFound, "file %s not found", filename)
		return
	}

	rc, blob, err := h.Service.OpenFile(c.Request.Context(), file)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	defer rc.Close()

	if isChecksumExtension(ext) {
		var hash string
		switch ext {
		case extensionMD5:
			hash = blob.HashMD5
		case extensionSHA1:
			hash = blob.HashSHA1
		case extensionSHA256:
			hash = blob.HashSHA256
		case extensionSHA512:
			hash = blob.HashSHA512
		}
		c.Header("Content-Type", "text/plain; charset=utf-8")
		c.String(http.StatusOK, hash)
		return
	}

	switch ext {
	case extensionJar:
		c.Header("Content-Type", contentTypeJar)
	case extensionPom:
		c.Header("Content-Type", contentTypeXML)
	default:
		c.Header("Content-Type", "application/octet-stream")
	}
	c.Header("Content-Length", fmt.Sprintf("%d", blob.Size))
	c.Header("Last-Modified", time.Unix(file.CreatedUnix, 0).UTC().Format(http.TimeFormat))
	if !serveContent {
		c.Status(http.StatusOK)
		return
	}
	_, _ = io.Copy(c.Writer, rc)
}

// handleUpload implements PUT /*path. Maven clients PUT one file at a
// time and assume any required parent directories already exist; we
// gate on params validity, then dispatch based on the file extension
// (pom triggers metadata extraction, checksum sidecars are verified
// but not stored, anything else is a normal blob upload).
func (h *Handler) handleUpload(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireWrite(c, tenant) {
		return
	}

	params, err := extractPathParameters(c.Param("path"))
	if err != nil {
		c.String(http.StatusBadRequest, "%v", err)
		return
	}

	// Client-pushed maven-metadata.xml at the per-(group,artifact)
	// level is ignored — we regenerate it on demand. Returning 200
	// rather than 4xx keeps `mvn deploy` and Gradle's publish task
	// happy. (A per-version SNAPSHOT maven-metadata.xml IS a real
	// file we want to persist; that's the `params.Version != ""`
	// branch falling through.)
	if params.IsMeta && params.Version == "" {
		c.Status(http.StatusOK)
		return
	}

	packageName := buildPackageID(params.GroupID, params.ArtifactID)

	lockKey := fmt.Sprintf("%d|%s", tenant.ID, packageName)
	h.uploads.CheckIn(lockKey)
	defer h.uploads.CheckOut(lockKey)

	buf, err := h.Service.NewHashedBuffer(c.Request.Body)
	if err != nil {
		c.String(http.StatusInternalServerError, "buffer: %v", err)
		return
	}
	defer buf.Close()

	ext := strings.ToLower(filepath.Ext(params.Filename))

	// Checksum sidecar uploads: verify that the supplied checksum
	// matches the corresponding hash we stored on the lead file.
	// Maven and Gradle PUT these sidecars after the artifact itself
	// as a poor-man's integrity check; rejecting a mismatch here
	// surfaces upload corruption immediately.
	if isChecksumExtension(ext) {
		if err := h.verifyChecksumUpload(c, tenant, packageName, params, ext, buf); err != nil {
			// verifyChecksumUpload already wrote the response.
			_ = err
			return
		}
		c.Status(http.StatusOK)
		return
	}

	if _, err := buf.Seek(0, io.SeekStart); err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}

	// .pom carries the canonical metadata; parse it before we
	// commit so we can reject malformed POMs with a 400 and not
	// orphan a blob.
	var (
		meta        *Metadata
		metaJSON    string
		licenseStr  string
		isPomUpload = ext == extensionPom
	)
	if isPomUpload {
		meta, err = ParsePackageMetaData(buf)
		if err != nil {
			if errors.Is(err, ErrNoGroupID) {
				c.String(http.StatusBadRequest, "%v", err)
				return
			}
			c.String(http.StatusBadRequest, "parse pom: %v", err)
			return
		}
		if b, mErr := json.Marshal(meta); mErr == nil {
			metaJSON = string(b)
		}
		if len(meta.Licenses) > 0 {
			licenseStr = meta.Licenses[0]
		}
		if _, err := buf.Seek(0, io.SeekStart); err != nil {
			c.String(http.StatusInternalServerError, "%v", err)
			return
		}
	}

	if !h.checkIngest(c, tenant, packageName, params.Version, params.Filename, licenseStr) {
		return
	}

	_, ver, _, err := h.Service.CreatePackageOrAddFileToExisting(
		c.Request.Context(),
		pkgsvc.CreationInfo{
			TenantID:            tenant.ID,
			PackageType:         models.TypeMaven,
			PackageName:         packageName,
			PackageLookupName:   strings.ToLower(packageName),
			Version:             params.Version,
			VersionMetadataJSON: metaJSON,
			Filename:            params.Filename,
			IsLead:              isPomUpload,
		}, buf)
	if err != nil {
		if errors.Is(err, models.ErrDuplicatePackageFile) {
			c.String(http.StatusConflict, "file %s already exists for %s@%s", params.Filename, packageName, params.Version)
			return
		}
		c.String(http.StatusInternalServerError, "ingest: %v", err)
		return
	}

	// The pom may arrive after a sibling jar already created the
	// version row with empty metadata. Backfill it now.
	if isPomUpload && metaJSON != "" {
		if err := h.Models.UpdateVersionMetadata(c.Request.Context(), ver.ID, metaJSON); err != nil {
			c.String(http.StatusInternalServerError, "%v", err)
			return
		}
		if licenseStr != "" {
			_ = h.Models.SetLicense(c.Request.Context(), ver.ID, licenseStr)
		}
	}

	c.Status(http.StatusCreated)
}

// verifyChecksumUpload reads the checksum value from buf and matches
// it against the lead file's stored hash. On mismatch / missing
// file we return the right HTTP status; on success we return nil and
// the caller writes 200.
func (h *Handler) verifyChecksumUpload(
	c *gin.Context,
	tenant *tenants.Tenant,
	packageName string,
	params parameters,
	ext string,
	buf io.ReadSeeker,
) error {
	if _, err := buf.Seek(0, io.SeekStart); err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return err
	}
	hash, err := io.ReadAll(buf)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return err
	}
	// Maven CLIs write checksum files as plain hex with no
	// trailing newline, but some implementations append one.
	hashStr := strings.TrimSpace(string(hash))

	pkg, err := h.Models.GetPackageByLookup(c.Request.Context(), tenant.ID, models.TypeMaven, strings.ToLower(packageName))
	if err != nil {
		c.String(http.StatusNotFound, "package %s not found", packageName)
		return err
	}
	ver, err := h.Models.GetVersion(c.Request.Context(), pkg.ID, params.Version)
	if err != nil {
		c.String(http.StatusNotFound, "version %s@%s not found", packageName, params.Version)
		return err
	}
	baseName := params.Filename[:len(params.Filename)-len(ext)]
	file, err := h.Models.GetFileByVersionAndName(c.Request.Context(), ver.ID, baseName)
	if err != nil {
		c.String(http.StatusNotFound, "file %s not found", baseName)
		return err
	}
	_, blob, err := h.Service.OpenFile(c.Request.Context(), file)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return err
	}
	var want string
	switch ext {
	case extensionMD5:
		want = blob.HashMD5
	case extensionSHA1:
		want = blob.HashSHA1
	case extensionSHA256:
		want = blob.HashSHA256
	case extensionSHA512:
		want = blob.HashSHA512
	}
	if !strings.EqualFold(want, hashStr) {
		c.String(http.StatusBadRequest, "checksum mismatch: want %s, got %s", want, hashStr)
		return errors.New("checksum mismatch")
	}
	return nil
}

// --- maven-metadata.xml generation -----------------------------------------

// mavenMetadataXML is the on-wire shape Maven and Gradle clients
// expect at <groupId>/<artifactId>/maven-metadata.xml. The element
// order matters — `versioning>release` must precede `versioning>latest`
// or older Maven 3.x parsers warn (and have historically rejected).
// We mirror Forgejo's element layout exactly.
type mavenMetadataXML struct {
	XMLName    xml.Name `xml:"metadata"`
	GroupID    string   `xml:"groupId"`
	ArtifactID string   `xml:"artifactId"`
	Release    string   `xml:"versioning>release,omitempty"`
	Latest     string   `xml:"versioning>latest"`
	Version    []string `xml:"versioning>versions>version"`
}

func buildMavenMetadataXML(groupID, artifactID string, versions []*models.Version) []byte {
	if len(versions) == 0 {
		return nil
	}
	sort.Slice(versions, func(i, j int) bool {
		// Maven and Gradle order versions by their *creation
		// timestamp* in the registry, not by semver — see the
		// Maven Repository Metadata Reference. This matters for
		// the `latest` element when sequential 1.0.0-beta + 1.0.0
		// releases land in mixed order.
		return versions[i].CreatedUnix < versions[j].CreatedUnix
	})
	resp := &mavenMetadataXML{
		GroupID:    groupID,
		ArtifactID: artifactID,
		Latest:     versions[len(versions)-1].Version,
	}
	resp.Version = make([]string, 0, len(versions))
	var lastRelease string
	for _, v := range versions {
		resp.Version = append(resp.Version, v.Version)
		if !strings.HasSuffix(v.Version, "-SNAPSHOT") {
			lastRelease = v.Version
		}
	}
	resp.Release = lastRelease

	body, _ := xml.Marshal(resp)
	out := make([]byte, 0, len(xml.Header)+len(body))
	out = append(out, []byte(xml.Header)...)
	out = append(out, body...)
	return out
}

func hashChecksum(ext string, body []byte) string {
	switch ext {
	case extensionMD5:
		h := md5.Sum(body)
		return hex.EncodeToString(h[:])
	case extensionSHA1:
		h := sha1.Sum(body)
		return hex.EncodeToString(h[:])
	case extensionSHA256:
		h := sha256.Sum256(body)
		return hex.EncodeToString(h[:])
	case extensionSHA512:
		h := sha512.Sum512(body)
		return hex.EncodeToString(h[:])
	}
	return ""
}

// --- policy hooks ----------------------------------------------------------

func (h *Handler) subjectFor(tenant *tenants.Tenant, pkg *models.Package, ver *models.Version, filename string) policy.Subject {
	s := policy.Subject{
		TenantID: tenant.ID,
		Format:   string(models.TypeMaven),
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
		Format:   string(models.TypeMaven),
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
