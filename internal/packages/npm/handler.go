// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2021 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// The HTTP endpoint shape (packument GET / publish PUT / tarball GET /
// dist-tags CRUD) and the packument response shape are modeled on
// forgejo/routers/api/packages/npm/{npm,api}.go (MIT).

package npm

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/astockwell/pkgmirror/internal/auth"
	"github.com/astockwell/pkgmirror/internal/models"
	pkgsvc "github.com/astockwell/pkgmirror/internal/packages"
	"github.com/astockwell/pkgmirror/internal/policy"
	"github.com/astockwell/pkgmirror/internal/tenants"

	"github.com/gin-gonic/gin"
	semver "github.com/hashicorp/go-version"
)

// Handler is the npm registry HTTP handler.
type Handler struct {
	Service *pkgsvc.Service
	Models  *models.Store
	Tenants *tenants.Store
	Engine  policy.Engine
}

// NewHandler constructs a Handler. If eng is nil the no-op engine is used.
func NewHandler(svc *pkgsvc.Service, m *models.Store, ts *tenants.Store, eng policy.Engine) *Handler {
	if eng == nil {
		eng = policy.NoopEngine{}
	}
	return &Handler{Service: svc, Models: m, Tenants: ts, Engine: eng}
}

// Register mounts npm routes on g (expected to be scoped to
// /api/packages/:tenant/npm).
//
// We support both scoped (@org/name) and unscoped names. Gin lets us
// express the scoped variant by literal-prefixing the route with "@".
// Some clients URL-encode the slash inside scoped names (%2F); Gin does
// NOT auto-decode that, so for scoped names we accept the two-segment
// form (@:scope/:id) only. The npm CLI uses the two-segment form, so
// this is fine in practice.
//
//	PUT  /:name                                  publish (unscoped)
//	PUT  /@:scope/:name                          publish (scoped)
//	GET  /:name                                  packument (unscoped)
//	GET  /@:scope/:name                          packument (scoped)
//	GET  /:name/-/:filename                      tarball
//	GET  /@:scope/:name/-/:filename              tarball (scoped)
//	GET  /-/package/:name/dist-tags              list dist-tags
//	GET  /-/package/@:scope/:name/dist-tags      list dist-tags (scoped)
//	PUT  /-/package/:name/dist-tags/:tag         set dist-tag (body = "version")
//	DELETE /-/package/:name/dist-tags/:tag       remove dist-tag
//	(scoped variants identical)
func (h *Handler) Register(g *gin.RouterGroup) {
	// Dist-tag routes (more specific) first so they don't collide with
	// /:name patterns.
	g.GET("/-/package/:name/dist-tags", h.listDistTags)
	g.GET("/-/package/@:scope/:name/dist-tags", h.listDistTags)
	g.PUT("/-/package/:name/dist-tags/:tag", h.setDistTag)
	g.PUT("/-/package/@:scope/:name/dist-tags/:tag", h.setDistTag)
	g.DELETE("/-/package/:name/dist-tags/:tag", h.deleteDistTag)
	g.DELETE("/-/package/@:scope/:name/dist-tags/:tag", h.deleteDistTag)

	// Tarballs.
	g.GET("/:name/-/:filename", h.downloadTarball)
	g.GET("/@:scope/:name/-/:filename", h.downloadTarball)

	// Packument GET + publish PUT. These are the bottom-of-the-pile catch-alls.
	g.GET("/:name", h.packument)
	g.GET("/@:scope/:name", h.packument)
	g.PUT("/:name", h.publish)
	g.PUT("/@:scope/:name", h.publish)
}

// --- routing helpers -------------------------------------------------------

func (h *Handler) tenantFromPath(c *gin.Context) *tenants.Tenant {
	name := c.Param("tenant")
	t, err := h.Tenants.GetByName(c.Request.Context(), name)
	if err != nil {
		if errors.Is(err, tenants.ErrNotExist) {
			c.JSON(http.StatusNotFound, errorJSON("tenant %q not found", name))
		} else {
			c.JSON(http.StatusInternalServerError, errorJSON("%v", err))
		}
		return nil
	}
	return t
}

// packageNameFromParams reassembles the package name from the gin path
// parameters. Scoped names come in as (scope = "acme", name = "foo")
// and are reassembled to "@acme/foo".
func packageNameFromParams(c *gin.Context) string {
	scope := c.Param("scope")
	name := c.Param("name")
	if scope != "" {
		return "@" + scope + "/" + name
	}
	return name
}

