// Package pypi - upstream pull-through.
//
// Wires the internal/upstream fetcher into the PyPI handler so:
//
//   - GET /simple/:name/  on a totally-unknown package fetches the
//     PEP 691 JSON index from pypi.org, rewrites every file URL to
//     point at our own /files/... endpoint, and serves it. Nothing is
//     persisted yet.
//
//   - GET /files/:name/:version/:filename  on a not-yet-local file
//     re-fetches the PEP 691 JSON, finds the matching entry,
//     verifies it advertises the expected filename, fetches the blob
//     over the fetcher (size-capped, allowlist-gated, single-flighted),
//     ingests it through the same code path twine upload would, then
//     serves the now-local file.
//
// We deliberately do NOT merge a local-then-upstream view. If a tenant
// has uploaded any version of a package privately, that package is
// "owned" and the standard local-only index serves. Pull-through only
// kicks in when local has nothing for that (package | file) tuple.
// That keeps the demo flow predictable and avoids the edge cases that
// surround index merging (yanked versions, hash mismatches between
// upstream and local, etc.).
package pypi

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strings"

	"github.com/astockwell/pkgmirror/internal/models"
	pkgsvc "github.com/astockwell/pkgmirror/internal/packages"
	"github.com/astockwell/pkgmirror/internal/tenants"
	"github.com/astockwell/pkgmirror/internal/upstream"

	"github.com/gin-gonic/gin"
)

// upstreamSimpleFile is one entry in the PEP 691 JSON response. We
// only parse the fields we need; the rest are ignored.
type upstreamSimpleFile struct {
	Filename string            `json:"filename"`
	URL      string            `json:"url"`
	Hashes   map[string]string `json:"hashes"`
}

type upstreamSimplePackage struct {
	Name     string               `json:"name"`
	Versions []string             `json:"versions"`
	Files    []upstreamSimpleFile `json:"files"`
}

// fetchUpstreamSimple grabs the PEP 691 JSON for lookupName from the
// resolved upstream URL. Returns parsed payload or a sentinel error
// the caller can use to map to HTTP status.
func (h *Handler) fetchUpstreamSimple(c *gin.Context, tenant *tenants.Tenant, lookupName string) (*upstreamSimplePackage, error) {
	if h.Upstream == nil {
		return nil, upstream.ErrUpstreamOff
	}
	req := upstream.Request{
		TenantID:     tenant.ID,
		Format:       "pypi",
		Kind:         upstream.KindMetadata,
		UpstreamPath: "/simple/" + lookupName + "/",
		CanonicalKey: fmt.Sprintf("pypi:%d:%s:simple", tenant.ID, lookupName),
	}
	res, err := h.Upstream.Fetch(c.Request.Context(), req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	// We always force PEP 691 via the Accept header. The default
	// fetcher doesn't set custom headers so we have to do a second
	// request via the fetcher's Fetch... actually no. The fetcher
	// doesn't expose Accept-setting today, so we fall back to parsing
	// HTML if the response is HTML. Most PyPI mirrors honor JSON when
	// the URL ends with /json, but PEP 691 wants content negotiation.
	//
	// For v1 demo: parse JSON when content type matches, otherwise
	// parse HTML.
	if isJSONIndex(res.ContentType) {
		var payload upstreamSimplePackage
		if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
			return nil, fmt.Errorf("decode upstream JSON: %w", err)
		}
		return &payload, nil
	}
	// HTML fallback. Read and parse <a href="..." #sha256=...">filename</a>.
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("read upstream HTML: %w", err)
	}
	return parseSimpleHTML(lookupName, body), nil
}

// isJSONIndex matches Content-Type for PEP 691 JSON.
func isJSONIndex(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.Contains(ct, "application/vnd.pypi.simple.v1+json") ||
		strings.Contains(ct, "application/json")
}

