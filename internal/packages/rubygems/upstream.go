// Package rubygems - upstream pull-through.
//
// Wires the internal/upstream fetcher into the RubyGems handler so:
//
//   - GET /info/:package on a totally-unknown package fetches the
//     compact-index info file from index.rubygems.org, runs each
//     advertised version through the policy engine (with publish
//     times hydrated from rubygems.org's gem JSON API), and serves
//     the filtered compact-index lines. Nothing is persisted yet.
//
//   - GET /gems/:filename on a not-yet-local file re-fetches the
//     compact-index info to pluck out the SHA256 of the wanted
//     filename, runs the synthetic Subject through the policy
//     engine BEFORE the blob fetch, then fetches the .gem,
//     verifies the SHA256 byte-for-byte, ingests it through the
//     regular pipeline, runs the post-ingest Read gate (catches
//     quarantine), then serves.
//
// Same shape as the PyPI pull-through. RubyGems and PyPI both need
// a two-hop because publish times don't live in the compact index;
// rubygems.org/api/v1/gems/<name>.json is the analog of PyPI's
// Warehouse JSON.
//
// We deliberately do NOT proxy the legacy /specs.4.8.gz family
// (full-registry indexes are tens of MB and change constantly) or
// the global /versions file (would need its own bandersnatch-style
// sync feature). See plans/rubygems-pull-through.md §2 + §6 for the
// scope rationale.
//
// v1 limitation: ruby-platform gems only on cold miss. The filename
// format is "<name>-<version>.gem" for ruby and
// "<name>-<version>-<platform>.gem" for platform gems, and reverse-
// parsing the second case is ambiguous without first calling /info.
// Cold requests for platform-tagged filenames 404 with a clear
// message; users can `gem push` them manually. Follow-up:
// plans/rubygems-pull-through.md §8.
package rubygems

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
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

// compactIndexInfo is the parsed form of one upstream /info/<gem> file.
// Lines have the form:
//
//	<version>[-<platform>] <deps>|checksum:<sha256>[,ruby:<req>][,rubygems:<req>]
//
// We only need (version, platform, checksum) for the pull-through
// flow; deps + ruby/rubygems version requirements come along for
// the ride when we re-serve the filtered index to clients.
type compactIndexInfo struct {
	// Lines is the raw compact-index file split + parsed. Order matters
	// (clients use the order to determine version precedence in some
	// flows), so we preserve upstream's ordering verbatim.
	Lines []compactIndexLine
}

// compactIndexLine is one parsed line of a /info/<gem> response.
type compactIndexLine struct {
	Version  string // e.g. "1.0.0"
	Platform string // "" for the default ruby platform; "x86_64-linux" etc. otherwise
	Checksum string // sha256 hex
	Raw      string // the unmodified upstream line, used when re-serving
}

// upstreamGemJSON is the slice of rubygems.org/api/v1/gems/<name>.json
// we care about. The full API returns substantially more; we ignore
// the rest.
//
// The endpoint returns the LATEST version's metadata plus a versions
// array we don't get directly here; for the package-level fan-out we
// instead use /api/v1/versions/<name>.json which returns one entry
// per version with the publish time inline.
type upstreamGemVersion struct {
	Number      string `json:"number"`
	Platform    string `json:"platform"`
	CreatedAt   string `json:"created_at"`   // RFC3339, e.g. "2024-05-14T19:23:11.123Z"
	RubygemsURI string `json:"rubygems_uri"` // optional; some old entries omit
}

// passthroughEnabled reports whether the (tenant, "rubygems") tuple
// has pull-through on. Mirror of the PyPI helper of the same name;
// body is identical.
func (h *Handler) passthroughEnabled(c *gin.Context, tenant *tenants.Tenant) bool {
	if h.Upstream == nil {
		return false
	}
	cfg, err := h.Upstream.ResolveConfig(c.Request.Context(), tenant.ID, "rubygems")
	if err != nil {
		return false
	}
	return cfg.Mode != upstream.ModeOff
}

