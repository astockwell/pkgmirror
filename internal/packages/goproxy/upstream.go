// Package goproxy - upstream pull-through.
//
// Wires the internal/upstream fetcher into the Go module proxy handler
// so a `go get example.com/foo@v1.2.3` against an empty pkgmirror tenant
// fetches from proxy.golang.org, runs through the supply-chain policy
// engine, persists, and serves.
//
// Per the Go module proxy protocol (https://go.dev/ref/mod#goproxy-protocol),
// there are five endpoints:
//
//	GET /<module>/@v/list                  - newline-delimited versions
//	GET /<module>/@v/<version>.info        - JSON {"Version":..., "Time":...}
//	GET /<module>/@v/<version>.mod         - the module's go.mod
//	GET /<module>/@v/<version>.zip         - the module's source tree as zip
//	GET /<module>/@latest                  - JSON for the latest version
//
// All five gain a pull-through miss path. The publish time (`Time` in the
// .info response) goes straight into package_versions.upstream_published_unix
// without any second-hop API call - in contrast with PyPI where we have
// to do a separate Warehouse JSON fetch.
//
// `@latest` is treated as mutable metadata with a short TTL (5 minutes,
// per plans/upstream-pull-through.md §9.3 + the engineer-side decision
// to re-fetch on TTL expiry rather than serve stale-but-cached). The
// `@v/list` index is similarly mutable. Both flow through the upstream
// fetcher's metadata cache.
//
// sumdb (sum.golang.org) is INTENTIONALLY NOT proxied in v1. Operators
// who want sumdb verification should leave GOSUMDB pointing at the
// canonical sum.golang.org; pkgmirror only handles the proxy plane.
// See plans/upstream-pull-through.md §9.3 + the README on this trade-off.

package goproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/astockwell/pkgmirror/internal/models"
	pkgsvc "github.com/astockwell/pkgmirror/internal/packages"
	"github.com/astockwell/pkgmirror/internal/policy"
	"github.com/astockwell/pkgmirror/internal/tenants"
	"github.com/astockwell/pkgmirror/internal/upstream"

	"github.com/gin-gonic/gin"
)

// upstreamInfo is the shape of the .info JSON response from the Go
// module proxy. We only need Version + Time; later fields are ignored.
type upstreamInfo struct {
	Version string    `json:"Version"`
	Time    time.Time `json:"Time"`
}

// passthroughEnabled reports whether (tenant, "go") has pull-through on.
// Mirrors the PyPI helper of the same name.
func (h *Handler) passthroughEnabled(c *gin.Context, tenant *tenants.Tenant) bool {
	if h.Upstream == nil {
		return false
	}
	cfg, err := h.Upstream.ResolveConfig(c.Request.Context(), tenant.ID, "go")
	if err != nil {
		return false
	}
	return cfg.Mode != upstream.ModeOff
}

// fetchUpstreamList grabs the newline-delimited version list for module
// from the configured upstream. Returns ([]string of versions, nil) on
// success; ([], sentinel) on upstream off / not-found / transport error.
func (h *Handler) fetchUpstreamList(c *gin.Context, tenant *tenants.Tenant, module string) ([]string, error) {
	if h.Upstream == nil {
		return nil, upstream.ErrUpstreamOff
	}
	req := upstream.Request{
		TenantID:     tenant.ID,
		Format:       "go",
		Kind:         upstream.KindMetadata,
		UpstreamPath: "/" + module + "/@v/list",
		CanonicalKey: fmt.Sprintf("go:%d:%s:list", tenant.ID, module),
	}
	res, err := h.Upstream.Fetch(c.Request.Context(), req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("read upstream list: %w", err)
	}
	// Parse: one version per line; ignore empties.
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		v := strings.TrimSpace(l)
		if v != "" {
			out = append(out, v)
		}
	}
	return out, nil
}