// parseSimpleHTML extracts files from a PEP 503 HTML index. Tiny
// hand-rolled parser; we only look at <a href="..."#sha256=hex"> tags.
// Good enough for pypi.org's output; if a downstream mirror emits
// exotic HTML, the operator can configure that mirror to return JSON.
func parseSimpleHTML(lookupName string, body []byte) *upstreamSimplePackage {
	pkg := &upstreamSimplePackage{Name: lookupName}
	versionSeen := map[string]struct{}{}
	s := string(body)
	for {
		i := strings.Index(s, "<a ")
		if i < 0 {
			break
		}
		s = s[i:]
		end := strings.Index(s, "</a>")
		if end < 0 {
			break
		}
		tag := s[:end+4]
		s = s[end+4:]

		href := attrValue(tag, "href")
		if href == "" {
			continue
		}
		// Filename = text between > and </a>.
		gt := strings.Index(tag, ">")
		if gt < 0 || gt+1 >= len(tag)-4 {
			continue
		}
		fname := strings.TrimSpace(tag[gt+1 : len(tag)-4])
		if fname == "" {
			continue
		}
		// Pull #sha256=hex out of href.
		url, frag, _ := strings.Cut(href, "#")
		hashes := map[string]string{}
		if frag != "" {
			if k, v, ok := strings.Cut(frag, "="); ok && k == "sha256" {
				hashes["sha256"] = v
			}
		}
		pkg.Files = append(pkg.Files, upstreamSimpleFile{
			Filename: fname,
			URL:      url,
			Hashes:   hashes,
		})
		// Best-effort version extraction from filename.
		if v := versionFromFilename(lookupName, fname); v != "" {
			if _, dup := versionSeen[v]; !dup {
				versionSeen[v] = struct{}{}
				pkg.Versions = append(pkg.Versions, v)
			}
		}
	}
	return pkg
}

// attrValue does a tiny case-insensitive HTML attribute lookup. Good
// enough for index parsing; we don't need a full HTML parser.
func attrValue(tag, attr string) string {
	lower := strings.ToLower(tag)
	key := attr + "="
	i := strings.Index(lower, key)
	if i < 0 {
		return ""
	}
	rest := tag[i+len(key):]
	if len(rest) < 1 {
		return ""
	}
	q := rest[0]
	if q != '"' && q != '\'' {
		return ""
	}
	end := strings.IndexByte(rest[1:], q)
	if end < 0 {
		return ""
	}
	return rest[1 : 1+end]
}

// versionFromFilename heuristically extracts the version from a PEP
// 491 wheel name or sdist name. Returns "" when ambiguous; the index
// is still served correctly even if Versions is empty (it's only used
// for the rendered local /simple page after first ingest).
func versionFromFilename(lookupName, filename string) string {
	low := strings.TrimSuffix(strings.TrimSuffix(filename, ".whl"), ".tar.gz")
	low = strings.TrimSuffix(low, ".zip")
	// Wheel: name-version-pyver-abi-platform.whl
	parts := strings.Split(low, "-")
	if len(parts) >= 2 {
		// Find the boundary between name-tokens and version. Walk
		// forward from the start of parts; if parts[i] starts with a
		// digit we treat it as the version.
		for i := 1; i < len(parts); i++ {
			if len(parts[i]) > 0 && parts[i][0] >= '0' && parts[i][0] <= '9' {
				return parts[i]
			}
		}
	}
	_ = lookupName
	return ""
}

