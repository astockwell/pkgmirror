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
	"time"

	"github.com/astockwell/pkgmirror/internal/models"
	pkgsvc "github.com/astockwell/pkgmirror/internal/packages"
	"github.com/astockwell/pkgmirror/internal/policy"
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
//
// Each upstream file is evaluated through the policy Engine with
// ActionRead before being included. This prevents cooldown / blocklist
// rules from being trivially bypassed by the cold-cache path - without
// this filter, uv would see every upstream version, pick the newest,
// and only discover the policy on the /files/ miss path (where today
// it isn't enforced either). When a cooldown rule uses
// time_source: upstream_publish, the filter does ONE extra package-
// level Warehouse JSON fetch (cached by the fetcher's metadata cache)
// to populate Subject.Attrs["upstream_published_unix"] so the rule can
// actually fire on first-sight of a fresh version.
func (h *Handler) servePulledThroughIndex(c *gin.Context, tenant *tenants.Tenant, lookupName string, pkg *upstreamSimplePackage) {
	entries, survivingVersions := h.upstreamFileEntries(c, tenant, lookupName, pkg, nil)
	// Mirror the file-level filter into the Versions[] list so PEP 691
	// consumers don't see ghost versions whose files were all dropped.
	versions := make([]string, 0, len(pkg.Versions))
	for _, v := range pkg.Versions {
		if _, ok := survivingVersions[v]; ok {
			versions = append(versions, v)
		}
	}
	if wantsJSONSimple(c.Request) {
		c.Header("Content-Type", "application/vnd.pypi.simple.v1+json")
		jsonFiles := make([]jsonFile, len(entries))
		for i, f := range entries {
			jsonFiles[i] = jsonFile{
				Filename: f.Filename,
				URL:      f.URL,
				Hashes:   jsonHashes{SHA256: f.SHA256},
			}
		}
		_ = json.NewEncoder(c.Writer).Encode(jsonPackage{
			Name:     pkg.Name,
			Meta:     jsonMeta{APIVersion: "1.0"},
			Versions: versions,
			Files:    jsonFiles,
		})
		return
	}
	c.Header("Content-Type", "text/html; charset=utf-8")
	_ = pulledThroughTmpl.Execute(c.Writer, struct {
		Name  string
		Files []pypiFileEntry
	}{Name: pkg.Name, Files: entries})
}

// pypiFileEntry is the shape both the local-rendering and pull-through
// paths feed into the PEP 503 / PEP 691 templates. Hoisted from a
// per-function local type so packageIndex can mix local + upstream
// entries into one rendered response.
type pypiFileEntry struct {
	Filename       string
	URL            string
	SHA256         string
	Size           int64
	RequiresPython string
}

// upstreamFileEntries fetches the upstream PEP 691 payload-derived
// entries, runs each through the policy engine (ActionRead) with
// upstream_published_unix hydrated when a non-noop engine is in play,
// and returns (entries, set of surviving version strings).
//
// skipFilenames lets callers (specifically packageIndex's merge path)
// drop upstream entries whose filename already exists locally - so
// the rendered index doesn't list two URLs for the same wheel. Empty
// or nil means "include everything that passes policy".
func (h *Handler) upstreamFileEntries(
	c *gin.Context,
	tenant *tenants.Tenant,
	lookupName string,
	pkg *upstreamSimplePackage,
	skipFilenames map[string]struct{},
) ([]pypiFileEntry, map[string]struct{}) {
	// When the engine is the no-op (tests, dev with no rules) skip
	// the Warehouse pre-fetch - it adds an upstream round-trip whose
	// only purpose is to feed an evaluator that won't reject anything.
	var publishedAt map[string]int64
	if _, isNoop := h.Engine.(policy.NoopEngine); !isNoop {
		publishedAt = h.fetchUpstreamPublishedAll(c, tenant, lookupName)
	}

	entries := make([]pypiFileEntry, 0, len(pkg.Files))
	survivingVersions := map[string]struct{}{}
	for _, f := range pkg.Files {
		if _, dup := skipFilenames[f.Filename]; dup {
			continue
		}
		// Best-effort version-from-filename to build the local URL.
		ver := versionFromFilename(pkg.Name, f.Filename)
		if ver == "" {
			ver = "0"
		}
		if h.Engine != nil && h.passthroughBlocked(c, tenant, lookupName, ver, f.Filename, publishedAt) {
			continue
		}
		entries = append(entries, pypiFileEntry{
			Filename: f.Filename,
			URL:      fmt.Sprintf("../../files/%s/%s/%s", pkg.Name, ver, f.Filename),
			SHA256:   f.Hashes["sha256"],
		})
		survivingVersions[ver] = struct{}{}
	}
	return entries, survivingVersions
}