// fetchUpstreamInfo grabs the .info JSON for (module, version). The
// .info response IS the upstream publish time - no second hop needed.
// Best-effort: any failure returns (nil, error). Cached by the
// fetcher's metadata cache with the configured TTL.
func (h *Handler) fetchUpstreamInfo(c *gin.Context, tenant *tenants.Tenant, module, version string) (*upstreamInfo, error) {
	if h.Upstream == nil {
		return nil, upstream.ErrUpstreamOff
	}
	req := upstream.Request{
		TenantID:     tenant.ID,
		Format:       "go",
		Kind:         upstream.KindMetadata,
		UpstreamPath: "/" + module + "/@v/" + version + ".info",
		CanonicalKey: fmt.Sprintf("go:%d:%s:%s:info", tenant.ID, module, version),
	}
	res, err := h.Upstream.Fetch(c.Request.Context(), req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("read upstream info: %w", err)
	}
	var info upstreamInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("decode upstream info: %w", err)
	}
	if info.Version == "" {
		info.Version = version
	}
	return &info, nil
}

// fetchUpstreamLatest grabs the @latest JSON from upstream. Identical
// shape to a .info response; separate helper so the canonical key is
// distinct (we want the metadata cache to honor the configured TTL
// independently for list / version-info / latest).
func (h *Handler) fetchUpstreamLatest(c *gin.Context, tenant *tenants.Tenant, module string) (*upstreamInfo, error) {
	if h.Upstream == nil {
		return nil, upstream.ErrUpstreamOff
	}
	req := upstream.Request{
		TenantID:     tenant.ID,
		Format:       "go",
		Kind:         upstream.KindMetadata,
		UpstreamPath: "/" + module + "/@latest",
		CanonicalKey: fmt.Sprintf("go:%d:%s:latest", tenant.ID, module),
	}
	res, err := h.Upstream.Fetch(c.Request.Context(), req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("read upstream @latest: %w", err)
	}
	var info upstreamInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("decode upstream @latest: %w", err)
	}
	return &info, nil
}