// servePulledThroughIndex emits a PEP 503 HTML index built from the
// upstream payload but with every URL rewritten to point at our own
// /files/ endpoint. The client (pip/uv) will hit us back, and the
// download miss path will pull each file through on demand.
//
// Tenant name is in the URL path so the rewritten links are absolute-
// relative to the API root. We use ../../files/... (matching the
// hand-uploaded path style elsewhere in the handler).
func (h *Handler) servePulledThroughIndex(c *gin.Context, pkg *upstreamSimplePackage) {
	// Build the same structure the local rendering uses so the
	// existing inline template can render it. Pull SHA-256 out of
	// hashes; everything else (version, requires-python) we leave
	// blank - pip/uv read the hash from the URL fragment regardless.
	type fileEntry struct {
		Filename       string
		URL            string
		SHA256         string
		Size           int64
		RequiresPython string
	}
	files := make([]fileEntry, 0, len(pkg.Files))
	for _, f := range pkg.Files {
		// Best-effort version-from-filename to build the local URL.
		ver := versionFromFilename(pkg.Name, f.Filename)
		if ver == "" {
			ver = "0"
		}
		files = append(files, fileEntry{
			Filename: f.Filename,
			URL:      fmt.Sprintf("../../files/%s/%s/%s", pkg.Name, ver, f.Filename),
			SHA256:   f.Hashes["sha256"],
		})
	}
	if wantsJSONSimple(c.Request) {
		c.Header("Content-Type", "application/vnd.pypi.simple.v1+json")
		jsonFiles := make([]jsonFile, len(files))
		for i, f := range files {
			jsonFiles[i] = jsonFile{
				Filename: f.Filename,
				URL:      f.URL,
				Hashes:   jsonHashes{SHA256: f.SHA256},
			}
		}
		_ = json.NewEncoder(c.Writer).Encode(jsonPackage{
			Name:     pkg.Name,
			Meta:     jsonMeta{APIVersion: "1.0"},
			Versions: pkg.Versions,
			Files:    jsonFiles,
		})
		return
	}
	c.Header("Content-Type", "text/html; charset=utf-8")
	_ = pulledThroughTmpl.Execute(c.Writer, struct {
		Name  string
		Files []fileEntry
	}{Name: pkg.Name, Files: files})
}

// pullThroughDownload finds the upstream entry for filename, fetches
// it via the upstream Fetcher (size-capped + allowlisted), verifies
// the sha256 if upstream provided one, and persists through the
// regular ingest pipeline so a second request hits the local cache.
//
// Returns (true, nil) on success after writing the file to c.Writer.
// (false, nil) when caller should fall through to a 404. (false, err)
// on an internal error - caller writes 502/500 with the message.
func (h *Handler) pullThroughDownload(c *gin.Context, tenant *tenants.Tenant, lookupName, version, filename string) (bool, error) {
	if h.Upstream == nil {
		return false, nil
	}

	pkg, err := h.fetchUpstreamSimple(c, tenant, lookupName)
	if err != nil {
		if errors.Is(err, upstream.ErrUpstreamOff) || errors.Is(err, upstream.ErrUpstreamNotFound) {
			return false, nil
		}
		return false, err
	}

	var entry *upstreamSimpleFile
	for i := range pkg.Files {
		if strings.EqualFold(pkg.Files[i].Filename, filename) {
			entry = &pkg.Files[i]
			break
		}
	}
	if entry == nil {
		return false, nil
	}

	blobReq := upstream.Request{
		TenantID:     tenant.ID,
		Format:       "pypi",
		Kind:         upstream.KindBlob,
		UpstreamPath: entry.URL,
		CanonicalKey: fmt.Sprintf("pypi:%d:%s:%s:%s", tenant.ID, lookupName, version, filename),
	}
	res, err := h.Upstream.Fetch(c.Request.Context(), blobReq)
	if err != nil {
		if errors.Is(err, upstream.ErrUpstreamNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("fetch upstream blob: %w", err)
	}
	defer res.Body.Close()

	// Buffer + hash for the ingest path.
	buf, err := h.Service.NewHashedBuffer(res.Body)
	if err != nil {
		return false, fmt.Errorf("buffer upstream blob: %w", err)
	}
	defer buf.Close()
	_, _, sha256Hex, _ := buf.Sums()
	if want := entry.Hashes["sha256"]; want != "" && !strings.EqualFold(want, sha256Hex) {
		return false, fmt.Errorf("sha256 mismatch: upstream=%s got=%s", want, sha256Hex)
	}

	// Use the entry's parsed version if we have a confident one;
	// fall back to the client-provided URL :version param otherwise.
	useVersion := versionFromFilename(lookupName, filename)
	if useVersion == "" {
		useVersion = version
	}

	_, _, _, err = h.Service.CreatePackageOrAddFileToExisting(c.Request.Context(), pkgsvc.CreationInfo{
		TenantID:          tenant.ID,
		PackageType:       models.TypePyPI,
		PackageName:       lookupName,
		PackageLookupName: lookupName,
		Version:           useVersion,
		Filename:          filename,
		IsLead:            true,
	}, buf)
	if err != nil && !errors.Is(err, models.ErrDuplicatePackageFile) {
		return false, fmt.Errorf("persist upstream blob: %w", err)
	}

	// Now serve from the local file. We have to re-look it up since
	// CreatePackageOrAddFileToExisting doesn't return the *models.File.
	pkgRow, err := h.Models.GetPackageByLookup(c.Request.Context(), tenant.ID, models.TypePyPI, lookupName)
	if err != nil {
		return false, fmt.Errorf("re-lookup persisted package: %w", err)
	}
	ver, err := h.Models.GetVersion(c.Request.Context(), pkgRow.ID, useVersion)
	if err != nil {
		return false, fmt.Errorf("re-lookup persisted version: %w", err)
	}
	files, err := h.Models.ListFilesByVersion(c.Request.Context(), ver.ID)
	if err != nil {
		return false, fmt.Errorf("list persisted files: %w", err)
	}
	var fileRow *models.File
	for _, f := range files {
		if strings.EqualFold(f.Name, filename) {
			fileRow = f
			break
		}
	}
	if fileRow == nil {
		return false, fmt.Errorf("persisted file vanished from DB")
	}

	rc, _, err := h.Service.OpenFile(c.Request.Context(), fileRow)
	if err != nil {
		return false, fmt.Errorf("open persisted blob: %w", err)
	}
	defer rc.Close()
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, fileRow.Name))
	if _, err := io.Copy(c.Writer, rc); err != nil {
		// Headers already sent; nothing useful to do.
		_ = err
	}
	return true, nil
}