// mergeUpstreamIntoLocalIndex is the merge-path helper packageIndex
// calls when (a) pull-through is enabled and (b) the package exists
// locally. Fetches the upstream PEP 691 index, runs each upstream
// file through the policy engine, and returns the additions that
// should be appended to the local file list (filenames already in
// local are deduped out).
//
// Best-effort: any upstream failure (off mode, not found, transport
// error) returns (nil, nil) and the caller serves local-only.
// Crucially this does NOT propagate upstream 5xx as a 502 - merging
// is a feature; falling back to the local view on upstream trouble is
// the right behavior for a known-local package.
//
// versionSet is the set of upstream versions whose files survived the
// policy filter; callers union this with their local versions list
// for the rendered Versions[] section.
func (h *Handler) mergeUpstreamIntoLocalIndex(
	c *gin.Context,
	tenant *tenants.Tenant,
	lookupName string,
	skipFilenames map[string]struct{},
) (entries []pypiFileEntry, versionSet map[string]struct{}) {
	if !h.passthroughEnabled(c, tenant) {
		return nil, nil
	}
	up, err := h.fetchUpstreamSimple(c, tenant, lookupName)
	if err != nil {
		// Upstream off, not found, transport error: silently fall
		// back to local-only. We have a local view that's perfectly
		// servable; this isn't a 5xx-worthy condition.
		return nil, nil
	}
	return h.upstreamFileEntries(c, tenant, lookupName, up, skipFilenames)
}

// passthroughBlocked builds a synthetic Subject for an upstream file we
// haven't ingested yet and asks the policy engine whether ActionRead
// would be blocked. The Subject carries ingest_age_seconds=0 (a fresh
// ingest is what *would* happen if uv asked for this file) plus the
// upstream publish time if we have it, so cooldown and blocklist rules
// fire identically to the post-ingest path.
func (h *Handler) passthroughBlocked(c *gin.Context, tenant *tenants.Tenant, lookupName, version, filename string, publishedAt map[string]int64) bool {
	subj := policy.Subject{
		TenantID: tenant.ID,
		Format:   string(models.TypePyPI),
		Package:  lookupName,
		Version:  version,
		Filename: filename,
		Attrs: map[string]any{
			"ingest_age_seconds": int64(0),
			"created_unix":       time.Now().Unix(),
		},
	}
	if pub, ok := publishedAt[version]; ok && pub > 0 {
		subj.Attrs["upstream_published_unix"] = pub
	}
	r := h.Engine.Evaluate(c.Request.Context(), subj, policy.ActionRead)
	return r.IsBlocked()
}