// registryURL returns the absolute registry URL the client should use for
// follow-up requests (tarball download links in packuments).
func registryURL(c *gin.Context, tenantName string) string {
	scheme := "http"
	if c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := c.Request.Host
	if h := c.GetHeader("X-Forwarded-Host"); h != "" {
		host = h
	}
	return fmt.Sprintf("%s://%s/api/packages/%s/npm", scheme, host, tenantName)
}

func errorJSON(format string, args ...any) gin.H {
	return gin.H{"error": fmt.Sprintf(format, args...)}
}

// --- handlers --------------------------------------------------------------

// publish handles `npm publish` — a single JSON document with one version
// and a base64-encoded tarball.
func (h *Handler) publish(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireWrite(c, tenant) {
		return
	}

	pkg, err := ParsePackage(c.Request.Body)
	if err != nil {
		if errors.Is(err, ErrInvalidPackage) ||
			errors.Is(err, ErrInvalidPackageName) ||
			errors.Is(err, ErrInvalidPackageVersion) ||
			errors.Is(err, ErrInvalidAttachment) ||
			errors.Is(err, ErrInvalidIntegrity) {
			c.JSON(http.StatusBadRequest, errorJSON("%v", err))
			return
		}
		c.JSON(http.StatusBadRequest, errorJSON("parse publish: %v", err))
		return
	}

	if !h.checkIngest(c, tenant, pkg.Name, pkg.Version, pkg.Filename, pkg.Metadata.License) {
		return
	}

	metaJSON, _ := json.Marshal(pkg.Metadata)
	buf, err := pkgsvc.NewHashedBufferFromReader(bytes.NewReader(pkg.Data))
	if err != nil {
		c.JSON(http.StatusInternalServerError, errorJSON("buffer: %v", err))
		return
	}
	defer buf.Close()

	_, ver, _, err := h.Service.CreatePackageAndAddFile(c.Request.Context(), pkgsvc.CreationInfo{
		TenantID:            tenant.ID,
		PackageType:         models.TypeNpm,
		PackageName:         pkg.Name,
		PackageLookupName:   strings.ToLower(pkg.Name),
		Version:             pkg.Version,
		VersionMetadataJSON: string(metaJSON),
		Filename:            pkg.Filename,
		IsLead:              true,
	}, buf)
	if err != nil {
		if errors.Is(err, models.ErrDuplicatePackageVersion) {
			c.JSON(http.StatusConflict, errorJSON("version %s@%s already exists", pkg.Name, pkg.Version))
			return
		}
		c.JSON(http.StatusInternalServerError, errorJSON("ingest: %v", err))
		return
	}

	// Persist license to the dedicated column (drives license_allow).
	if pkg.Metadata.License != "" {
		_ = h.Models.SetLicense(c.Request.Context(), ver.ID, pkg.Metadata.License)
	}

	// Apply dist-tags from the publish payload. The "latest" tag is the
	// most common — npm auto-sets it on every publish.
	for _, tag := range pkg.DistTags {
		if !validateTag(tag) {
			continue
		}
		_ = h.Models.SetProperty(c.Request.Context(),
			models.PropertyRefVersion, ver.ID, TagProperty+"."+tag, pkg.Version)
	}

	c.Status(http.StatusCreated)
}

