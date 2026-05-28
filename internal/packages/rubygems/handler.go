// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2021 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// The HTTP endpoint shape (legacy specs.4.8.gz, compact index /info/<gem>
// and /versions, /quick/Marshal.4.8/<gem>.gemspec.rz, /gems/<file>, the
// /api/v1/gems POST + yank DELETE) is modeled on
// forgejo/routers/api/packages/rubygems/rubygems.go (MIT). The Ruby
// Marshal-encoded /specs.4.8.gz response shape, the /info/<gem> compact
// index format, and the gemspec.rz Gem::Specification skeleton are
// preserved verbatim from upstream so real `gem` and Bundler clients
// don't have to special-case our registry.

package rubygems

import (
	"compress/gzip"
	"compress/zlib"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

// Handler is the RubyGems registry HTTP handler.
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

// Register mounts rubygems routes on g (expected to be scoped to
// /api/packages/:tenant/rubygems).
//
// We serve both the legacy Marshal-encoded specs (used by older clients)
// and the compact index format (`/info`, `/versions`) used by modern gem
// + Bundler. The two are independent of each other — clients pick.
//
//	GET    /specs.4.8.gz                    full legacy specs index
//	GET    /latest_specs.4.8.gz             latest version of each package
//	GET    /prerelease_specs.4.8.gz         (empty — we don't model prerelease)
//	GET    /info/:package                   compact index info file
//	GET    /versions                        compact index versions file
//	GET    /quick/Marshal.4.8/:filename     per-gem zlib-Marshal gemspec
//	GET    /gems/:filename                  download the .gem tarball
//	POST   /api/v1/gems                     upload (auth)
//	DELETE /api/v1/gems/yank                yank a version (auth)
func (h *Handler) Register(g *gin.RouterGroup) {
	g.GET("/specs.4.8.gz", h.enumeratePackages)
	g.GET("/latest_specs.4.8.gz", h.enumeratePackagesLatest)
	g.GET("/prerelease_specs.4.8.gz", h.enumeratePackagesPrerelease)
	g.GET("/info/:package", h.servePackageInfo)
	g.GET("/versions", h.serveVersionsFile)
	g.GET("/quick/Marshal.4.8/:filename", h.servePackageSpecification)
	g.GET("/gems/:filename", h.downloadPackageFile)
	g.POST("/api/v1/gems", h.uploadPackageFile)
	// Older gem clients send the upload without a trailing slash to a path
	// that doesn't start with /api/v1; keep the canonical path only.
	g.DELETE("/api/v1/gems/yank", h.yankPackageVersion)
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

// versionMetadata decodes the JSON we persisted on ingest. Returns a
// zero-value Metadata if the row is empty or malformed — we don't fail
// the response on metadata problems because the gem file itself is still
// downloadable.
func versionMetadata(v *models.Version) *Metadata {
	m := &Metadata{Platform: "ruby"}
	if v == nil || v.MetadataJSON == "" {
		return m
	}
	_ = json.Unmarshal([]byte(v.MetadataJSON), m)
	if m.Platform == "" {
		m.Platform = "ruby"
	}
	return m
}

// listAllVersions returns every (package, version) pair for the tenant
// that the caller is currently allowed to see. Used by the legacy specs
// and compact index endpoints.
func (h *Handler) listAllVersions(c *gin.Context, tenant *tenants.Tenant) ([]*models.Package, map[int64][]*models.Version, error) {
	pkgs, err := h.Models.ListPackages(c.Request.Context(), tenant.ID, models.TypeRubyGems)
	if err != nil {
		return nil, nil, err
	}
	out := make(map[int64][]*models.Version, len(pkgs))
	visible := make([]*models.Package, 0, len(pkgs))
	for _, p := range pkgs {
		vs, err := h.Models.ListVersions(c.Request.Context(), p.ID)
		if err != nil {
			return nil, nil, err
		}
		vs = h.filterReadable(c, tenant, p, vs)
		if len(vs) == 0 {
			continue
		}
		visible = append(visible, p)
		out[p.ID] = vs
	}
	sort.Slice(visible, func(i, j int) bool { return visible[i].LowerName < visible[j].LowerName })
	return visible, out, nil
}

// --- legacy specs ----------------------------------------------------------

// specsTuple is the Marshal shape used inside specs.4.8.gz:
// [package_name, Gem::Version("1.0.0"), platform].
type specsTuple []any

func (h *Handler) enumeratePackages(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}
	pkgs, versions, err := h.listAllVersions(c, tenant)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	specs := make([]any, 0, 16)
	for _, p := range pkgs {
		for _, v := range versions[p.ID] {
			meta := versionMetadata(v)
			specs = append(specs, specsTuple{
				p.Name,
				&RubyUserMarshal{Name: "Gem::Version", Value: []string{v.Version}},
				meta.Platform,
			})
		}
	}
	writeSpecs(c, "specs.4.8", specs)
}

func (h *Handler) enumeratePackagesLatest(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}
	pkgs, versions, err := h.listAllVersions(c, tenant)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	specs := make([]any, 0, len(pkgs))
	for _, p := range pkgs {
		vs := versions[p.ID]
		if len(vs) == 0 {
			continue
		}
		// ListVersions returns oldest-first; the newest is the last entry.
		// RubyGems' notion of "latest" is the highest semver, but for our
		// minimal compatibility we use newest-upload — gem and Bundler
		// both re-check the compact index before resolving so this is
		// purely advisory.
		latest := vs[len(vs)-1]
		meta := versionMetadata(latest)
		specs = append(specs, specsTuple{
			p.Name,
			&RubyUserMarshal{Name: "Gem::Version", Value: []string{latest.Version}},
			meta.Platform,
		})
	}
	writeSpecs(c, "latest_specs.4.8", specs)
}

