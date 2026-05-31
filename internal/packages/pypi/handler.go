// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2021-2025 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// PyPI handlers, name/version validation, and PEP 503 / PEP 691 simple
// index rendering, modeled on forgejo/routers/api/packages/pypi/pypi.go
// (MIT). Unlike Forgejo we do PEP 503 name normalization in full
// (re-collapse runs of [-_.] and lowercase) and we emit a real root
// /simple/ index in addition to per-package pages.

// Package pypi implements the PyPI legacy upload + simple repository
// protocol (https://peps.python.org/pep-0503/, PEP 691, and the
// undocumented-but-de-facto twine upload form fields).
package pypi

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
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
	"github.com/astockwell/pkgmirror/internal/upstream"

	"github.com/gin-gonic/gin"
)

// Property keys for per-version PyPI metadata stashed in
// package_properties.
const (
	PropAuthor          = "pypi.author"
	PropSummary         = "pypi.summary"
	PropDescription     = "pypi.description"
	PropLongDescription = "pypi.long_description"
	PropProjectURL      = "pypi.project_url"
	PropLicense         = "pypi.license"
	PropRequiresPython  = "pypi.requires_python"
)

// Handler is the PyPI HTTP handler.
type Handler struct {
	Service  *pkgsvc.Service
	Models   *models.Store
	Tenants  *tenants.Store
	Engine   policy.Engine
	Upstream upstream.Fetcher // optional; nil disables pull-through
}

// NewHandler constructs a Handler. If eng is nil, the no-op engine is used.
// Upstream is set separately so existing call sites (older tests) keep
// compiling; use WithUpstream to set it, or just construct the struct
// directly.
func NewHandler(svc *pkgsvc.Service, m *models.Store, ts *tenants.Store, eng policy.Engine) *Handler {
	if eng == nil {
		eng = policy.NoopEngine{}
	}
	return &Handler{Service: svc, Models: m, Tenants: ts, Engine: eng}
}

// WithUpstream returns h with the pull-through fetcher attached. Use
// at construction time. Nil-safe: passing nil leaves pull-through off.
func (h *Handler) WithUpstream(f upstream.Fetcher) *Handler {
	h.Upstream = f
	return h
}

// Register attaches PyPI routes to g (already scoped to
// /api/packages/:tenant/pypi).
//
//	POST /                              upload (multipart legacy API)
//	GET  /simple/                       root simple index (HTML)
//	GET  /simple/:name/                 per-package simple index (HTML or JSON)
//	GET  /files/:name/:version/:filename  download
//
// We register both the trailing-slash and no-slash variants so that pip's
// default redirect behavior just works.
func (h *Handler) Register(g *gin.RouterGroup) {
	g.POST("", h.upload)
	g.POST("/", h.upload)
	g.GET("/simple", h.rootIndex)
	g.GET("/simple/", h.rootIndex)
	g.GET("/simple/:name", h.packageIndex)
	g.GET("/simple/:name/", h.packageIndex)
	g.GET("/files/:name/:version/:filename", h.download)
}