// mapUpstreamErr translates a fetcher error into an HTTP status +
// message. Used by both the index and download miss paths so the
// behavior is consistent.
func mapUpstreamErr(err error) (int, string) {
	switch {
	case errors.Is(err, upstream.ErrUpstreamOff),
		errors.Is(err, upstream.ErrUpstreamNotFound):
		return http.StatusNotFound, "not found"
	case errors.Is(err, upstream.ErrUpstreamRateLimit):
		return http.StatusTooManyRequests, "upstream pull-through rate limit"
	case errors.Is(err, upstream.ErrUpstreamTooLarge):
		return http.StatusBadGateway, "upstream response exceeded size limit"
	case errors.Is(err, upstream.ErrUpstreamForbidden):
		return http.StatusBadGateway, "upstream URL not permitted (allowlist or private-IP guard)"
	case errors.Is(err, upstream.ErrUpstreamTimeout):
		return http.StatusGatewayTimeout, "upstream pull-through timed out"
	default:
		return http.StatusBadGateway, "upstream pull-through failed: " + err.Error()
	}
}

// passthroughCheckMode reads the resolved tenant_upstreams config and
// returns true when pull-through is permitted (mode != off). False
// (with no HTTP write) when ops have opted out for this (tenant, pypi).
func (h *Handler) passthroughEnabled(c *gin.Context, tenant *tenants.Tenant) bool {
	if h.Upstream == nil {
		return false
	}
	cfg, err := h.Upstream.ResolveConfig(c.Request.Context(), tenant.ID, "pypi")
	if err != nil {
		return false
	}
	return cfg.Mode != upstream.ModeOff
}

// pulledThroughTmpl renders the proxied index. Same shape as
// pkgIndexTmpl in handler.go so pip/uv see the same format.
var pulledThroughTmpl = template.Must(template.New("upstream").Parse(
	`<!DOCTYPE html>
<html><head><meta name="pypi:repository-version" content="1.0"><title>Links for {{.Name}}</title></head>
<body>
<h1>Links for {{.Name}}</h1>
{{range .Files}}<a href="{{.URL}}#sha256={{.SHA256}}">{{.Filename}}</a>
{{end}}</body></html>
`))