func (h *Handler) enumeratePackagesPrerelease(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}
	writeSpecs(c, "prerelease_specs.4.8", []any{})
}

// writeSpecs gzip-wraps a Marshal-encoded specs array and sets the
// canonical Content-Disposition + filename. The inner gzip filename is
// the un-suffixed name; this matches what `gem` expects.
func writeSpecs(c *gin.Context, basename string, specs []any) {
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, basename+".gz"))
	zw := gzip.NewWriter(c.Writer)
	zw.Name = basename
	defer zw.Close()
	if err := NewMarshalEncoder(zw).Encode(specs); err != nil {
		// Headers are already sent; nothing useful to do but log via gin's
		// default writer. We still flush whatever bytes we have.
		c.Error(err) //nolint:errcheck // best-effort
	}
}

// --- compact index ---------------------------------------------------------

// servePackageInfo implements /info/:package per the compact-index spec:
//
//	https://guides.rubygems.org/rubygems-org-compact-index-api/
//
// Format: one line per version: "<version>[-<platform>] <deps>|<extras>"
// where <deps> is a comma-separated list of "name:reqs" and <extras> is
// "checksum:<sha256>[,ruby:<req>][,rubygems:<req>]".
func (h *Handler) servePackageInfo(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}
	name := c.Param("package")
	pkg, err := h.Models.GetPackage(c.Request.Context(), tenant.ID, models.TypeRubyGems, name)
	if err != nil {
		c.String(http.StatusNotFound, "Could not find package %s", name)
		return
	}
	versions, err := h.Models.ListVersions(c.Request.Context(), pkg.ID)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	versions = h.filterReadable(c, tenant, pkg, versions)
	if len(versions) == 0 {
		c.String(http.StatusNotFound, "Could not find package %s", name)
		return
	}
	body, err := h.buildInfoFile(c, pkg, versions)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.String(http.StatusOK, body)
}