// tenantFromPath resolves the :tenant gin param to a tenant row.
func (h *Handler) tenantFromPath(c *gin.Context) *tenants.Tenant {
	name := c.Param("tenant")
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

// --- Validation & normalization ---------------------------------------------

// nameMatcher implements PEP 426 valid name syntax.
var nameMatcher = regexp.MustCompile(`\A(?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9\.\-_]*[a-zA-Z0-9])\z`)

// versionMatcher implements PEP 440 (appendix B).
var versionMatcher = regexp.MustCompile(`\Av?` +
	`(?:[0-9]+!)?` +
	`[0-9]+(?:\.[0-9]+)*` +
	`(?:[-_\.]?(?:a|b|c|rc|alpha|beta|pre|preview)[-_\.]?[0-9]*)?` +
	`(?:-[0-9]+|[-_\.]?(?:post|rev|r)[-_\.]?[0-9]*)?` +
	`(?:[-_\.]?dev[-_\.]?[0-9]*)?` +
	`(?:\+[a-z0-9]+(?:[-_\.][a-z0-9]+)*)?` +
	`\z`)

// pep503Runs collapses any run of "-", "_", or "." into a single "-".
var pep503Runs = regexp.MustCompile(`[-_.]+`)

// NormalizeName performs full PEP 503 normalization (lowercase + collapse
// runs of [-_.] to a single "-"). Forgejo's normalizer is partial
// (it only replaces individual "." and "_" with "-" and does not
// lowercase); we do the full normalization so that callers can use any
// equivalent name and reach the same package.
func NormalizeName(name string) string {
	return strings.ToLower(pep503Runs.ReplaceAllString(name, "-"))
}

// isValidNameAndVersion reports whether name + version pass PEP 426 / 440.
func isValidNameAndVersion(name, version string) bool {
	return nameMatcher.MatchString(name) && versionMatcher.MatchString(version)
}

// --- Handlers ---------------------------------------------------------------

// upload: POST / — multipart form upload (the "legacy" twine API).
func (h *Handler) upload(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireWrite(c, tenant) {
		return
	}

	// 32 MiB form memory cap; larger files spill to a temp file.
	if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
		c.String(http.StatusBadRequest, "parse multipart: %v", err)
		return
	}
	file, fh, err := c.Request.FormFile("content")
	if err != nil {
		c.String(http.StatusBadRequest, "missing 'content' file: %v", err)
		return
	}
	defer file.Close()

	rawName := c.Request.FormValue("name")
	rawVersion := c.Request.FormValue("version")
	if !isValidNameAndVersion(rawName, rawVersion) {
		c.String(http.StatusBadRequest, "invalid PEP-426 name or PEP-440 version")
		return
	}
	lookupName := NormalizeName(rawName)

	buf, err := h.Service.NewHashedBuffer(file)
	if err != nil {
		c.String(http.StatusInternalServerError, "buffer upload: %v", err)
		return
	}
	defer buf.Close()

	// Verify the sha256_digest if the client supplied one (twine does).
	_, _, sha256Hex, _ := buf.Sums()
	if claimed := c.Request.FormValue("sha256_digest"); claimed != "" {
		if !strings.EqualFold(claimed, sha256Hex) {
			c.String(http.StatusBadRequest,
				"sha256_digest mismatch: client=%s server=%s", claimed, sha256Hex)
			return
		}
	}

	homepage := extractHomepageURL(c.Request.Form["project_urls"])
	if homepage == "" {
		// Deprecated metadata field; honor it for older clients.
		homepage = c.Request.FormValue("home_page")
	}

	versionProps := map[string]string{}
	for prop, val := range map[string]string{
		PropAuthor:          c.Request.FormValue("author"),
		PropSummary:         c.Request.FormValue("summary"),
		PropDescription:     c.Request.FormValue("description"),
		PropLongDescription: c.Request.FormValue("long_description"),
		PropProjectURL:      homepage,
		PropLicense:         c.Request.FormValue("license"),
		PropRequiresPython:  c.Request.FormValue("requires_python"),
	} {
		if val != "" {
			versionProps[prop] = val
		}
	}

	if !h.checkIngest(c, tenant, lookupName, rawVersion, fh.Filename, c.Request.FormValue("license")) {
		return
	}

	// Provenance gate: refuse uploads to a package that pkgmirror
	// originally ingested via pull-through. Without this, an insider
	// (or compromised CI token) could silently shadow an upstream
	// package by uploading a 'newer' version that the /simple/ merge
	// would then surface above the real upstream versions. The admin
	// can either delete the package or flip its provenance to
	// 'uploaded' via the console to take ownership. See
	// plans/created-via-package-ownership.md.
	if existing, perr := h.Models.GetPackageByLookup(c.Request.Context(), tenant.ID, models.TypePyPI, lookupName); perr == nil {
		if existing.CreatedVia == models.CreatedViaPullThrough {
			c.String(http.StatusConflict,
				"package %q is currently mirrored from upstream; "+
					"delete it via /console/tenants/%s/packages/pypi/%s "+
					"first if you want to take ownership of this name",
				existing.Name, tenant.Name, existing.LowerName)
			return
		}
	}

	_, ver, _, err := h.Service.CreatePackageOrAddFileToExisting(c.Request.Context(), pkgsvc.CreationInfo{
		TenantID:          tenant.ID,
		PackageType:       models.TypePyPI,
		PackageName:       rawName,
		PackageLookupName: lookupName,
		Version:           rawVersion,
		VersionProperties: versionProps,
		Filename:          fh.Filename,
		IsLead:            true,
		CreatedVia:        models.CreatedViaUploaded,
	}, buf)
	if err != nil {
		if errors.Is(err, models.ErrDuplicatePackageFile) {
			c.String(http.StatusConflict, "file %q already uploaded for %s==%s",
				fh.Filename, lookupName, rawVersion)
			return
		}
		c.String(http.StatusInternalServerError, "ingest: %v", err)
		return
	}

	// Persist the SPDX license (if supplied) to the dedicated column so
	// the license_allow evaluator can consult it on subsequent reads.
	// Done after ingest so a write failure here doesn't lose the artifact.
	if license := strings.TrimSpace(c.Request.FormValue("license")); license != "" && ver != nil {
		_ = h.Models.SetLicense(c.Request.Context(), ver.ID, license)
	}

	c.Status(http.StatusCreated)
}