// pullThroughDownload finds the upstream entry for filename, fetches
// it via the upstream Fetcher (size-capped + allowlisted), verifies
// the sha256 if upstream provided one, and persists through the
// regular ingest pipeline so a second request hits the local cache.
//
// Returns (true, nil) on success after writing the file to c.Writer.
// pullThroughDownload finds the upstream entry for filename, fetches
// it via the upstream Fetcher (size-capped + allowlisted), verifies
// the sha256 if upstream provided one, and persists through the
// regular ingest pipeline so a second request hits the local cache.
//
// Returns (true, nil) when this function has written the HTTP response
// (either the file bytes on success, or a 403 when policy denies the
// ingest). (false, nil) when caller should fall through to a 404.
// (false, err) on an internal error - caller writes 502/500 with the
// message.
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

	// Resolve the version we'll persist under up-front so the policy
	// check, the persist call, and the Warehouse stamp all agree on it.
	useVersion := versionFromFilename(lookupName, filename)
	if useVersion == "" {
		useVersion = version
	}

	// Policy gate, evaluated BEFORE the blob fetch so a denied version
	// also saves the upstream bandwidth. The publish-time lookup is
	// best-effort: on failure the cooldown evaluator falls back to
	// ingest age (which for a not-yet-persisted ingest is 0, so an
	// ingest-source cooldown will still block - that's the correct
	// behavior for "everything is fresh").
	pubUnix, havePub := h.fetchUpstreamPublishedForVersion(c, tenant, lookupName, useVersion)
	if h.Engine != nil {
		subj := policy.Subject{
			TenantID: tenant.ID,
			Format:   string(models.TypePyPI),
			Package:  lookupName,
			Version:  useVersion,
			Filename: filename,
			Attrs: map[string]any{
				"ingest_age_seconds": int64(0),
				"created_unix":       time.Now().Unix(),
			},
		}
		if havePub {
			subj.Attrs["upstream_published_unix"] = pubUnix
		}
		r := h.Engine.Evaluate(c.Request.Context(), subj, policy.ActionIngest)
		if r.Decision >= policy.Deny {
			c.String(http.StatusForbidden, "%s", policyReason(r))
			return true, nil
		}
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

	// Best-effort: stamp the version with upstream's publish time so
	// the cooldown evaluator's time_source: upstream_publish mode can
	// gate by upstream age, not by ingest age. We already fetched this
	// up-front for the policy check; persist that value instead of
	// re-fetching. Fall back to the post-ingest Warehouse hop only
	// when the pre-fetch failed.
	if !ver.UpstreamPublishedUnix.Valid {
		if havePub {
			_ = h.Models.SetUpstreamPublishedUnix(c.Request.Context(), ver.ID, pubUnix)
		} else {
			h.stampUpstreamPublished(c, tenant, ver, lookupName, useVersion)
		}
	}

	// Post-ingest Read gate. Pre-ingest we only stopped on Deny so a
	// `quarantine`-action rule would let the wheel be persisted (which
	// is the documented quarantine semantic - "store, but hide"). The
	// inflight request, however, must NOT receive the bytes either,
	// or the very first downloader bypasses every quarantine. Hand
	// the future Read decision back as 403 with the same reason the
	// post-ingest /simple/ filter and /files/ direct path would use;
	// the persisted row stays so an admin can promote it later.
	if h.Engine != nil {
		readSubj := h.subjectFor(tenant, pkgRow, ver, filename, "")
		// subjectFor reads upstream_published_unix off the (already
		// in-memory) ver row, which was loaded BEFORE the stamp above
		// fired. Patch it in from the pubUnix we already know so the
		// time_source: upstream_publish branch evaluates the same
		// here as it does on a subsequent /files/ request that loads
		// a fresh ver row.
		if havePub {
			if readSubj.Attrs == nil {
				readSubj.Attrs = map[string]any{}
			}
			if _, set := readSubj.Attrs["upstream_published_unix"]; !set {
				readSubj.Attrs["upstream_published_unix"] = pubUnix
			}
		}
		r := h.Engine.Evaluate(c.Request.Context(), readSubj, policy.ActionRead)
		if r.IsBlocked() {
			c.String(http.StatusForbidden, "%s", policyReason(r))
			return true, nil
		}
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

// ----- Warehouse JSON two-hop for upstream_published_unix -----
//
// The PEP 691 / PEP 503 simple index does NOT carry an upload time
// per file. To populate package_versions.upstream_published_unix
// (which the cooldown rule's time_source=upstream_publish reads) we
// do a second fetch to PyPI's Warehouse JSON API:
//
//     GET https://pypi.org/pypi/<name>/<version>/json
//
// The response includes urls[] (one per artifact); each has
// upload_time_iso_8601 in RFC 3339 form (e.g. "2026-05-14T19:23:11.123456Z").
// We take the MIN of those - some packages publish wheels for
// different platforms over a span of hours; the earliest upload is
// when the version first existed upstream.
//
// This is best-effort: an upstream that doesn't speak Warehouse JSON
// (custom mirrors), a parse failure, or a network timeout all leave
// upstream_published_unix NULL and the cooldown evaluator falls back
// to ingest age (with a Reason annotation per PR G).

// warehouseURLs is the JSON shape of pypi.org/pypi/<name>/<version>/json's
// urls[] array. We only need the field that carries the upload time.
type warehouseURLs struct {
	URLs []struct {
		UploadTimeISO8601 string `json:"upload_time_iso_8601"`
	} `json:"urls"`
}

// stampUpstreamPublished fires the Warehouse two-hop and persists the
// result. Never returns an error - failure modes (network, parse,
// non-PyPI upstream) just leave the column NULL, and ingest age
// remains the fallback. Logged via the gin context for ops to spot
// in the access log; not surfaced to the client.
func (h *Handler) stampUpstreamPublished(c *gin.Context, tenant *tenants.Tenant, ver *models.Version, lookupName, version string) {
	if h.Upstream == nil {
		return
	}
	req := upstream.Request{
		TenantID:     tenant.ID,
		Format:       "pypi",
		Kind:         upstream.KindMetadata,
		UpstreamPath: "/pypi/" + lookupName + "/" + version + "/json",
		CanonicalKey: fmt.Sprintf("pypi:%d:%s:%s:warehouse", tenant.ID, lookupName, version),
	}
	res, err := h.Upstream.Fetch(c.Request.Context(), req)
	if err != nil {
		// Mirror doesn't speak Warehouse JSON, or transient failure.
		// Leave upstream_published_unix NULL.
		return
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return
	}
	var payload warehouseURLs
	if err := json.Unmarshal(body, &payload); err != nil {
		return
	}
	earliest := earliestUpload(payload)
	if earliest == 0 {
		return
	}
	// Persist; on race with DELETE we ignore ErrVersionNotExist.
	_ = h.Models.SetUpstreamPublishedUnix(c.Request.Context(), ver.ID, earliest)
}

// earliestUpload returns the smallest valid upload_time_iso_8601 from
// the Warehouse payload, as a unix timestamp. 0 when none parse.
func earliestUpload(p warehouseURLs) int64 {
	var min int64
	for _, u := range p.URLs {
		t, err := time.Parse(time.RFC3339, u.UploadTimeISO8601)
		if err != nil {
			// Warehouse uses fractional seconds + 'Z'; RFC3339 accepts
			// that. If it ever doesn't, try RFC3339Nano explicitly.
			t, err = time.Parse(time.RFC3339Nano, u.UploadTimeISO8601)
			if err != nil {
				continue
			}
		}
		ts := t.Unix()
		if ts <= 0 {
			continue
		}
		if min == 0 || ts < min {
			min = ts
		}
	}
	return min
}

// fetchUpstreamPublishedForVersion returns the earliest upstream upload
// time for (lookupName, version) as a unix timestamp. Best-effort: any
// failure (mirror doesn't speak Warehouse JSON, network blip, parse
// error) returns (0, false) so the caller can fall back to ingest age.
//
// Used by the pull-through ingest path to populate
// Subject.Attrs["upstream_published_unix"] BEFORE policy evaluation,
// and to stamp package_versions.upstream_published_unix without
// re-fetching post-ingest.
func (h *Handler) fetchUpstreamPublishedForVersion(c *gin.Context, tenant *tenants.Tenant, lookupName, version string) (int64, bool) {
	if h.Upstream == nil {
		return 0, false
	}
	req := upstream.Request{
		TenantID:     tenant.ID,
		Format:       "pypi",
		Kind:         upstream.KindMetadata,
		UpstreamPath: "/pypi/" + lookupName + "/" + version + "/json",
		CanonicalKey: fmt.Sprintf("pypi:%d:%s:%s:warehouse", tenant.ID, lookupName, version),
	}
	res, err := h.Upstream.Fetch(c.Request.Context(), req)
	if err != nil {
		return 0, false
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return 0, false
	}
	var payload warehouseURLs
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, false
	}
	t := earliestUpload(payload)
	if t == 0 {
		return 0, false
	}
	return t, true
}

// warehouseReleases is the JSON shape of pypi.org/pypi/<name>/json
// for the bits we need: a releases object keyed by version, each
// holding one or more uploaded files.
type warehouseReleases struct {
	Releases map[string][]struct {
		UploadTimeISO8601 string `json:"upload_time_iso_8601"`
	} `json:"releases"`
}

// fetchUpstreamPublishedAll fetches the package-level Warehouse JSON
// (/pypi/<name>/json) and returns a map of version → earliest upload
// time. Used by the cold pull-through /simple/ filter to populate
// Subject.Attrs["upstream_published_unix"] for every candidate file in
// one round-trip, so cooldown rules with time_source: upstream_publish
// can run BEFORE any download (otherwise uv just gets the full
// upstream catalog and only sees the policy on download miss, by
// which point the resolver has already locked in a denied version).
//
// Best-effort: any failure returns nil. Cached by the fetcher's
// metadata cache so subsequent cold /simple/ requests for the same
// package within TTL are free.
func (h *Handler) fetchUpstreamPublishedAll(c *gin.Context, tenant *tenants.Tenant, lookupName string) map[string]int64 {
	if h.Upstream == nil {
		return nil
	}
	req := upstream.Request{
		TenantID:     tenant.ID,
		Format:       "pypi",
		Kind:         upstream.KindMetadata,
		UpstreamPath: "/pypi/" + lookupName + "/json",
		CanonicalKey: fmt.Sprintf("pypi:%d:%s:warehouse-all", tenant.ID, lookupName),
	}
	res, err := h.Upstream.Fetch(c.Request.Context(), req)
	if err != nil {
		return nil
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil
	}
	var payload warehouseReleases
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	if len(payload.Releases) == 0 {
		return nil
	}
	out := make(map[string]int64, len(payload.Releases))
	for ver, files := range payload.Releases {
		var min int64
		for _, f := range files {
			t, err := time.Parse(time.RFC3339, f.UploadTimeISO8601)
			if err != nil {
				t, err = time.Parse(time.RFC3339Nano, f.UploadTimeISO8601)
				if err != nil {
					continue
				}
			}
			ts := t.Unix()
			if ts <= 0 {
				continue
			}
			if min == 0 || ts < min {
				min = ts
			}
		}
		if min > 0 {
			out[ver] = min
		}
	}
	return out
}