// packument returns the merged metadata document for all versions of a
// package.
func (h *Handler) packument(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}
	name := packageNameFromParams(c)
	pkg, err := h.Models.GetPackageByLookup(c.Request.Context(), tenant.ID, models.TypeNpm, strings.ToLower(name))
	if err != nil {
		if errors.Is(err, models.ErrPackageNotExist) {
			c.JSON(http.StatusNotFound, errorJSON("no such package"))
			return
		}
		c.JSON(http.StatusInternalServerError, errorJSON("%v", err))
		return
	}

	versions, err := h.Models.ListVersions(c.Request.Context(), pkg.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, errorJSON("%v", err))
		return
	}
	// Apply policy — quarantined / denied versions are hidden.
	versions = h.filterReadable(c, tenant, pkg, versions)
	if len(versions) == 0 {
		c.JSON(http.StatusNotFound, errorJSON("no readable version"))
		return
	}

	// Sort by semver ASC.
	type verInfo struct {
		Ver  *models.Version
		Sem  *semver.Version
		Meta Metadata
		File *models.File
		Blob *models.Blob
	}
	infos := make([]verInfo, 0, len(versions))
	for _, v := range versions {
		s, err := semver.NewSemver(v.Version)
		if err != nil {
			continue
		}
		var meta Metadata
		_ = json.Unmarshal([]byte(v.MetadataJSON), &meta)
		files, err := h.Models.ListFilesByVersion(c.Request.Context(), v.ID)
		if err != nil || len(files) == 0 {
			continue
		}
		// Pick the lead .tgz.
		var f *models.File
		for _, x := range files {
			if x.IsLead {
				f = x
				break
			}
		}
		if f == nil {
			f = files[0]
		}
		blob, err := h.Models.GetBlobByID(c.Request.Context(), f.BlobID)
		if err != nil {
			continue
		}
		infos = append(infos, verInfo{Ver: v, Sem: s, Meta: meta, File: f, Blob: blob})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Sem.LessThan(infos[j].Sem) })
	if len(infos) == 0 {
		c.JSON(http.StatusNotFound, errorJSON("no readable version"))
		return
	}
	latest := infos[len(infos)-1]

	versionsResp := make(map[string]*PackageMetadataVersion, len(infos))
	for _, in := range infos {
		versionsResp[in.Sem.String()] = h.versionResponse(c, tenant, pkg, in.Ver, in.Meta, in.File, in.Blob)
	}

	// Collect dist-tags from per-version properties (npm.tag.<name>).
	distTags := map[string]string{}
	for _, in := range infos {
		// Properties for this version with prefix "npm.tag.".
		props, _ := h.Models.GetPropertiesByPrefix(c.Request.Context(),
			models.PropertyRefVersion, in.Ver.ID, TagProperty+".")
		for _, p := range props {
			tag := strings.TrimPrefix(p.Name, TagProperty+".")
			if tag != "" {
				distTags[tag] = in.Ver.Version
			}
		}
	}

	resp := &PackageMetadata{
		ID:          pkg.Name,
		Name:        pkg.Name,
		Description: latest.Meta.Description,
		DistTags:    distTags,
		Versions:    versionsResp,
		Readme:      latest.Meta.Readme,
		Homepage:    latest.Meta.ProjectURL,
		Author:      User{Name: latest.Meta.Author},
		Repository:  latest.Meta.Repository,
		License:     latest.Meta.License,
	}
	c.JSON(http.StatusOK, resp)
}

// versionResponse renders one version-block of a packument.
func (h *Handler) versionResponse(c *gin.Context, tenant *tenants.Tenant, pkg *models.Package, ver *models.Version, meta Metadata, f *models.File, blob *models.Blob) *PackageMetadataVersion {
	integrity := ""
	if blob.HashSHA512 != "" {
		raw, err := hex.DecodeString(blob.HashSHA512)
		if err == nil {
			integrity = "sha512-" + base64.StdEncoding.EncodeToString(raw)
		}
	}
	return &PackageMetadataVersion{
		ID:                   pkg.Name + "@" + ver.Version,
		Name:                 pkg.Name,
		Version:              ver.Version,
		Description:          meta.Description,
		Author:               User{Name: meta.Author},
		Homepage:             meta.ProjectURL,
		License:              meta.License,
		Repository:           meta.Repository,
		Keywords:             meta.Keywords,
		Dependencies:         meta.Dependencies,
		BundleDependencies:   meta.BundleDependencies,
		DevDependencies:      meta.DevelopmentDependencies,
		PeerDependencies:     meta.PeerDependencies,
		OptionalDependencies: meta.OptionalDependencies,
		Bin:                  meta.Bin,
		Readme:               meta.Readme,
		Dist: PackageDistribution{
			Shasum:    blob.HashSHA1,
			Integrity: integrity,
			Tarball:   fmt.Sprintf("%s/%s/-/%s", registryURL(c, tenant.Name), pkg.Name, f.Name),
		},
	}
}

// downloadTarball serves the .tgz bytes.
func (h *Handler) downloadTarball(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}
	name := packageNameFromParams(c)
	filename := c.Param("filename")
	pkg, err := h.Models.GetPackageByLookup(c.Request.Context(), tenant.ID, models.TypeNpm, strings.ToLower(name))
	if err != nil {
		c.JSON(http.StatusNotFound, errorJSON("no such package"))
		return
	}

	// We don't know the version from the URL; find the version that owns
	// a file with this name.
	versions, _ := h.Models.ListVersions(c.Request.Context(), pkg.ID)
	var match *models.File
	var owningVersion *models.Version
	for _, v := range versions {
		files, _ := h.Models.ListFilesByVersion(c.Request.Context(), v.ID)
		for _, f := range files {
			if strings.EqualFold(f.Name, filename) {
				match = f
				owningVersion = v
				break
			}
		}
		if match != nil {
			break
		}
	}
	if match == nil {
		c.JSON(http.StatusNotFound, errorJSON("no such file"))
		return
	}
	if !h.checkRead(c, tenant, pkg, owningVersion, match.Name) {
		return
	}
	rc, _, err := h.Service.OpenFile(c.Request.Context(), match)
	if err != nil {
		c.JSON(http.StatusInternalServerError, errorJSON("%v", err))
		return
	}
	defer rc.Close()
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, match.Name))
	_, _ = io.Copy(c.Writer, rc)
}