// rootIndex: GET /simple/ — PEP 503 root simple index, listing every package
// in the tenant.
func (h *Handler) rootIndex(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}
	pkgs, err := h.Models.ListPackages(c.Request.Context(), tenant.ID, models.TypePyPI)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].LowerName < pkgs[j].LowerName })

	rows := make([]struct{ Name, URL string }, 0, len(pkgs))
	for _, p := range pkgs {
		if !h.hasAnyReadableVersion(c, tenant, p) {
			continue
		}
		rows = append(rows, struct{ Name, URL string }{
			Name: p.Name,
			URL:  "./" + p.LowerName + "/",
		})
	}
	c.Header("Content-Type", "text/html; charset=utf-8")
	if err := rootIndexTmpl.Execute(c.Writer, rows); err != nil {
		// Headers already sent; nothing useful to do.
		_ = err
	}
}

// packageIndex: GET /simple/:name(/) — per-package simple index.
// Content type negotiation: Accept: application/vnd.pypi.simple.v1+json
// returns the PEP 691 JSON form; everything else returns HTML.
func (h *Handler) packageIndex(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}

	name := NormalizeName(c.Param("name"))
	pkg, err := h.Models.GetPackageByLookup(c.Request.Context(), tenant.ID, models.TypePyPI, name)
	if err != nil {
		if errors.Is(err, models.ErrPackageNotExist) {
			// Pull-through: package unknown locally. Try upstream.
			if h.passthroughEnabled(c, tenant) {
				up, ferr := h.fetchUpstreamSimple(c, tenant, name)
				if ferr == nil {
					h.servePulledThroughIndex(c, tenant, name, up)
					return
				}
				code, msg := mapUpstreamErr(ferr)
				c.String(code, "%s", msg)
				return
			}
			c.String(http.StatusNotFound, "no such package")
			return
		}
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	versions, err := h.Models.ListVersions(c.Request.Context(), pkg.ID)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	sort.Slice(versions, func(i, j int) bool {
		return strings.Compare(versions[i].Version, versions[j].Version) < 0
	})
	versions = h.filterReadableVersions(c, tenant, pkg, versions)

	type fileEntry struct {
		Filename       string
		URL            string
		SHA256         string
		Size           int64
		RequiresPython string
	}
	var files []fileEntry
	localFilenames := map[string]struct{}{}
	localVersions := map[string]struct{}{}
	for _, v := range versions {
		fs, err := h.Models.ListFilesByVersion(c.Request.Context(), v.ID)
		if err != nil {
			c.String(http.StatusInternalServerError, "%v", err)
			return
		}
		reqPy, _, _ := h.Models.GetProperty(c.Request.Context(),
			models.PropertyRefVersion, v.ID, PropRequiresPython)
		for _, f := range fs {
			size, sha := h.fetchBlobMeta(c, f.BlobID)
			files = append(files, fileEntry{
				Filename:       f.Name,
				URL:            fmt.Sprintf("../../files/%s/%s/%s", pkg.LowerName, v.Version, f.Name),
				SHA256:         sha,
				Size:           size,
				RequiresPython: reqPy,
			})
			localFilenames[f.Name] = struct{}{}
		}
		localVersions[v.Version] = struct{}{}
	}

	// Merge: pull the upstream index too (when pull-through is on)
	// and add any upstream files whose filename isn't already in
	// local AND pass the policy gate. This is the "the package now
	// exists locally, but a freshly-allowed-by-policy upstream
	// version should still show up" case - e.g. after a cooldown
	// rule's window elapses, or after the rule is disabled.
	//
	// Errors here (upstream off / 404 / transport blip) silently
	// fall back to the local-only view; we have something useful
	// to serve and the merge is a feature, not load-bearing.
	//
	// Provenance gate: only merge when this package was originally
	// pulled through. Tenant-uploaded packages are SEALED - we never
	// reach upstream for their names, which (a) eliminates the
	// typosquat hole the merge would otherwise open and (b) saves
	// a round-trip on every /simple/ request for tenant-owned
	// packages. See plans/created-via-package-ownership.md.
	var upEntries []pypiFileEntry
	var upVersions map[string]struct{}
	if pkg.CreatedVia == models.CreatedViaPullThrough {
		upEntries, upVersions = h.mergeUpstreamIntoLocalIndex(c, tenant, name, localFilenames)
	}
	for _, e := range upEntries {
		files = append(files, fileEntry{
			Filename: e.Filename,
			URL:      e.URL,
			SHA256:   e.SHA256,
		})
	}
	// Union the version lists for PEP 691 consumers.
	mergedVersions := make([]string, 0, len(versions)+len(upVersions))
	for _, v := range versions {
		mergedVersions = append(mergedVersions, v.Version)
	}
	for v := range upVersions {
		if _, dup := localVersions[v]; dup {
			continue
		}
		mergedVersions = append(mergedVersions, v)
	}
	sort.Strings(mergedVersions)

	if wantsJSONSimple(c.Request) {
		c.Header("Content-Type", "application/vnd.pypi.simple.v1+json")
		jsonFiles := make([]jsonFile, len(files))
		for i, f := range files {
			jsonFiles[i] = jsonFile{
				Filename:       f.Filename,
				URL:            f.URL,
				Hashes:         jsonHashes{SHA256: f.SHA256},
				RequiresPython: f.RequiresPython,
				Size:           f.Size,
			}
		}
		_ = json.NewEncoder(c.Writer).Encode(jsonPackage{
			Name:     pkg.Name,
			Meta:     jsonMeta{APIVersion: "1.0"},
			Versions: mergedVersions,
			Files:    jsonFiles,
		})
		return
	}

	c.Header("Content-Type", "text/html; charset=utf-8")
	_ = pkgIndexTmpl.Execute(c.Writer, struct {
		Name  string
		Files []fileEntry
	}{Name: pkg.Name, Files: files})
}