// fetchUpstreamModBytes returns the raw go.mod bytes for (module, version).
func (h *Handler) fetchUpstreamModBytes(c *gin.Context, tenant *tenants.Tenant, module, version string) ([]byte, error) {
	if h.Upstream == nil {
		return nil, upstream.ErrUpstreamOff
	}
	req := upstream.Request{
		TenantID:     tenant.ID,
		Format:       "go",
		Kind:         upstream.KindBlob, // immutable
		UpstreamPath: "/" + module + "/@v/" + version + ".mod",
		CanonicalKey: fmt.Sprintf("go:%d:%s:%s:mod", tenant.ID, module, version),
	}
	res, err := h.Upstream.Fetch(c.Request.Context(), req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	return io.ReadAll(res.Body)
}

// passthroughBlocked evaluates ActionRead for a not-yet-ingested
// upstream version, with the publish time hydrated from the info we
// just fetched. Mirrors the PyPI helper.
func (h *Handler) passthroughBlocked(c *gin.Context, tenant *tenants.Tenant, module, version string, pubUnix int64) bool {
	if h.Engine == nil {
		return false
	}
	subj := policy.Subject{
		TenantID: tenant.ID,
		Format:   string(models.TypeGo),
		Package:  strings.ToLower(module),
		Version:  version,
		Attrs: map[string]any{
			"ingest_age_seconds": int64(0),
			"created_unix":       time.Now().Unix(),
		},
	}
	if pubUnix > 0 {
		subj.Attrs["upstream_published_unix"] = pubUnix
	}
	r := h.Engine.Evaluate(c.Request.Context(), subj, policy.ActionRead)
	return r.IsBlocked()
}

// filterUpstreamVersions applies the policy engine to each upstream
// version, returning the subset that ActionRead permits. Fetches
// per-version .info to populate Subject.Attrs["upstream_published_unix"]
// for cooldown rules with time_source: upstream_publish - otherwise
// the fallback would block everything (ingest age = 0 for not-yet-
// ingested versions).
//
// Cost: one .info HTTP per version. Cached by the metadata cache, so
// the first list-miss for a module with N versions is N+1 round-trips;
// subsequent ones are cache hits. For most Go modules N is small. When
// the engine is the no-op (no rules ever), we skip the .info fan-out
// entirely.
func (h *Handler) filterUpstreamVersions(c *gin.Context, tenant *tenants.Tenant, module string, versions []string) []string {
	if h.Engine == nil {
		return versions
	}
	if _, isNoop := h.Engine.(policy.NoopEngine); isNoop {
		return versions
	}
	out := make([]string, 0, len(versions))
	for _, v := range versions {
		var pubUnix int64
		if info, err := h.fetchUpstreamInfo(c, tenant, module, v); err == nil {
			pubUnix = info.Time.Unix()
		}
		if h.passthroughBlocked(c, tenant, module, v, pubUnix) {
			continue
		}
		out = append(out, v)
	}
	return out
}

// servePullThroughList replies to /@v/list with the upstream version
// set, filtered through the policy engine. Same shape the local list
// would emit (one version per line, text/plain).
func (h *Handler) servePullThroughList(c *gin.Context, tenant *tenants.Tenant, module string) bool {
	versions, err := h.fetchUpstreamList(c, tenant, module)
	if err != nil {
		if errors.Is(err, upstream.ErrUpstreamOff) || errors.Is(err, upstream.ErrUpstreamNotFound) {
			return false
		}
		code, msg := mapUpstreamErr(err)
		c.String(code, "%s", msg)
		return true
	}
	visible := h.filterUpstreamVersions(c, tenant, module, versions)
	// Sort for deterministic output; go toolchain doesn't require it
	// but it makes tests + audits easier.
	sort.Strings(visible)
	c.Header("Content-Type", "text/plain; charset=utf-8")
	for _, v := range visible {
		fmt.Fprintln(c.Writer, v)
	}
	return true
}

// servePullThroughInfo replies to /@v/<version>.info with the upstream
// .info JSON, after evaluating policy. Quarantined / denied versions
// 403 with the same reason the post-ingest path would use.
//
// Side effect: on a non-blocked .info we eagerly fetch + persist the
// .mod + .zip too, so that subsequent requests for the same version
// hit the local cache. This matches `go get`'s real access pattern
// (info -> mod -> zip in close succession) and keeps the policy
// decision consistent across all three endpoints for a single version.
func (h *Handler) servePullThroughInfo(c *gin.Context, tenant *tenants.Tenant, module, version string) bool {
	info, err := h.fetchUpstreamInfo(c, tenant, module, version)
	if err != nil {
		if errors.Is(err, upstream.ErrUpstreamOff) || errors.Is(err, upstream.ErrUpstreamNotFound) {
			return false
		}
		code, msg := mapUpstreamErr(err)
		c.String(code, "%s", msg)
		return true
	}
	pubUnix := info.Time.Unix()

	// Pre-ingest deny check - short-circuit before any blob fetch.
	if h.Engine != nil {
		subj := policy.Subject{
			TenantID: tenant.ID,
			Format:   string(models.TypeGo),
			Package:  strings.ToLower(module),
			Version:  info.Version,
			Attrs: map[string]any{
				"ingest_age_seconds":      int64(0),
				"created_unix":            time.Now().Unix(),
				"upstream_published_unix": pubUnix,
			},
		}
		r := h.Engine.Evaluate(c.Request.Context(), subj, policy.ActionIngest)
		if r.Decision >= policy.Deny {
			c.String(http.StatusForbidden, "%s", policyReason(r))
			return true
		}
	}

	// Ingest + serve the zip side-effect (also populates go.mod).
	if err := h.pullThroughIngest(c, tenant, module, info.Version, pubUnix); err != nil {
		if errors.Is(err, upstream.ErrUpstreamNotFound) {
			return false
		}
		code, msg := mapUpstreamErr(err)
		c.String(code, "%s", msg)
		return true
	}

	// Post-ingest Read gate - catches quarantine action. Bytes stay
	// persisted (admin can promote later); the inflight client gets 403.
	if h.Engine != nil {
		subj := policy.Subject{
			TenantID: tenant.ID,
			Format:   string(models.TypeGo),
			Package:  strings.ToLower(module),
			Version:  info.Version,
			Attrs: map[string]any{
				"ingest_age_seconds":      int64(0),
				"created_unix":            time.Now().Unix(),
				"upstream_published_unix": pubUnix,
			},
		}
		r := h.Engine.Evaluate(c.Request.Context(), subj, policy.ActionRead)
		if r.IsBlocked() {
			c.String(http.StatusForbidden, "%s", policyReason(r))
			return true
		}
	}

	c.JSON(http.StatusOK, info)
	return true
}

// servePullThroughLatest replies to /@latest. Same fetch + policy gate
// as servePullThroughInfo but using the @latest upstream path so the
// metadata cache TTL controls how often we re-fetch.
func (h *Handler) servePullThroughLatest(c *gin.Context, tenant *tenants.Tenant, module string) bool {
	info, err := h.fetchUpstreamLatest(c, tenant, module)
	if err != nil {
		if errors.Is(err, upstream.ErrUpstreamOff) || errors.Is(err, upstream.ErrUpstreamNotFound) {
			return false
		}
		code, msg := mapUpstreamErr(err)
		c.String(code, "%s", msg)
		return true
	}
	pubUnix := info.Time.Unix()

	if h.passthroughBlocked(c, tenant, module, info.Version, pubUnix) {
		// Don't leak the version name; pretend there's no latest.
		// (A quarantined latest still means "we have nothing publicly
		// servable for this module".)
		c.String(http.StatusNotFound, "no readable version")
		return true
	}

	// Best-effort eager ingest so the follow-up info/mod/zip hits cache.
	// Failure here doesn't fail @latest itself - @latest can still answer.
	_ = h.pullThroughIngest(c, tenant, module, info.Version, pubUnix)

	c.JSON(http.StatusOK, info)
	return true
}

// servePullThroughMod replies to /@v/<version>.mod. If the version is
// known locally we go through the normal path; otherwise we fetch the
// upstream .info (for the policy check + publish stamp), gate, ingest
// the full version (mod + zip), then serve the persisted .mod.
func (h *Handler) servePullThroughMod(c *gin.Context, tenant *tenants.Tenant, module, version string) bool {
	info, err := h.fetchUpstreamInfo(c, tenant, module, version)
	if err != nil {
		if errors.Is(err, upstream.ErrUpstreamOff) || errors.Is(err, upstream.ErrUpstreamNotFound) {
			return false
		}
		code, msg := mapUpstreamErr(err)
		c.String(code, "%s", msg)
		return true
	}
	pubUnix := info.Time.Unix()

	if h.Engine != nil {
		subj := policy.Subject{
			TenantID: tenant.ID,
			Format:   string(models.TypeGo),
			Package:  strings.ToLower(module),
			Version:  info.Version,
			Attrs: map[string]any{
				"ingest_age_seconds":      int64(0),
				"created_unix":            time.Now().Unix(),
				"upstream_published_unix": pubUnix,
			},
		}
		r := h.Engine.Evaluate(c.Request.Context(), subj, policy.ActionIngest)
		if r.Decision >= policy.Deny {
			c.String(http.StatusForbidden, "%s", policyReason(r))
			return true
		}
	}

	if err := h.pullThroughIngest(c, tenant, module, info.Version, pubUnix); err != nil {
		if errors.Is(err, upstream.ErrUpstreamNotFound) {
			return false
		}
		code, msg := mapUpstreamErr(err)
		c.String(code, "%s", msg)
		return true
	}

	// Read gate (catches quarantine).
	if h.Engine != nil {
		subj := policy.Subject{
			TenantID: tenant.ID,
			Format:   string(models.TypeGo),
			Package:  strings.ToLower(module),
			Version:  info.Version,
			Attrs: map[string]any{
				"ingest_age_seconds":      int64(0),
				"created_unix":            time.Now().Unix(),
				"upstream_published_unix": pubUnix,
			},
		}
		r := h.Engine.Evaluate(c.Request.Context(), subj, policy.ActionRead)
		if r.IsBlocked() {
			c.String(http.StatusForbidden, "%s", policyReason(r))
			return true
		}
	}

	// Serve the now-persisted go.mod from the property store.
	pkg, err := h.Models.GetPackage(c.Request.Context(), tenant.ID, models.TypeGo, module)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return true
	}
	ver, err := h.Models.GetVersion(c.Request.Context(), pkg.ID, info.Version)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return true
	}
	goMod, ok, err := h.Models.GetProperty(c.Request.Context(), models.PropertyRefVersion, ver.ID, PropertyGoMod)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return true
	}
	if !ok {
		c.String(http.StatusNotFound, "go.mod not found")
		return true
	}
	c.Header("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(c.Writer, goMod)
	return true
}