// serveVersionsFile implements /versions per the compact-index spec:
// "---\n" header, one line per package: "<name> <ver1>,<ver2>,... <md5>".
// The md5 is over the corresponding /info/<name> response body — clients
// use it to decide whether to refetch.
func (h *Handler) serveVersionsFile(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}
	pkgs, versions, err := h.listAllVersions(c, tenant)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	var b strings.Builder
	b.WriteString("---\n")
	for _, p := range pkgs {
		vs := versions[p.ID]
		if len(vs) == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s ", p.Name)
		for i, v := range vs {
			b.WriteString(v.Version)
			meta := versionMetadata(v)
			if meta.Platform != "" && meta.Platform != "ruby" {
				b.WriteByte('_')
				b.WriteString(meta.Platform)
			}
			if i != len(vs)-1 {
				b.WriteByte(',')
			}
		}
		info, err := h.buildInfoFile(c, p, vs)
		if err != nil {
			c.String(http.StatusInternalServerError, "%v", err)
			return
		}
		sum := md5.Sum([]byte(info))
		fmt.Fprintf(&b, " %x\n", sum)
	}
	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.String(http.StatusOK, b.String())
}

func (h *Handler) buildInfoFile(c *gin.Context, pkg *models.Package, versions []*models.Version) (string, error) {
	var b strings.Builder
	b.WriteString("---\n")
	for _, v := range versions {
		line, err := h.buildRequirementLine(c, pkg, v)
		if err != nil {
			return "", err
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func (h *Handler) buildRequirementLine(c *gin.Context, pkg *models.Package, v *models.Version) (string, error) {
	meta := versionMetadata(v)
	// Dependency requirements: "dep1:>= 1.0&< 2.0,dep2:>= 0"
	var deps strings.Builder
	for i, dep := range meta.RuntimeDependencies {
		if i != 0 {
			deps.WriteByte(',')
		}
		deps.WriteString(dep.Name)
		deps.WriteByte(':')
		writeRequirements(&deps, dep.Version)
	}
	// Find the lead file for this version so we can quote its sha256.
	files, err := h.Models.ListFilesByVersion(c.Request.Context(), v.ID)
	if err != nil {
		return "", err
	}
	wantFilename := FullFilename(pkg.Name, v.Version, meta.Platform)
	var leadFile *models.File
	for _, f := range files {
		if strings.EqualFold(f.Name, wantFilename) {
			leadFile = f
			break
		}
	}
	// Fall back to the file marked IsLead if filename doesn't match
	// (shouldn't happen for a normally-uploaded gem).
	if leadFile == nil {
		for _, f := range files {
			if f.IsLead {
				leadFile = f
				break
			}
		}
	}
	if leadFile == nil {
		return "", fmt.Errorf("no lead file for %s@%s", pkg.Name, v.Version)
	}
	blob, err := h.Models.GetBlobByID(c.Request.Context(), leadFile.BlobID)
	if err != nil {
		return "", err
	}
	var extras strings.Builder
	fmt.Fprintf(&extras, "checksum:%s", blob.HashSHA256)
	if len(meta.RequiredRubyVersion) != 0 {
		extras.WriteString(",ruby:")
		writeRequirements(&extras, meta.RequiredRubyVersion)
	}
	if len(meta.RequiredRubygemsVersion) != 0 {
		extras.WriteString(",rubygems:")
		writeRequirements(&extras, meta.RequiredRubygemsVersion)
	}
	if meta.Platform != "" && meta.Platform != "ruby" {
		return fmt.Sprintf("%s-%s %s|%s", v.Version, meta.Platform, deps.String(), extras.String()), nil
	}
	return fmt.Sprintf("%s %s|%s", v.Version, deps.String(), extras.String()), nil
}

func writeRequirements(b *strings.Builder, reqs []VersionRequirement) {
	if len(reqs) == 0 {
		reqs = []VersionRequirement{{Restriction: ">=", Version: "0"}}
	}
	for i, r := range reqs {
		if i != 0 {
			b.WriteByte('&')
		}
		b.WriteString(r.Restriction)
		b.WriteByte(' ')
		b.WriteString(r.Version)
	}
}

// --- per-gem spec ----------------------------------------------------------

// servePackageSpecification implements /quick/Marshal.4.8/<filename>:
// a zlib-compressed Ruby Marshal Gem::Specification skeleton. Older
// `gem` clients fetch this to introspect a gem before downloading.
func (h *Handler) servePackageSpecification(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}
	filename := c.Param("filename")
	if !strings.HasSuffix(filename, ".gemspec.rz") {
		c.Status(http.StatusNotImplemented)
		return
	}
	gemFile := filename[:len(filename)-len(".gemspec.rz")] + ".gem"
	pkg, ver, _, err := h.findByFilename(c, tenant, gemFile)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	meta := versionMetadata(ver)
	spec := &RubyUserDef{
		Name: "Gem::Specification",
		Value: []any{
			"3.2.3", // @rubygems_version
			4,       // @specification_version
			pkg.Name,
			&RubyUserMarshal{Name: "Gem::Version", Value: []string{ver.Version}},
			nil,           // date
			meta.Summary,  // @summary
			nil,           // @required_ruby_version
			nil,           // @required_rubygems_version
			meta.Platform, // @original_platform
			[]any{},       // @dependencies
			nil,           // rubyforge_project
			"",            // @email
			meta.Authors,
			meta.Description,
			meta.ProjectURL,
			true,          // has_rdoc
			meta.Platform, // @new_platform
			nil,
			meta.Licenses,
		},
	}
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filename))
	zw := zlib.NewWriter(c.Writer)
	defer zw.Close()
	if err := NewMarshalEncoder(zw).Encode(spec); err != nil {
		c.Error(err) //nolint:errcheck
	}
}