// --- dist-tags -------------------------------------------------------------

var tagMatcher = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9._-]*$`)

func validateTag(tag string) bool { return tag != "" && tagMatcher.MatchString(tag) }

func (h *Handler) listDistTags(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}
	name := packageNameFromParams(c)
	pkg, err := h.Models.GetPackageByLookup(c.Request.Context(), tenant.ID, models.TypeNpm, strings.ToLower(name))
	if err != nil {
		c.JSON(http.StatusNotFound, errorJSON("no such package"))
		return
	}
	versions, _ := h.Models.ListVersions(c.Request.Context(), pkg.ID)
	tags := map[string]string{}
	for _, v := range versions {
		props, _ := h.Models.GetPropertiesByPrefix(c.Request.Context(),
			models.PropertyRefVersion, v.ID, TagProperty+".")
		for _, p := range props {
			tag := strings.TrimPrefix(p.Name, TagProperty+".")
			if tag != "" {
				tags[tag] = v.Version
			}
		}
	}
	c.JSON(http.StatusOK, tags)
}

func (h *Handler) setDistTag(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireWrite(c, tenant) {
		return
	}
	tag := c.Param("tag")
	if !validateTag(tag) {
		c.JSON(http.StatusBadRequest, errorJSON("invalid tag name"))
		return
	}
	name := packageNameFromParams(c)
	pkg, err := h.Models.GetPackageByLookup(c.Request.Context(), tenant.ID, models.TypeNpm, strings.ToLower(name))
	if err != nil {
		c.JSON(http.StatusNotFound, errorJSON("no such package"))
		return
	}
	body, _ := io.ReadAll(c.Request.Body)
	versionStr := strings.Trim(strings.TrimSpace(string(body)), `"`)
	ver, err := h.Models.GetVersion(c.Request.Context(), pkg.ID, versionStr)
	if err != nil {
		c.JSON(http.StatusNotFound, errorJSON("no such version %s@%s", name, versionStr))
		return
	}
	// Clear the tag from every other version first.
	if err := h.clearTagAcrossVersions(c, pkg.ID, tag); err != nil {
		c.JSON(http.StatusInternalServerError, errorJSON("%v", err))
		return
	}
	if err := h.Models.SetProperty(c.Request.Context(),
		models.PropertyRefVersion, ver.ID, TagProperty+"."+tag, ver.Version); err != nil {
		c.JSON(http.StatusInternalServerError, errorJSON("%v", err))
		return
	}
	c.Status(http.StatusOK)
}

func (h *Handler) deleteDistTag(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireWrite(c, tenant) {
		return
	}
	tag := c.Param("tag")
	if !validateTag(tag) {
		c.JSON(http.StatusBadRequest, errorJSON("invalid tag name"))
		return
	}
	name := packageNameFromParams(c)
	pkg, err := h.Models.GetPackageByLookup(c.Request.Context(), tenant.ID, models.TypeNpm, strings.ToLower(name))
	if err != nil {
		c.JSON(http.StatusNotFound, errorJSON("no such package"))
		return
	}
	if err := h.clearTagAcrossVersions(c, pkg.ID, tag); err != nil {
		c.JSON(http.StatusInternalServerError, errorJSON("%v", err))
		return
	}
	c.Status(http.StatusOK)
}

func (h *Handler) clearTagAcrossVersions(c *gin.Context, packageID int64, tag string) error {
	versions, err := h.Models.ListVersions(c.Request.Context(), packageID)
	if err != nil {
		return err
	}
	for _, v := range versions {
		if err := h.Models.DeleteProperty(c.Request.Context(),
			models.PropertyRefVersion, v.ID, TagProperty+"."+tag); err != nil {
			return err
		}
	}
	return nil
}

// --- policy hooks ----------------------------------------------------------

func (h *Handler) subjectFor(tenant *tenants.Tenant, pkg *models.Package, ver *models.Version, filename string) policy.Subject {
	s := policy.Subject{
		TenantID: tenant.ID,
		Format:   string(models.TypeNpm),
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
		c.JSON(http.StatusForbidden, errorJSON("%s", policyReason(r)))
		return false
	}
	return true
}

func (h *Handler) checkIngest(c *gin.Context, tenant *tenants.Tenant, name, version, filename, license string) bool {
	subj := policy.Subject{
		TenantID: tenant.ID,
		Format:   string(models.TypeNpm),
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
		c.JSON(http.StatusForbidden, errorJSON("%s", policyReason(r)))
		return false
	}
	return true
}

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