// servePullThroughZip replies to /@v/<version>.zip with the same
// ingest + policy gate flow as the .mod path, then serves the
// persisted blob.
func (h *Handler) servePullThroughZip(c *gin.Context, tenant *tenants.Tenant, module, version string) bool {
	info, err := h.fetchUpstreamInfo(c, tenant, module, version)
	if err != nil {
		if errors.Is(err, upstream.ErrUpstreamOff) || errors.Is(err, upstream.ErrUpstreamNotFound) {
			return false
		}
		code, msg := mapUpstreamErr(err)
		c.String(code, "%s", msg)
		return true
	}
	pubUnix := info.Time.Unix()

	if h.Engine != nil {
		subj := policy.Subject{
			TenantID: tenant.ID,
			Format:   string(models.TypeGo),
			Package:  strings.ToLower(module),
			Version:  info.Version,
			Attrs: map[string]any{
				"ingest_age_seconds":      int64(0),
				"created_unix":            time.Now().Unix(),
				"upstream_published_unix": pubUnix,
			},
		}
		r := h.Engine.Evaluate(c.Request.Context(), subj, policy.ActionIngest)
		if r.Decision >= policy.Deny {
			c.String(http.StatusForbidden, "%s", policyReason(r))
			return true
		}
	}

	if err := h.pullThroughIngest(c, tenant, module, info.Version, pubUnix); err != nil {
		if errors.Is(err, upstream.ErrUpstreamNotFound) {
			return false
		}
		code, msg := mapUpstreamErr(err)
		c.String(code, "%s", msg)
		return true
	}

	if h.Engine != nil {
		subj := policy.Subject{
			TenantID: tenant.ID,
			Format:   string(models.TypeGo),
			Package:  strings.ToLower(module),
			Version:  info.Version,
			Attrs: map[string]any{
				"ingest_age_seconds":      int64(0),
				"created_unix":            time.Now().Unix(),
				"upstream_published_unix": pubUnix,
			},
		}
		r := h.Engine.Evaluate(c.Request.Context(), subj, policy.ActionRead)
		if r.IsBlocked() {
			c.String(http.StatusForbidden, "%s", policyReason(r))
			return true
		}
	}

	pkg, err := h.Models.GetPackage(c.Request.Context(), tenant.ID, models.TypeGo, module)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return true
	}
	ver, err := h.Models.GetVersion(c.Request.Context(), pkg.ID, info.Version)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return true
	}
	files, err := h.Models.ListFilesByVersion(c.Request.Context(), ver.ID)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return true
	}
	var fileRow *models.File
	for _, f := range files {
		if f.IsLead {
			fileRow = f
			break
		}
	}
	if fileRow == nil && len(files) > 0 {
		fileRow = files[0]
	}
	if fileRow == nil {
		c.String(http.StatusInternalServerError, "persisted zip vanished")
		return true
	}
	rc, _, err := h.Service.OpenFile(c.Request.Context(), fileRow)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return true
	}
	defer rc.Close()
	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, fileRow.Name))
	_, _ = io.Copy(c.Writer, rc)
	return true
}