// downloadPackageFile serves the raw .gem bytes.
func (h *Handler) downloadPackageFile(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}
	filename := c.Param("filename")
	pkg, ver, file, err := h.findByFilename(c, tenant, filename)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	if !h.checkRead(c, tenant, pkg, ver, file.Name) {
		return
	}
	rc, _, err := h.Service.OpenFile(c.Request.Context(), file)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	defer rc.Close()
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, file.Name))
	_, _ = io.Copy(c.Writer, rc)
}

// findByFilename walks the tenant's RubyGems packages looking for one
// whose (name, version, platform) triple produces filename. Returns the
// owning package, version, and file row.
func (h *Handler) findByFilename(c *gin.Context, tenant *tenants.Tenant, filename string) (*models.Package, *models.Version, *models.File, error) {
	pkgs, err := h.Models.ListPackages(c.Request.Context(), tenant.ID, models.TypeRubyGems)
	if err != nil {
		return nil, nil, nil, err
	}
	wantLower := strings.ToLower(filename)
	for _, p := range pkgs {
		versions, err := h.Models.ListVersions(c.Request.Context(), p.ID)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, v := range versions {
			meta := versionMetadata(v)
			candidate := strings.ToLower(FullFilename(p.Name, v.Version, meta.Platform))
			if candidate != wantLower {
				continue
			}
			files, err := h.Models.ListFilesByVersion(c.Request.Context(), v.ID)
			if err != nil {
				return nil, nil, nil, err
			}
			for _, f := range files {
				if strings.EqualFold(f.Name, filename) {
					return p, v, f, nil
				}
			}
		}
	}
	return nil, nil, nil, errors.New("not found")
}

// --- upload + yank ---------------------------------------------------------