// download: GET /files/:name/:version/:filename
func (h *Handler) download(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !auth.RequireRead(c, tenant) {
		return
	}
	name := NormalizeName(c.Param("name"))
	version := c.Param("version")
	filename := c.Param("filename")

	pkg, err := h.Models.GetPackageByLookup(c.Request.Context(), tenant.ID, models.TypePyPI, name)
	if err != nil {
		// Pull-through on totally-unknown package.
		if h.passthroughEnabled(c, tenant) {
			ok, perr := h.pullThroughDownload(c, tenant, name, version, filename)
			if ok {
				return
			}
			if perr != nil {
				code, msg := mapUpstreamErr(perr)
				c.String(code, "%s", msg)
				return
			}
		}
		c.String(http.StatusNotFound, "not found")
		return
	}
	ver, err := h.Models.GetVersion(c.Request.Context(), pkg.ID, version)
	if err != nil {
		if h.passthroughEnabled(c, tenant) {
			ok, perr := h.pullThroughDownload(c, tenant, name, version, filename)
			if ok {
				return
			}
			if perr != nil {
				code, msg := mapUpstreamErr(perr)
				c.String(code, "%s", msg)
				return
			}
		}
		c.String(http.StatusNotFound, "not found")
		return
	}
	files, err := h.Models.ListFilesByVersion(c.Request.Context(), ver.ID)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	var match *models.File
	for _, f := range files {
		if strings.EqualFold(f.Name, filename) {
			match = f
			break
		}
	}
	if match == nil {
		if h.passthroughEnabled(c, tenant) {
			ok, perr := h.pullThroughDownload(c, tenant, name, version, filename)
			if ok {
				return
			}
			if perr != nil {
				code, msg := mapUpstreamErr(perr)
				c.String(code, "%s", msg)
				return
			}
		}
		c.String(http.StatusNotFound, "no such file")
		return
	}
	if !h.checkRead(c, tenant, pkg, ver, match.Name) {
		return
	}
	rc, _, err := h.Service.OpenFile(c.Request.Context(), match)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	defer rc.Close()
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, match.Name))
	_, _ = io.Copy(c.Writer, rc)
}