// pullThroughIngest performs the actual fetch + persist for one
// version. Idempotent: a second call on an already-ingested version
// is a no-op (CreatePackageAndAddFile returns ErrDuplicatePackageVersion
// which we tolerate the same way the PyPI path does).
//
// Wraps three upstream calls: .mod (cheap text), .zip (the artifact).
// Publish time is supplied by the caller from the .info they already
// fetched, so this function does NOT re-fetch .info.
func (h *Handler) pullThroughIngest(c *gin.Context, tenant *tenants.Tenant, module, version string, pubUnix int64) error {
	if h.Upstream == nil {
		return upstream.ErrUpstreamOff
	}

	// Already persisted? Don't re-fetch.
	if pkg, err := h.Models.GetPackage(c.Request.Context(), tenant.ID, models.TypeGo, module); err == nil {
		if _, verr := h.Models.GetVersion(c.Request.Context(), pkg.ID, version); verr == nil {
			return nil
		}
	}

	goModBytes, err := h.fetchUpstreamModBytes(c, tenant, module, version)
	if err != nil {
		return fmt.Errorf("fetch upstream go.mod: %w", err)
	}

	// Now the zip. Stream into a HashedBuffer so the existing ingest
	// service can compute hashes for the blob store.
	zipReq := upstream.Request{
		TenantID:     tenant.ID,
		Format:       "go",
		Kind:         upstream.KindBlob,
		UpstreamPath: "/" + module + "/@v/" + version + ".zip",
		CanonicalKey: fmt.Sprintf("go:%d:%s:%s:zip", tenant.ID, module, version),
	}
	zipRes, err := h.Upstream.Fetch(c.Request.Context(), zipReq)
	if err != nil {
		return fmt.Errorf("fetch upstream zip: %w", err)
	}
	defer zipRes.Body.Close()

	buf, err := h.Service.NewHashedBuffer(zipRes.Body)
	if err != nil {
		return fmt.Errorf("buffer upstream zip: %w", err)
	}
	defer buf.Close()

	filename := fmt.Sprintf("%s.zip", version)
	_, _, _, err = h.Service.CreatePackageOrAddFileToExisting(c.Request.Context(), pkgsvc.CreationInfo{
		TenantID:    tenant.ID,
		PackageType: models.TypeGo,
		PackageName: module,
		Version:     version,
		VersionProperties: map[string]string{
			PropertyGoMod: string(goModBytes),
		},
		Filename:   filename,
		IsLead:     true,
		CreatedVia: models.CreatedViaPullThrough,
	}, buf)
	if err != nil && !errors.Is(err, models.ErrDuplicatePackageFile) {
		return fmt.Errorf("persist upstream zip: %w", err)
	}

	// Stamp upstream publish time when we have it.
	if pubUnix > 0 {
		pkg, perr := h.Models.GetPackage(c.Request.Context(), tenant.ID, models.TypeGo, module)
		if perr == nil {
			if ver, verr := h.Models.GetVersion(c.Request.Context(), pkg.ID, version); verr == nil {
				_ = h.Models.SetUpstreamPublishedUnix(c.Request.Context(), ver.ID, pubUnix)
			}
		}
	}
	return nil
}

// mapUpstreamErr converts an upstream sentinel into the matching HTTP
// status + short message body. Same shape as the PyPI helper of the
// same name; lifted into this file so goproxy doesn't import pypi.
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