// uploadPackageFile implements POST /api/v1/gems — the body is the raw
// .gem tarball. `gem push` sends it as `application/octet-stream` with
// `Authorization: <token>` (no Bearer prefix); we accept either form.
func (h *Handler) uploadPackageFile(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireWrite(c, tenant) {
		return
	}
	buf, err := h.Service.NewHashedBuffer(c.Request.Body)
	if err != nil {
		c.String(http.StatusInternalServerError, "buffer: %v", err)
		return
	}
	defer buf.Close()

	// The hashed buffer is positioned at end-of-write; rewind before reading.
	if _, err := buf.Seek(0, io.SeekStart); err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	pkg, err := ParsePackageMetaData(buf)
	if err != nil {
		if errors.Is(err, ErrMissingMetadataFile) ||
			errors.Is(err, ErrInvalidName) ||
			errors.Is(err, ErrInvalidVersion) {
			c.String(http.StatusBadRequest, "%v", err)
			return
		}
		c.String(http.StatusBadRequest, "parse gem: %v", err)
		return
	}
	if _, err := buf.Seek(0, io.SeekStart); err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}

	filename := FullFilename(pkg.Name, pkg.Version, pkg.Metadata.Platform)
	license := ""
	if len(pkg.Metadata.Licenses) > 0 {
		license = pkg.Metadata.Licenses[0]
	}
	if !h.checkIngest(c, tenant, pkg.Name, pkg.Version, filename, license) {
		return
	}

	metaJSON, _ := json.Marshal(pkg.Metadata)
	_, ver, _, err := h.Service.CreatePackageAndAddFile(c.Request.Context(), pkgsvc.CreationInfo{
		TenantID:            tenant.ID,
		PackageType:         models.TypeRubyGems,
		PackageName:         pkg.Name,
		PackageLookupName:   strings.ToLower(pkg.Name),
		Version:             pkg.Version,
		VersionMetadataJSON: string(metaJSON),
		Filename:            filename,
		IsLead:              true,
	}, buf)
	if err != nil {
		if errors.Is(err, models.ErrDuplicatePackageVersion) {
			c.String(http.StatusConflict, "version %s@%s already exists", pkg.Name, pkg.Version)
			return
		}
		c.String(http.StatusInternalServerError, "ingest: %v", err)
		return
	}
	if license != "" {
		_ = h.Models.SetLicense(c.Request.Context(), ver.ID, license)
	}
	c.Status(http.StatusCreated)
}

// yankPackageVersion implements DELETE /api/v1/gems/yank. RubyGems yank
// semantics are "make this version uninstallable but leave the file
// archived"; we map that to a policy-style quarantine with a stable
// reason so it round-trips into the audit log and the /admin/quarantine
// view.
func (h *Handler) yankPackageVersion(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireWrite(c, tenant) {
		return
	}
	// net/http's ParseForm only reads bodies for POST/PUT/PATCH; the
	// `gem yank` client sends DELETE with a form-encoded body, so we
	// parse manually. We cap the read at 1 MiB defensively.
	name, version, err := parseYankForm(c.Request)
	if err != nil {
		c.String(http.StatusBadRequest, "%v", err)
		return
	}
	if name == "" || version == "" {
		c.String(http.StatusBadRequest, "gem_name and version are required")
		return
	}
	pkg, err := h.Models.GetPackage(c.Request.Context(), tenant.ID, models.TypeRubyGems, name)
	if err != nil {
		c.String(http.StatusNotFound, "package %s not found", name)
		return
	}
	ver, err := h.Models.GetVersion(c.Request.Context(), pkg.ID, version)
	if err != nil {
		c.String(http.StatusNotFound, "version %s@%s not found", name, version)
		return
	}
	if err := h.Models.QuarantineVersion(c.Request.Context(), ver.ID, 0, "yanked"); err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	c.String(http.StatusOK, "Successfully yanked %s-%s\n", name, version)
}

// parseYankForm reads up to 1 MiB of the request body and decodes it as
// application/x-www-form-urlencoded. We do this manually because
// net/http's ParseForm() only reads bodies for POST/PUT/PATCH, and the
// gem yank API uses DELETE.
func parseYankForm(r *http.Request) (name, version string, err error) {
	if r.Body == nil {
		return "", "", nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return "", "", err
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return "", "", err
	}
	// Also honor query-string values if a client sent them there.
	for k, v := range r.URL.Query() {
		if _, ok := values[k]; !ok {
			values[k] = v
		}
	}
	return values.Get("gem_name"), values.Get("version"), nil
}

// --- policy hooks ----------------------------------------------------------

func (h *Handler) subjectFor(tenant *tenants.Tenant, pkg *models.Package, ver *models.Version, filename string) policy.Subject {
	s := policy.Subject{
		TenantID: tenant.ID,
		Format:   string(models.TypeRubyGems),
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
		Format:   string(models.TypeRubyGems),
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

func (h *Handler) filterReadable(c *gin.Context, tenant *tenants.Tenant, pkg *models.Package, versions []*models.Version) []*models.Version {
	out := versions[:0]
	for _, v := range versions {
		if v.IsQuarantined() {
			continue
		}
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