// --- Helpers ---------------------------------------------------------------

func (h *Handler) fetchBlobMeta(c *gin.Context, blobID int64) (int64, string) {
	row := h.Models.DB.QueryRowContext(c.Request.Context(),
		`SELECT size, hash_sha256 FROM package_blobs WHERE id = ?`, blobID)
	var (
		size int64
		sha  string
	)
	_ = row.Scan(&size, &sha)
	return size, sha
}

// extractHomepageURL pulls the project_urls[] entry labeled "Homepage" (with
// PEP-style label normalization).
func extractHomepageURL(projectURLs []string) string {
	for _, p := range projectURLs {
		label, url, ok := strings.Cut(p, ",")
		if !ok {
			continue
		}
		if normalizeProjectLabel(label) == "homepage" {
			return strings.TrimSpace(url)
		}
	}
	return ""
}

// normalizeProjectLabel applies the well-known-project-urls label
// normalization (strip punctuation and whitespace, lowercase).
func normalizeProjectLabel(label string) string {
	var b strings.Builder
	for _, r := range label {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r - 'A' + 'a')
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// wantsJSONSimple checks whether the client asked for PEP 691 JSON.
func wantsJSONSimple(r *http.Request) bool {
	for _, h := range r.Header["Accept"] {
		for _, part := range strings.Split(h, ",") {
			if strings.HasPrefix(strings.TrimSpace(part), "application/vnd.pypi.simple.v1+json") {
				return true
			}
		}
	}
	return false
}

// --- JSON shapes (PEP 691) -------------------------------------------------

type jsonHashes struct {
	SHA256 string `json:"sha256"`
}
type jsonFile struct {
	Filename       string     `json:"filename"`
	URL            string     `json:"url"`
	Hashes         jsonHashes `json:"hashes"`
	RequiresPython string     `json:"requires-python,omitempty"`
	Size           int64      `json:"size"`
}
type jsonMeta struct {
	APIVersion string `json:"api-version"`
}
type jsonPackage struct {
	Name     string     `json:"name"`
	Meta     jsonMeta   `json:"meta"`
	Versions []string   `json:"versions"`
	Files    []jsonFile `json:"files"`
}

// --- Policy hooks ----------------------------------------------------------

// subjectFor builds a Subject for one (package, version, filename) triple.
func (h *Handler) subjectFor(tenant *tenants.Tenant, pkg *models.Package, ver *models.Version, filename, license string) policy.Subject {
	s := policy.Subject{
		TenantID: tenant.ID,
		Format:   string(models.TypePyPI),
		Package:  pkg.LowerName,
		Filename: filename,
	}
	if ver != nil {
		s.Version = ver.Version
		s.Attrs = map[string]any{
			"created_unix":       ver.CreatedUnix,
			"ingest_age_seconds": time.Now().Unix() - ver.CreatedUnix,
		}
		// Surface upstream publish time when the pull-through adapter
		// captured it (see upstream.go stampUpstreamPublished). The
		// cooldown evaluator's time_source: upstream_publish mode reads
		// this; absent the attribute it falls back to ingest age.
		if ver.UpstreamPublishedUnix.Valid {
			s.Attrs["upstream_published_unix"] = ver.UpstreamPublishedUnix.Int64
		}
		// Caller-supplied license (upload form) takes precedence;
		// otherwise fall back to the value stored on the version row.
		if license != "" {
			s.Attrs["license"] = license
		} else if ver.License.Valid && ver.License.String != "" {
			s.Attrs["license"] = ver.License.String
		}
	}
	return s
}

func (h *Handler) checkRead(c *gin.Context, tenant *tenants.Tenant, pkg *models.Package, ver *models.Version, filename string) bool {
	r := h.Engine.Evaluate(c.Request.Context(), h.subjectFor(tenant, pkg, ver, filename, ""), policy.ActionRead)
	if r.IsBlocked() {
		c.String(http.StatusForbidden, "%s", policyReason(r))
		return false
	}
	return true
}

func (h *Handler) checkIngest(c *gin.Context, tenant *tenants.Tenant, lookupName, version, filename, license string) bool {
	subj := policy.Subject{
		TenantID: tenant.ID,
		Format:   string(models.TypePyPI),
		Package:  lookupName,
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

func (h *Handler) filterReadableVersions(c *gin.Context, tenant *tenants.Tenant, pkg *models.Package, versions []*models.Version) []*models.Version {
	out := versions[:0]
	for _, v := range versions {
		r := h.Engine.Evaluate(c.Request.Context(), h.subjectFor(tenant, pkg, v, "", ""), policy.ActionRead)
		if r.IsBlocked() {
			continue
		}
		out = append(out, v)
	}
	return out
}

// hasAnyReadableVersion reports whether at least one version of pkg passes
// the policy for ActionRead. Used to hide a package from the root index
// when all its versions are quarantined/denied.
func (h *Handler) hasAnyReadableVersion(c *gin.Context, tenant *tenants.Tenant, pkg *models.Package) bool {
	versions, err := h.Models.ListVersions(c.Request.Context(), pkg.ID)
	if err != nil || len(versions) == 0 {
		return false
	}
	for _, v := range versions {
		r := h.Engine.Evaluate(c.Request.Context(), h.subjectFor(tenant, pkg, v, "", ""), policy.ActionRead)
		if !r.IsBlocked() {
			return true
		}
	}
	return false
}

func policyReason(r policy.Result) string {
	if r.Reason == "" {
		return r.Decision.String()
	}
	return r.Reason
}

// --- Inline HTML templates -------------------------------------------------

var rootIndexTmpl = template.Must(template.New("root").Parse(
	`<!DOCTYPE html>
<html><head><meta name="pypi:repository-version" content="1.0"><title>Simple index</title></head>
<body>
{{range .}}<a href="{{.URL}}">{{.Name}}</a>
{{end}}</body></html>
`))

var pkgIndexTmpl = template.Must(template.New("pkg").Parse(
	`<!DOCTYPE html>
<html><head><meta name="pypi:repository-version" content="1.0"><title>Links for {{.Name}}</title></head>
<body>
<h1>Links for {{.Name}}</h1>
{{range .Files}}<a href="{{.URL}}#sha256={{.SHA256}}"{{if .RequiresPython}} data-requires-python="{{.RequiresPython}}"{{end}}>{{.Filename}}</a>
{{end}}</body></html>
`))