// fetchUpstreamInfo grabs the compact-index /info/<name> for the
// package and parses it into compactIndexInfo. Cached by the fetcher's
// metadata cache. Best-effort: any failure returns the sentinel error
// the caller can map to an HTTP status.
//
// In production this hits rubygems.org/info/<name>, which 301-redirects
// to index.rubygems.org/info/<name>. Both hosts are in the default
// allowlist (defaults.go) so the fetcher follows the redirect.
func (h *Handler) fetchUpstreamInfo(c *gin.Context, tenant *tenants.Tenant, name string) (*compactIndexInfo, error) {
	if h.Upstream == nil {
		return nil, upstream.ErrUpstreamOff
	}
	req := upstream.Request{
		TenantID:     tenant.ID,
		Format:       "rubygems",
		Kind:         upstream.KindMetadata,
		UpstreamPath: "/info/" + name,
		CanonicalKey: fmt.Sprintf("rubygems:%d:%s:info", tenant.ID, name),
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
	info, perr := parseCompactIndex(body)
	if perr != nil {
		return nil, fmt.Errorf("parse upstream info: %w", perr)
	}
	return info, nil
}

// parseCompactIndex parses a /info/<gem> response body into the
// structured form. The first line is always "---", subsequent lines
// are one-per-version. Tolerant: malformed lines are skipped (logged
// would be nice but we have no logger here; the upstream is
// occasionally lax about its own format).
func parseCompactIndex(body []byte) (*compactIndexInfo, error) {
	info := &compactIndexInfo{}
	sc := bufio.NewScanner(strings.NewReader(string(body)))
	// Compact-index files can have very long lines for packages with
	// many deps; default scanner buffer is 64KB which is usually
	// enough, but rails et al. occasionally exceed it. Bump to 1MB.
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	first := true
	for sc.Scan() {
		line := sc.Text()
		if first {
			first = false
			// The leading "---" is a YAML-ish header. Tolerate its
			// absence (some mirrors omit it).
			if line == "---" {
				continue
			}
		}
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		parsed, ok := parseCompactIndexLine(line)
		if !ok {
			continue
		}
		info.Lines = append(info.Lines, parsed)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return info, nil
}

// parseCompactIndexLine extracts (version, platform, checksum) from a
// single compact-index line:
//
//	<version>[-<platform>] <deps>|<extras>
//
// where <extras> is a comma-separated list including "checksum:<hex>".
// Returns (line, true) on success, zero-value + false on parse failure.
func parseCompactIndexLine(line string) (compactIndexLine, bool) {
	out := compactIndexLine{Raw: line}
	// Split into "<version-and-platform>" + " " + "<rest>".
	sp := strings.IndexByte(line, ' ')
	if sp < 0 {
		// Lines with no dependencies still have a trailing space + "|<extras>";
		// no space means malformed.
		return out, false
	}
	vp := line[:sp]
	rest := line[sp+1:]
	// Pull out (version, platform). Platform is whatever comes after
	// the FIRST '-' that's followed by a non-digit, because versions
	// can contain '-' too (pre-release tags like "1.0.0-beta1").
	out.Version, out.Platform = splitVersionPlatform(vp)
	// Find "|<extras>" and pull "checksum:<hex>" out.
	pipe := strings.IndexByte(rest, '|')
	if pipe < 0 {
		return out, false
	}
	extras := rest[pipe+1:]
	for _, item := range strings.Split(extras, ",") {
		k, v, ok := strings.Cut(item, ":")
		if ok && k == "checksum" {
			out.Checksum = v
			break
		}
	}
	if out.Version == "" || out.Checksum == "" {
		return out, false
	}
	return out, true
}

// splitVersionPlatform separates the "<version>[-<platform>]" prefix
// into its two components. Conservative: if there's no platform we
// return ("", "") for the platform half rather than guessing.
//
// Heuristic: a version is one-or-more dot-separated digit-led tokens
// optionally followed by pre-release tags. We walk hyphen boundaries
// from the right: the FIRST hyphen whose right side is not a typical
// version token (e.g. "x86_64-linux", "java", "universal-darwin") is
// the version/platform boundary.
//
// We use the simpler form: take everything up to the last hyphen that
// produces a "digity" right side. If no such hyphen exists, no
// platform. This handles "1.0.0-beta1" (no platform, all-digity) and
// "1.16.0-x86_64-linux" (platform present, "x86_64-linux"). Edge
// cases that fall through (custom pre-release naming) are rare in
// the wild and the worst case is that platform stays "" and the
// filename mismatch surfaces at .gem fetch time.
func splitVersionPlatform(s string) (version, platform string) {
	// Walk hyphens left-to-right. The first hyphen whose right side
	// starts with a non-digit, non-pre-release-tag character likely
	// indicates a platform boundary.
	for i := 0; i < len(s); i++ {
		if s[i] != '-' {
			continue
		}
		right := s[i+1:]
		if right == "" {
			break
		}
		// Pre-release tags ("beta1", "rc.1", "alpha", "pre.5") tend to
		// be lowercase letters + digits. Platform tags ("x86_64-linux",
		// "java", "universal-darwin") tend to contain underscores or
		// be deeper hyphenated. Heuristic: presence of '_' or a second
		// '-' inside the right side implies platform.
		if strings.ContainsAny(right, "_") || strings.ContainsRune(right, '-') {
			return s[:i], right
		}
		// Single-token right side: "java", "mswin32". Treat as platform
		// when it's all-letters (or starts with one). Versions/pre-
		// releases always start with a digit.
		if !isAsciiDigit(right[0]) {
			return s[:i], right
		}
	}
	return s, ""
}

func isAsciiDigit(b byte) bool { return b >= '0' && b <= '9' }

// fetchUpstreamVersions grabs the per-version metadata array from
// rubygems.org/api/v1/versions/<name>.json. Returns a map of
// "<version>[-<platform>]" -> unix-timestamp. Best-effort: any
// failure returns nil so cooldown falls back to ingest age.
//
// One round-trip per cold package hydrates upstream_published_unix
// for EVERY version in the compact index. Analog of PyPI's
// /pypi/<name>/json fetch. Metadata-cached.
func (h *Handler) fetchUpstreamVersions(c *gin.Context, tenant *tenants.Tenant, name string) map[string]int64 {
	if h.Upstream == nil {
		return nil
	}
	req := upstream.Request{
		TenantID:     tenant.ID,
		Format:       "rubygems",
		Kind:         upstream.KindMetadata,
		UpstreamPath: "/api/v1/versions/" + name + ".json",
		CanonicalKey: fmt.Sprintf("rubygems:%d:%s:versions-json", tenant.ID, name),
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
	var entries []upstreamGemVersion
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil
	}
	out := make(map[string]int64, len(entries))
	for _, e := range entries {
		t, err := time.Parse(time.RFC3339, e.CreatedAt)
		if err != nil {
			// rubygems.org sometimes uses RFC3339Nano (fractional seconds).
			t, err = time.Parse(time.RFC3339Nano, e.CreatedAt)
			if err != nil {
				continue
			}
		}
		ts := t.Unix()
		if ts <= 0 {
			continue
		}
		// Key by "<version>" for ruby platform, "<version>-<platform>"
		// otherwise. Matches the way the compact-index line keys its
		// version-platform identity.
		key := e.Number
		if e.Platform != "" && e.Platform != "ruby" {
			key = e.Number + "-" + e.Platform
		}
		out[key] = ts
	}
	return out
}

// passthroughBlocked builds a synthetic Subject for a not-yet-ingested
// upstream version and asks the policy engine whether ActionRead is
// blocked. Mirror of the PyPI helper of the same name.
func (h *Handler) passthroughBlocked(c *gin.Context, tenant *tenants.Tenant, name, version, filename string, publishedAt map[string]int64, versionKey string) bool {
	subj := policy.Subject{
		TenantID: tenant.ID,
		Format:   string(models.TypeRubyGems),
		Package:  strings.ToLower(name),
		Version:  version,
		Filename: filename,
		Attrs: map[string]any{
			"ingest_age_seconds": int64(0),
			"created_unix":       time.Now().Unix(),
		},
	}
	if pub, ok := publishedAt[versionKey]; ok && pub > 0 {
		subj.Attrs["upstream_published_unix"] = pub
	}
	r := h.Engine.Evaluate(c.Request.Context(), subj, policy.ActionRead)
	return r.IsBlocked()
}

// filterUpstreamLines applies per-version policy to the parsed
// compact-index lines and returns the surviving subset. Skips the
// rubygems.org JSON fan-out when the engine is the no-op (tests, dev
// with no rules) - it would be a wasted round-trip.
func (h *Handler) filterUpstreamLines(c *gin.Context, tenant *tenants.Tenant, name string, lines []compactIndexLine) []compactIndexLine {
	if h.Engine == nil {
		return lines
	}
	var publishedAt map[string]int64
	if _, isNoop := h.Engine.(policy.NoopEngine); !isNoop {
		publishedAt = h.fetchUpstreamVersions(c, tenant, name)
	}
	out := make([]compactIndexLine, 0, len(lines))
	for _, l := range lines {
		key := l.Version
		if l.Platform != "" && l.Platform != "ruby" {
			key = l.Version + "-" + l.Platform
		}
		filename := name + "-" + key + ".gem"
		if h.passthroughBlocked(c, tenant, name, l.Version, filename, publishedAt, key) {
			continue
		}
		out = append(out, l)
	}
	return out
}

// servePullThroughInfo replies to /info/<name> with upstream's compact-
// index lines filtered through the policy engine. Same shape the local
// handler would emit so bundler doesn't have to special-case our
// registry.
//
// Returns true when this function wrote the HTTP response, false when
// the caller should fall through to a 404.
func (h *Handler) servePullThroughInfo(c *gin.Context, tenant *tenants.Tenant, name string) bool {
	info, err := h.fetchUpstreamInfo(c, tenant, name)
	if err != nil {
		if errors.Is(err, upstream.ErrUpstreamOff) || errors.Is(err, upstream.ErrUpstreamNotFound) {
			return false
		}
		code, msg := mapUpstreamErr(err)
		c.String(code, "%s", msg)
		return true
	}
	visible := h.filterUpstreamLines(c, tenant, name, info.Lines)
	if len(visible) == 0 {
		// Upstream had the package, but everything is policy-blocked.
		// Return 404 with the same "Could not find package" body the
		// local handler uses so bundler's error message stays consistent.
		c.String(http.StatusNotFound, "Could not find package %s", name)
		return true
	}
	var b strings.Builder
	b.WriteString("---\n")
	for _, l := range visible {
		b.WriteString(l.Raw)
		b.WriteByte('\n')
	}
	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.String(http.StatusOK, b.String())
	return true
}

// fetchUpstreamGem downloads the .gem blob for filename from upstream.
// The caller must verify the SHA256 against the value from the compact
// index after the fetch - this function returns the raw bytes and lets
// the caller decide.
func (h *Handler) fetchUpstreamGem(c *gin.Context, tenant *tenants.Tenant, filename string) (io.ReadCloser, error) {
	if h.Upstream == nil {
		return nil, upstream.ErrUpstreamOff
	}
	req := upstream.Request{
		TenantID:     tenant.ID,
		Format:       "rubygems",
		Kind:         upstream.KindBlob,
		UpstreamPath: "/gems/" + filename,
		CanonicalKey: fmt.Sprintf("rubygems:%d:%s:gem", tenant.ID, filename),
	}
	res, err := h.Upstream.Fetch(c.Request.Context(), req)
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

// servePullThroughGem implements the cold /gems/<filename> miss path:
// fetch compact-index info to discover the SHA256 + canonical version
// string, run the synthetic Subject through the policy engine BEFORE
// the blob fetch, then download + SHA-verify + persist + post-ingest
// Read gate + serve.
//
// Returns (true, nil) when this function wrote the HTTP response
// (success bytes, 403 on policy, 502 on hash mismatch). (false, nil)
// when the caller should fall through to a 404. (false, err) on an
// internal error - caller writes 500 with the message.
func (h *Handler) servePullThroughGem(c *gin.Context, tenant *tenants.Tenant, filename string) (bool, error) {
	if h.Upstream == nil {
		return false, nil
	}

	// Reverse-parse filename -> (name, version, platform). v1 ships
	// ruby-platform only: filenames like "<name>-<version>.gem" with
	// no platform suffix. Platform-tagged filenames (-x86_64-linux.gem
	// etc.) return false here so the caller 404s - see plans/
	// rubygems-pull-through.md §3.3.
	name, version, ok := parseGemFilename(filename)
	if !ok {
		return false, nil
	}

	info, err := h.fetchUpstreamInfo(c, tenant, name)
	if err != nil {
		if errors.Is(err, upstream.ErrUpstreamOff) || errors.Is(err, upstream.ErrUpstreamNotFound) {
			return false, nil
		}
		return false, err
	}

	// Find the matching line. We're after the ruby-platform entry
	// (Platform == "") for this version; if upstream only has a
	// platform-tagged build, we 404.
	var match *compactIndexLine
	for i := range info.Lines {
		l := info.Lines[i]
		if l.Version == version && (l.Platform == "" || l.Platform == "ruby") {
			match = &l
			break
		}
	}
	if match == nil {
		return false, nil
	}

	// Hydrate upstream publish time so the cooldown evaluator's
	// upstream_publish mode can gate before the blob fetch.
	var pubUnix int64
	if _, isNoop := h.Engine.(policy.NoopEngine); !isNoop {
		all := h.fetchUpstreamVersions(c, tenant, name)
		pubUnix = all[version]
	}

	// Pre-ingest gate: only Deny short-circuits here. Quarantine
	// rules let the bytes be persisted then 403 the inflight read
	// post-ingest (see below).
	if h.Engine != nil {
		subj := policy.Subject{
			TenantID: tenant.ID,
			Format:   string(models.TypeRubyGems),
			Package:  strings.ToLower(name),
			Version:  version,
			Filename: filename,
			Attrs: map[string]any{
				"ingest_age_seconds": int64(0),
				"created_unix":       time.Now().Unix(),
			},
		}
		if pubUnix > 0 {
			subj.Attrs["upstream_published_unix"] = pubUnix
		}
		r := h.Engine.Evaluate(c.Request.Context(), subj, policy.ActionIngest)
		if r.Decision >= policy.Deny {
			c.String(http.StatusForbidden, "%s", policyReason(r))
			return true, nil
		}
	}

	// Fetch + hash the blob.
	body, err := h.fetchUpstreamGem(c, tenant, filename)
	if err != nil {
		if errors.Is(err, upstream.ErrUpstreamNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("fetch upstream gem: %w", err)
	}
	defer body.Close()

	buf, err := h.Service.NewHashedBuffer(body)
	if err != nil {
		return false, fmt.Errorf("buffer upstream gem: %w", err)
	}
	defer buf.Close()
	_, _, sha256Hex, _ := buf.Sums()
	if !strings.EqualFold(match.Checksum, sha256Hex) {
		// Hash mismatch: abort, do NOT persist. The compact index's
		// checksum field is the only thing standing between us and a
		// mid-flight tamper.
		c.String(http.StatusBadGateway,
			"upstream gem sha256 mismatch: index=%s actual=%s",
			match.Checksum, sha256Hex)
		return true, nil
	}

	// Parse the .gem to recover the structured metadata (used by the
	// existing local /info handler, the Marshal spec endpoint, etc.).
	if _, err := buf.Seek(0, io.SeekStart); err != nil {
		return false, fmt.Errorf("rewind hashed buffer: %w", err)
	}
	pkg, perr := ParsePackageMetaData(buf)
	if perr != nil {
		// Persisting without metadata leaves a half-broken row that
		// the local handler would fail to render. Abort.
		return false, fmt.Errorf("parse upstream gem: %w", perr)
	}
	if _, err := buf.Seek(0, io.SeekStart); err != nil {
		return false, fmt.Errorf("rewind hashed buffer: %w", err)
	}

	metaJSON, _ := json.Marshal(pkg.Metadata)
	license := ""
	if len(pkg.Metadata.Licenses) > 0 {
		license = pkg.Metadata.Licenses[0]
	}

	_, ver, _, err := h.Service.CreatePackageOrAddFileToExisting(c.Request.Context(), pkgsvc.CreationInfo{
		TenantID:            tenant.ID,
		PackageType:         models.TypeRubyGems,
		PackageName:         pkg.Name,
		PackageLookupName:   strings.ToLower(pkg.Name),
		Version:             pkg.Version,
		VersionMetadataJSON: string(metaJSON),
		Filename:            filename,
		IsLead:              true,
		CreatedVia:          models.CreatedViaPullThrough,
	}, buf)
	if err != nil && !errors.Is(err, models.ErrDuplicatePackageFile) {
		return false, fmt.Errorf("persist upstream gem: %w", err)
	}

	// Re-fetch package + version rows so we can stamp + serve.
	pkgRow, err := h.Models.GetPackage(c.Request.Context(), tenant.ID, models.TypeRubyGems, pkg.Name)
	if err != nil {
		return false, fmt.Errorf("re-lookup persisted package: %w", err)
	}
	if ver == nil {
		ver, err = h.Models.GetVersion(c.Request.Context(), pkgRow.ID, pkg.Version)
		if err != nil {
			return false, fmt.Errorf("re-lookup persisted version: %w", err)
		}
	}

	if pubUnix > 0 && !ver.UpstreamPublishedUnix.Valid {
		_ = h.Models.SetUpstreamPublishedUnix(c.Request.Context(), ver.ID, pubUnix)
	}
	if license != "" && !ver.License.Valid {
		_ = h.Models.SetLicense(c.Request.Context(), ver.ID, license)
	}

	// Post-ingest Read gate: a quarantine rule means the blob stays
	// persisted (so admin can promote) but the inflight client gets
	// 403. Pass pubUnix explicitly in case the row we just loaded is
	// stale wrt the stamp above.
	if h.Engine != nil {
		readSubj := h.subjectFor(tenant, pkgRow, ver, filename)
		if pubUnix > 0 {
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

	// Serve from the now-persisted local file.
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
		return false, fmt.Errorf("persisted gem vanished from DB")
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

// parseGemFilename reverse-parses "<name>-<version>.gem" into its
// components. v1 limitation: only ruby-platform filenames are
// accepted. Platform-tagged filenames ("<name>-<version>-<platform>.gem")
// return ok=false because we can't unambiguously split <name>-<version>
// without knowing the package name from another channel.
//
// Returns (name, version, true) on success, ("", "", false) when:
//   - filename doesn't end in ".gem"
//   - no hyphen (can't separate name from version)
//   - the right-side token after the last hyphen-before-".gem" doesn't
//     look like a version (starts with non-digit) - likely a platform
func parseGemFilename(filename string) (name, version string, ok bool) {
	if !strings.HasSuffix(filename, ".gem") {
		return "", "", false
	}
	base := strings.TrimSuffix(filename, ".gem")
	// Find the LAST hyphen whose right side looks like a version
	// (starts with a digit). Walk right-to-left.
	for i := len(base) - 1; i > 0; i-- {
		if base[i] != '-' {
			continue
		}
		right := base[i+1:]
		if right == "" {
			break
		}
		if !isAsciiDigit(right[0]) {
			// Right side doesn't look like a version - probably a
			// platform suffix. Keep walking left for an earlier hyphen.
			continue
		}
		// First hyphen we encounter (rightmost) with a digit-led right
		// side: treat that as the name/version boundary. If a platform
		// is present, this'll mis-split (we'd find the version-platform
		// boundary instead of the name-version boundary) - which is why
		// we additionally reject any candidate whose right side
		// contains a hyphen (a platform tag like "x86_64-linux" would).
		if strings.ContainsRune(right, '-') {
			return "", "", false
		}
		return base[:i], right, true
	}
	return "", "", false
}

// mapUpstreamErr translates a fetcher sentinel into an HTTP status +
// short message body. Copy of the PyPI helper.
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
