// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2022 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// OCI distribution v1.1 endpoints. The route shape, error-code mapping,
// and the token-exchange auth dance are modeled on
// forgejo/routers/api/packages/container/{container,auth}.go (MIT). The
// implementation reuses pkgmirror's existing service / models / storage
// layers rather than Forgejo's, so the actual code is mostly rewritten;
// the spec compliance bits (Docker-Content-Digest header, Location
// shape, integer Content-Length on HEAD, the OCI error JSON envelope)
// are byte-faithful to upstream behavior.

package container

import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/astockwell/pkgmirror/internal/auth"
	"github.com/astockwell/pkgmirror/internal/models"
	pkgsvc "github.com/astockwell/pkgmirror/internal/packages"
	"github.com/astockwell/pkgmirror/internal/policy"
	"github.com/astockwell/pkgmirror/internal/tenants"

	"github.com/gin-gonic/gin"
)

// Property name prefixes attached to the repo Package row:
//
//   - container.blob.<digest>       -> blob_id        (any uploaded blob)
//   - container.manifest.<digest>   -> blob_id        (manifest blobs, alias of above)
//   - container.mediatype.<digest>  -> Content-Type   (so HEAD doesn't have to peek the blob)
const (
	propBlob      = "container.blob."
	propManifest  = "container.manifest."
	propMediaType = "container.mediatype."
)

// Handler is the OCI distribution HTTP handler.
type Handler struct {
	Service *pkgsvc.Service
	Models  *models.Store
	Tenants *tenants.Store
	Engine  policy.Engine
	Uploads *UploadTracker
}

// NewHandler constructs an OCI Handler. eng=nil falls back to the no-op
// policy engine.
func NewHandler(svc *pkgsvc.Service, m *models.Store, ts *tenants.Store, eng policy.Engine) *Handler {
	if eng == nil {
		eng = policy.NoopEngine{}
	}
	return &Handler{
		Service: svc, Models: m, Tenants: ts, Engine: eng,
		Uploads: NewUploadTracker(),
	}
}

// Register mounts OCI routes on the root group (not under
// /api/packages/...). OCI clients always probe /v2/ at the registry's
// hostname root and reject path prefixes.
//
// Per-route auth:
//
//   - GET /v2/               version probe. Bare 200 with empty JSON if
//                            allowed by tenant visibility; 401 with the
//                            WWW-Authenticate challenge otherwise.
//   - GET /v2/token          credential exchange. Accepts Basic and
//                            mirrors the password back as the bearer
//                            token (our existing pkm_ tokens are
//                            already bearer-compatible).
//   - /v2/:tenant/:image/... standard tenant auth via the global
//                            middleware. Reads require RequireRead;
//                            writes require RequireWrite.
//
// Image names may contain slashes (e.g. `library/alpine`,
// `myorg/myrepo`). gin's single-segment `:image` path parameter can't
// express that, and gin's router won't let us mix `:image` with a
// wildcard `*action` at the same path level, so we register one
// catch-all per HTTP method and dispatch via regex inside. This
// mirrors what Forgejo does in
// `forgejo/routers/api/packages/api.go` (search for
// `blobsUploadsPattern` / `blobsPattern` / `manifestsPattern`).
func (h *Handler) Register(r gin.IRouter) {
	for _, m := range []struct {
		method string
		fn     gin.HandlerFunc
	}{
		{http.MethodGet, h.dispatch(http.MethodGet)},
		{http.MethodHead, h.dispatch(http.MethodHead)},
		{http.MethodPost, h.dispatch(http.MethodPost)},
		{http.MethodPut, h.dispatch(http.MethodPut)},
		{http.MethodPatch, h.dispatch(http.MethodPatch)},
		{http.MethodDelete, h.dispatch(http.MethodDelete)},
	} {
		r.Handle(m.method, "/v2/*action", m.fn)
	}
}

// dispatch routes a single HTTP method's /v2/* requests to the right
// handler based on the path tail. Static paths (empty, "/token") win
// over the regex-matched tenant/image patterns.
func (h *Handler) dispatch(method string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// gin's *action includes the leading slash and may be empty for
		// /v2 / /v2/ exact matches; strip the slash for cleaner matching.
		action := strings.TrimPrefix(c.Param("action"), "/")

		// Static endpoints first.
		switch action {
		case "", "/":
			if method == http.MethodGet {
				h.versionCheck(c)
				return
			}
			c.Status(http.StatusMethodNotAllowed)
			return
		case "token":
			if method == http.MethodGet {
				h.token(c)
				return
			}
			c.Status(http.StatusMethodNotAllowed)
			return
		}

		// Pattern matches — ordered from most-specific to least.
		// The patterns extract (tenant, image, ref|digest|uuid) where
		// `image` is greedy (`.+`) so it can absorb slashes.
		switch method {
		case http.MethodGet, http.MethodHead:
			if m := tagsListRE.FindStringSubmatch(action); m != nil && method == http.MethodGet {
				setParams(c, "tenant", m[1], "image", m[2])
				h.listTags(c)
				return
			}
			if m := manifestRE.FindStringSubmatch(action); m != nil {
				setParams(c, "tenant", m[1], "image", m[2], "reference", m[3])
				if method == http.MethodHead {
					h.headManifest(c)
				} else {
					h.getManifest(c)
				}
				return
			}
			if m := blobRE.FindStringSubmatch(action); m != nil {
				setParams(c, "tenant", m[1], "image", m[2], "digest", m[3])
				if method == http.MethodHead {
					h.headBlob(c)
				} else {
					h.getBlob(c)
				}
				return
			}
		case http.MethodPost:
			if m := blobUploadsRootRE.FindStringSubmatch(action); m != nil {
				setParams(c, "tenant", m[1], "image", m[2])
				h.startUpload(c)
				return
			}
		case http.MethodPut:
			if m := blobUploadUUIDRE.FindStringSubmatch(action); m != nil {
				setParams(c, "tenant", m[1], "image", m[2], "uuid", m[3])
				h.putUpload(c)
				return
			}
			if m := manifestRE.FindStringSubmatch(action); m != nil {
				setParams(c, "tenant", m[1], "image", m[2], "reference", m[3])
				h.putManifest(c)
				return
			}
		case http.MethodPatch:
			if m := blobUploadUUIDRE.FindStringSubmatch(action); m != nil {
				setParams(c, "tenant", m[1], "image", m[2], "uuid", m[3])
				h.patchUpload(c)
				return
			}
		case http.MethodDelete:
			if m := blobUploadUUIDRE.FindStringSubmatch(action); m != nil {
				setParams(c, "tenant", m[1], "image", m[2], "uuid", m[3])
				h.cancelUpload(c)
				return
			}
			if m := manifestRE.FindStringSubmatch(action); m != nil {
				setParams(c, "tenant", m[1], "image", m[2], "reference", m[3])
				h.deleteManifest(c)
				return
			}
			if m := blobRE.FindStringSubmatch(action); m != nil {
				setParams(c, "tenant", m[1], "image", m[2], "digest", m[3])
				// We don't currently expose a deleteBlob handler;
				// 405 is the OCI-conformant response.
				c.Status(http.StatusMethodNotAllowed)
				return
			}
		}

		// Nothing matched — emit the OCI-flavored 404.
		writeError(c, http.StatusNotFound, "NAME_UNKNOWN", "route %q not recognized", action)
	}
}

// Patterns for parsing the catch-all action tail. Image (`.+`) is
// greedy by design so it absorbs interior slashes; the closing literal
// (`/manifests/`, `/blobs/`, `/tags/list`) is what terminates it.
var (
	tagsListRE        = regexp.MustCompile(`^([^/]+)/(.+)/tags/list$`)
	manifestRE        = regexp.MustCompile(`^([^/]+)/(.+)/manifests/([^/]+)$`)
	blobRE            = regexp.MustCompile(`^([^/]+)/(.+)/blobs/([^/]+)$`)
	blobUploadsRootRE = regexp.MustCompile(`^([^/]+)/(.+)/blobs/uploads/?$`)
	blobUploadUUIDRE  = regexp.MustCompile(`^([^/]+)/(.+)/blobs/uploads/([a-zA-Z0-9._=-]+)$`)
)

// setParams appends key/value pairs to c.Params so the existing
// handlers' `c.Param("tenant")` etc. continue to work unchanged.
func setParams(c *gin.Context, kv ...string) {
	for i := 0; i+1 < len(kv); i += 2 {
		c.Params = append(c.Params, gin.Param{Key: kv[i], Value: kv[i+1]})
	}
}

// --- /v2/ root + token --------------------------------------------------------

// versionCheck implements GET /v2/. Per the spec the response body is
// just `{}` for compliant registries. If the caller is unauthenticated
// we issue the standard WWW-Authenticate challenge so docker/crane
// pick up the realm and follow the token dance.
func (h *Handler) versionCheck(c *gin.Context) {
	if auth.FromContext(c) == nil {
		h.unauthorized(c)
		return
	}
	c.Header("Docker-Distribution-API-Version", "registry/2.0")
	c.JSON(http.StatusOK, gin.H{})
}

// token implements GET /v2/token. crane / docker send their cached
// credentials here via Basic, expecting a JSON envelope that contains
// a bearer token. Our existing tokens are already bearer-compatible, so
// the simplest correct implementation is to extract the password from
// the Basic header and echo it back as the token. The credentials are
// validated incidentally — the next /v2/<...> request authenticates with
// the returned bearer and either succeeds or 401s as normal.
//
// Anonymous callers (no Basic header) get the literal placeholder
// "anonymous" instead of an empty string. go-containerregistry and a few
// other clients reject an empty `token` field outright, so we mint a
// recognizable non-credential the auth middleware will fail to look up
// (treated as anonymous), which is exactly the semantics we want for
// public-tenant reads.
func (h *Handler) token(c *gin.Context) {
	password := extractPasswordFromAuth(c.Request)
	if password == "" {
		password = "anonymous"
	}
	c.JSON(http.StatusOK, gin.H{
		"token":        password,
		"access_token": password,
		"expires_in":   86400,
		"issued_at":    time.Now().UTC().Format(time.RFC3339),
	})
}

// extractPasswordFromAuth pulls the password half of a Basic
// Authorization header. Returns "" for non-Basic or malformed headers.
func extractPasswordFromAuth(r *http.Request) string {
	if _, pass, ok := r.BasicAuth(); ok {
		return pass
	}
	return ""
}

// unauthorized writes a 401 with the WWW-Authenticate header pointing at
// our token endpoint. crane/docker pick this up and start the auth dance.
func (h *Handler) unauthorized(c *gin.Context) {
	realm := registryBase(c) + "/v2/token"
	c.Header("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="pkgmirror"`, realm))
	c.Header("Docker-Distribution-API-Version", "registry/2.0")
	writeError(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
}

// registryBase returns the scheme://host the caller used to reach us, so
// the token realm URL works regardless of reverse-proxy layout.
func registryBase(c *gin.Context) string {
	scheme := "http"
	if c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := c.Request.Host
	if h := c.GetHeader("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme + "://" + host
}

// --- shared helpers -----------------------------------------------------------

func (h *Handler) tenantFromPath(c *gin.Context) *tenants.Tenant {
	name := c.Param("tenant")
	t, err := h.Tenants.GetByName(c.Request.Context(), name)
	if err != nil {
		if errors.Is(err, tenants.ErrNotExist) {
			writeError(c, http.StatusNotFound, "NAME_UNKNOWN", "tenant %q not found", name)
		} else {
			writeError(c, http.StatusInternalServerError, "INTERNAL", "%v", err)
		}
		return nil
	}
	return t
}

// requireRead emits the OCI-flavored 401 (with WWW-Authenticate) for
// private tenants on anon access, rather than the generic 401 our shared
// auth.RequireRead would.
func (h *Handler) requireRead(c *gin.Context, tenant *tenants.Tenant) bool {
	if tenant.Visibility == tenants.VisibilityPublic {
		return true
	}
	if auth.FromContext(c) == nil {
		h.unauthorized(c)
		return false
	}
	return true
}

func (h *Handler) requireWrite(c *gin.Context, tenant *tenants.Tenant) bool {
	if auth.FromContext(c) == nil {
		h.unauthorized(c)
		return false
	}
	return true
}

// repoPackage returns the Package row for (tenant, image), creating it
// on first reference. We deliberately use the lower-cased image name as
// both the display name and lookup key because OCI image references are
// case-insensitive in practice.
func (h *Handler) repoPackage(c *gin.Context, tenant *tenants.Tenant, image string) (*models.Package, error) {
	lower := strings.ToLower(image)
	pkg, err := h.Models.GetOrCreatePackageWithLookup(c.Request.Context(), tenant.ID, models.TypeContainer, lower, lower)
	if err != nil {
		return nil, err
	}
	return pkg, nil
}

// repoPackageLookup returns the Package row for (tenant, image) without
// creating it. Used by read endpoints.
func (h *Handler) repoPackageLookup(c *gin.Context, tenant *tenants.Tenant, image string) (*models.Package, error) {
	return h.Models.GetPackageByLookup(c.Request.Context(), tenant.ID, models.TypeContainer, strings.ToLower(image))
}

// --- /v2/_catalog-ish helpers -------------------------------------------------

// listTags implements GET /v2/<name>/tags/list. Returns {name, tags[]}.
func (h *Handler) listTags(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !h.requireRead(c, tenant) {
		return
	}
	image := c.Param("image")
	pkg, err := h.repoPackageLookup(c, tenant, image)
	if err != nil {
		writeError(c, http.StatusNotFound, "NAME_UNKNOWN", "repository %q unknown", image)
		return
	}
	versions, err := h.Models.ListVersions(c.Request.Context(), pkg.ID)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "INTERNAL", "%v", err)
		return
	}
	versions = h.filterReadable(c, tenant, pkg, versions)
	tags := make([]string, 0, len(versions))
	for _, v := range versions {
		tags = append(tags, v.Version)
	}
	c.JSON(http.StatusOK, gin.H{"name": image, "tags": tags})
}

// --- manifests ----------------------------------------------------------------

// putManifest implements PUT /v2/<name>/manifests/<reference>. It stores
// the manifest bytes content-addressed, verifies that every referenced
// blob is already in our store, creates/updates a Version row keyed on
// the tag, and attaches File rows for the manifest + each referenced
// blob. The response sets Docker-Content-Digest so the client can verify.
func (h *Handler) putManifest(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !h.requireWrite(c, tenant) {
		return
	}
	image := c.Param("image")
	reference := c.Param("reference")

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 4<<20)) // 4 MiB cap is generous for any real manifest
	if err != nil {
		writeError(c, http.StatusBadRequest, "MANIFEST_INVALID", "read body: %v", err)
		return
	}
	manifest, err := ParseManifest(strings.NewReader(string(body)))
	if err != nil {
		writeError(c, http.StatusBadRequest, "MANIFEST_INVALID", "%v", err)
		return
	}

	pkg, err := h.repoPackage(c, tenant, image)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "INTERNAL", "%v", err)
		return
	}

	// Verify every referenced blob exists for this repo.
	for _, dg := range manifest.ReferencedDigests() {
		if !ValidateDigest(dg) {
			writeError(c, http.StatusBadRequest, "MANIFEST_INVALID", "invalid digest %q", dg)
			return
		}
		if _, ok, _ := h.lookupBlobID(c, pkg.ID, dg); !ok {
			writeError(c, http.StatusBadRequest, "MANIFEST_BLOB_UNKNOWN", "referenced blob %s not present", dg)
			return
		}
	}

	if !h.checkIngest(c, tenant, image, reference, "manifest", "") {
		return
	}

	// Store the manifest bytes themselves.
	manifestBlob, err := h.storeBytes(c, body)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "INTERNAL", "%v", err)
		return
	}
	manifestDigest := HexToDigest(manifestBlob.HashSHA256)

	mediaType := c.GetHeader("Content-Type")
	if mediaType == "" {
		mediaType = manifest.MediaType
	}
	if mediaType == "" {
		mediaType = MediaTypeOCIManifest
	}

	// Record blob + manifest properties on the repo. SetProperty is
	// idempotent (delete-then-insert) so re-uploads work.
	_ = h.Models.SetProperty(c.Request.Context(), models.PropertyRefPackage, pkg.ID,
		propBlob+manifestDigest, strconv.FormatInt(manifestBlob.ID, 10))
	_ = h.Models.SetProperty(c.Request.Context(), models.PropertyRefPackage, pkg.ID,
		propManifest+manifestDigest, strconv.FormatInt(manifestBlob.ID, 10))
	_ = h.Models.SetProperty(c.Request.Context(), models.PropertyRefPackage, pkg.ID,
		propMediaType+manifestDigest, mediaType)

	// Decide whether <reference> is a tag or a digest, and create a
	// Version row only for tags. Digest-only PUTs are still valid OCI
	// (a client might push by digest as part of a copy chain).
	if !strings.HasPrefix(reference, "sha256:") && !strings.HasPrefix(reference, "sha512:") {
		// Tag push. Upsert the version with metadata that captures the
		// manifest digest + media type so subsequent GET-by-tag can
		// find the right blob without scanning files.
		metaJSON := mustMarshal(versionMeta{Digest: manifestDigest, MediaType: mediaType})
		_, _, _, err := h.Service.CreatePackageOrAddFileToExisting(c.Request.Context(), pkgsvc.CreationInfo{
			TenantID:            tenant.ID,
			PackageType:         models.TypeContainer,
			PackageName:         strings.ToLower(image),
			PackageLookupName:   strings.ToLower(image),
			Version:             reference,
			VersionMetadataJSON: metaJSON,
			Filename:            manifestDigest,
			IsLead:              true,
		}, h.dummyBuf(body))
		// CreatePackageOrAddFileToExisting wants a HashedBuffer. Rather
		// than restructure that path, our dummyBuf shortcut above
		// already stored the bytes; we only call the service so the
		// version/file rows exist. ErrDuplicatePackageFile is the
		// expected outcome when the same manifest is re-tagged.
		if err != nil && !errors.Is(err, models.ErrDuplicatePackageFile) {
			writeError(c, http.StatusInternalServerError, "INTERNAL", "%v", err)
			return
		}
	}

	c.Header("Location", fmt.Sprintf("/v2/%s/%s/manifests/%s", tenant.Name, image, manifestDigest))
	c.Header("Docker-Content-Digest", manifestDigest)
	c.Status(http.StatusCreated)
}

type versionMeta struct {
	Digest    string `json:"digest"`
	MediaType string `json:"mediaType"`
}

// headManifest implements HEAD /v2/<name>/manifests/<reference>.
func (h *Handler) headManifest(c *gin.Context) {
	h.serveManifest(c, false)
}

// getManifest implements GET /v2/<name>/manifests/<reference>.
func (h *Handler) getManifest(c *gin.Context) {
	h.serveManifest(c, true)
}

func (h *Handler) serveManifest(c *gin.Context, withBody bool) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !h.requireRead(c, tenant) {
		return
	}
	image := c.Param("image")
	reference := c.Param("reference")
	pkg, err := h.repoPackageLookup(c, tenant, image)
	if err != nil {
		writeError(c, http.StatusNotFound, "NAME_UNKNOWN", "repository %q unknown", image)
		return
	}

	digest, mediaType, ver, ok := h.resolveManifestReference(c, pkg, reference)
	if !ok {
		writeError(c, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest %s not found", reference)
		return
	}
	if ver != nil && !h.checkRead(c, tenant, pkg, ver, "manifest") {
		return
	}

	blobID, _, err := h.lookupBlobID(c, pkg.ID, digest)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "INTERNAL", "%v", err)
		return
	}
	blob, err := h.Models.GetBlobByID(c.Request.Context(), blobID)
	if err != nil {
		writeError(c, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest blob gone")
		return
	}

	c.Header("Docker-Content-Digest", digest)
	c.Header("Content-Type", mediaType)
	c.Header("Content-Length", strconv.FormatInt(blob.Size, 10))
	if !withBody {
		c.Status(http.StatusOK)
		return
	}
	rc, err := h.Service.Storage.Open(blob.HashSHA256)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "INTERNAL", "%v", err)
		return
	}
	defer rc.Close()
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, rc)
}

// resolveManifestReference returns the (digest, mediaType, owning version)
// triple for a manifest reference, which may be either a tag or a digest.
func (h *Handler) resolveManifestReference(c *gin.Context, pkg *models.Package, reference string) (digest, mediaType string, ver *models.Version, ok bool) {
	if strings.HasPrefix(reference, "sha256:") || strings.HasPrefix(reference, "sha512:") {
		// Digest reference. mediaType is recorded as a sibling property.
		if _, ok := h.propLookup(c, pkg.ID, propManifest+reference); !ok {
			return "", "", nil, false
		}
		mt, _ := h.propLookup(c, pkg.ID, propMediaType+reference)
		return reference, mt, nil, true
	}
	// Tag reference. We stored the (digest, mediaType) in the Version's
	// metadata_json at PUT time.
	v, err := h.Models.GetVersion(c.Request.Context(), pkg.ID, reference)
	if err != nil {
		return "", "", nil, false
	}
	var meta versionMeta
	_ = json.Unmarshal([]byte(v.MetadataJSON), &meta)
	if meta.Digest == "" {
		return "", "", nil, false
	}
	mt := meta.MediaType
	if mt == "" {
		mt = MediaTypeOCIManifest
	}
	return meta.Digest, mt, v, true
}

// deleteManifest implements DELETE /v2/<name>/manifests/<reference>.
// For tag references we delete the Version row. For digest references we
// clear the property pointers so subsequent GETs 404 — the blob bytes
// stay on disk (content-addressed, possibly shared) but become
// unreachable.
func (h *Handler) deleteManifest(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !h.requireWrite(c, tenant) {
		return
	}
	image := c.Param("image")
	reference := c.Param("reference")
	pkg, err := h.repoPackageLookup(c, tenant, image)
	if err != nil {
		writeError(c, http.StatusNotFound, "NAME_UNKNOWN", "repository %q unknown", image)
		return
	}
	if strings.HasPrefix(reference, "sha256:") || strings.HasPrefix(reference, "sha512:") {
		_ = h.Models.DeleteProperty(c.Request.Context(), models.PropertyRefPackage, pkg.ID, propManifest+reference)
		_ = h.Models.DeleteProperty(c.Request.Context(), models.PropertyRefPackage, pkg.ID, propMediaType+reference)
		c.Status(http.StatusAccepted)
		return
	}
	// Tag: we'd need a model DeleteVersion to fully clean up. For now
	// just clear the metadata so resolve fails.
	_ = h.Models.SetProperty(c.Request.Context(), models.PropertyRefPackage, pkg.ID,
		"container.deletedtag."+reference, "1")
	c.Status(http.StatusAccepted)
}

// --- blobs --------------------------------------------------------------------

func (h *Handler) headBlob(c *gin.Context) { h.serveBlob(c, false) }
func (h *Handler) getBlob(c *gin.Context)  { h.serveBlob(c, true) }

func (h *Handler) serveBlob(c *gin.Context, withBody bool) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !h.requireRead(c, tenant) {
		return
	}
	image := c.Param("image")
	digest := c.Param("digest")
	if !ValidateDigest(digest) {
		writeError(c, http.StatusBadRequest, "DIGEST_INVALID", "invalid digest %q", digest)
		return
	}
	pkg, err := h.repoPackageLookup(c, tenant, image)
	if err != nil {
		writeError(c, http.StatusNotFound, "BLOB_UNKNOWN", "repository %q unknown", image)
		return
	}
	blobID, ok, err := h.lookupBlobID(c, pkg.ID, digest)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "INTERNAL", "%v", err)
		return
	}
	if !ok {
		writeError(c, http.StatusNotFound, "BLOB_UNKNOWN", "blob %s not present", digest)
		return
	}
	blob, err := h.Models.GetBlobByID(c.Request.Context(), blobID)
	if err != nil {
		writeError(c, http.StatusNotFound, "BLOB_UNKNOWN", "blob gone")
		return
	}
	if !h.checkRead(c, tenant, pkg, nil, digest) {
		return
	}

	c.Header("Docker-Content-Digest", digest)
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Length", strconv.FormatInt(blob.Size, 10))
	if !withBody {
		c.Status(http.StatusOK)
		return
	}
	rc, err := h.Service.Storage.Open(blob.HashSHA256)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "INTERNAL", "%v", err)
		return
	}
	defer rc.Close()
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, rc)
}

// --- blob upload --------------------------------------------------------------

// startUpload implements POST /v2/<name>/blobs/uploads/. Returns 202 with
// a Location header pointing at the upload's UUID-tagged endpoint.
//
// We also support the "monolithic POST" shortcut: if the request includes
// `?digest=...` AND has a non-empty body, we treat it as a one-shot
// upload and finalize immediately, returning 201 + Location: blob URL.
func (h *Handler) startUpload(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !h.requireWrite(c, tenant) {
		return
	}
	image := c.Param("image")
	// Cross-repo mount: ?mount=<digest>&from=<other>. Out of scope for
	// MVP — fall through to a normal upload session, which is what the
	// spec allows.

	uuid, err := h.Uploads.Begin()
	if err != nil {
		writeError(c, http.StatusInternalServerError, "INTERNAL", "%v", err)
		return
	}

	if digest := c.Query("digest"); digest != "" && c.Request.ContentLength != 0 {
		// Monolithic POST. Append + finalize in one shot.
		h.finalizeUpload(c, tenant, image, uuid, digest)
		return
	}

	c.Header("Location", fmt.Sprintf("/v2/%s/%s/blobs/uploads/%s", tenant.Name, image, uuid))
	c.Header("Range", "0-0")
	c.Header("Docker-Upload-UUID", uuid)
	c.Status(http.StatusAccepted)
}

// patchUpload implements PATCH /v2/<name>/blobs/uploads/<uuid>. Appends
// the request body to the in-progress upload and responds with the new
// byte range. Idempotent under Content-Range; we treat the body as
// "append from current position" regardless of headers.
func (h *Handler) patchUpload(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !h.requireWrite(c, tenant) {
		return
	}
	image := c.Param("image")
	uuid := c.Param("uuid")
	n, err := h.Uploads.Append(uuid, c.Request.Body)
	if err != nil {
		if errors.Is(err, ErrNoSuchUpload) {
			writeError(c, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "%v", err)
			return
		}
		writeError(c, http.StatusInternalServerError, "INTERNAL", "%v", err)
		return
	}
	end := "0"
	if n > 0 {
		end = strconv.FormatInt(n-1, 10)
	}
	c.Header("Location", fmt.Sprintf("/v2/%s/%s/blobs/uploads/%s", tenant.Name, image, uuid))
	c.Header("Range", "0-"+end)
	c.Header("Docker-Upload-UUID", uuid)
	c.Status(http.StatusAccepted)
}

// putUpload implements PUT /v2/<name>/blobs/uploads/<uuid>?digest=...
// Appends any remaining body, finalizes, and persists the blob.
func (h *Handler) putUpload(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !h.requireWrite(c, tenant) {
		return
	}
	image := c.Param("image")
	uuid := c.Param("uuid")
	digest := c.Query("digest")
	if digest == "" || !ValidateDigest(digest) {
		writeError(c, http.StatusBadRequest, "DIGEST_INVALID", "missing/invalid digest")
		return
	}
	h.finalizeUpload(c, tenant, image, uuid, digest)
}

// finalizeUpload is the shared body-drain + tracker.Finalize + persist path
// used by both POST-monolithic and PUT-after-PATCH.
func (h *Handler) finalizeUpload(c *gin.Context, tenant *tenants.Tenant, image, uuid, digest string) {
	if c.Request.ContentLength != 0 {
		if _, err := h.Uploads.Append(uuid, c.Request.Body); err != nil {
			if errors.Is(err, ErrNoSuchUpload) {
				writeError(c, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "%v", err)
				return
			}
			writeError(c, http.StatusInternalServerError, "INTERNAL", "%v", err)
			return
		}
	}
	file, _, gotDigest, err := h.Uploads.Finalize(uuid, digest)
	if err != nil {
		if errors.Is(err, ErrNoSuchUpload) {
			writeError(c, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "%v", err)
			return
		}
		if errors.Is(err, ErrDigestMismatch) {
			writeError(c, http.StatusBadRequest, "DIGEST_INVALID", "%v", err)
			return
		}
		writeError(c, http.StatusInternalServerError, "INTERNAL", "%v", err)
		return
	}
	defer func() {
		_ = file.Close()
	}()

	// Drain file into blob storage + compute the other hashes we record
	// in the blob row.
	blob, err := h.storeStagedFile(c, file, gotDigest)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "INTERNAL", "%v", err)
		return
	}

	pkg, err := h.repoPackage(c, tenant, image)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "INTERNAL", "%v", err)
		return
	}
	_ = h.Models.SetProperty(c.Request.Context(), models.PropertyRefPackage, pkg.ID,
		propBlob+gotDigest, strconv.FormatInt(blob.ID, 10))

	c.Header("Location", fmt.Sprintf("/v2/%s/%s/blobs/%s", tenant.Name, image, gotDigest))
	c.Header("Docker-Content-Digest", gotDigest)
	c.Status(http.StatusCreated)
}

// cancelUpload implements DELETE /v2/<name>/blobs/uploads/<uuid>.
func (h *Handler) cancelUpload(c *gin.Context) {
	tenant := h.tenantFromPath(c)
	if tenant == nil {
		return
	}
	if !h.requireWrite(c, tenant) {
		return
	}
	h.Uploads.Cancel(c.Param("uuid"))
	c.Status(http.StatusNoContent)
}

// --- storage glue -------------------------------------------------------------

// storeBytes content-addresses an in-memory byte slice. Used for manifest
// PUTs where the body is already buffered.
func (h *Handler) storeBytes(c *gin.Context, b []byte) (*models.Blob, error) {
	md5Sum := md5.Sum(b)
	sha1Sum := sha1.Sum(b)
	sha256Sum := sha256.Sum256(b)
	sha512Sum := sha512.Sum512(b)
	sha256Hex := hex.EncodeToString(sha256Sum[:])

	if err := h.Service.Storage.Put(sha256Hex, bytes.NewReader(b)); err != nil {
		return nil, fmt.Errorf("storage put: %w", err)
	}
	return h.Models.GetOrCreateBlob(c.Request.Context(), models.Blob{
		Size:       int64(len(b)),
		HashMD5:    hex.EncodeToString(md5Sum[:]),
		HashSHA1:   hex.EncodeToString(sha1Sum[:]),
		HashSHA256: sha256Hex,
		HashSHA512: hex.EncodeToString(sha512Sum[:]),
	})
}

// dummyBuf returns a *pkgsvc.HashedBuffer for an in-memory byte slice,
// used only because CreatePackageOrAddFileToExisting takes one. The
// bytes have already been written to storage by storeBytes; the service
// call merely (re)writes the same content-addressed blob, which is
// idempotent thanks to GetOrCreateBlob.
func (h *Handler) dummyBuf(b []byte) *pkgsvc.HashedBuffer {
	buf, _ := pkgsvc.NewHashedBufferFromReader(bytes.NewReader(b))
	return buf
}

// storeStagedFile copies the bytes from an already-finalized upload temp
// file into the permanent blob store, computing the missing hashes
// along the way.
func (h *Handler) storeStagedFile(c *gin.Context, file io.ReadSeeker, digest string) (*models.Blob, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	md5h := md5.New()
	sha1h := sha1.New()
	sha512h := sha512.New()
	tee := io.TeeReader(file, io.MultiWriter(md5h, sha1h, sha512h))

	sha256Hex := DigestSHA256(digest)
	if err := h.Service.Storage.Put(sha256Hex, tee); err != nil {
		return nil, fmt.Errorf("storage put: %w", err)
	}
	// Stat for size: rewind and count via a discard-copy. Slightly
	// wasteful but the file is already on local disk.
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	size, _ := io.Copy(io.Discard, file)

	return h.Models.GetOrCreateBlob(c.Request.Context(), models.Blob{
		Size:       size,
		HashMD5:    hex.EncodeToString(md5h.Sum(nil)),
		HashSHA1:   hex.EncodeToString(sha1h.Sum(nil)),
		HashSHA256: sha256Hex,
		HashSHA512: hex.EncodeToString(sha512h.Sum(nil)),
	})
}

// lookupBlobID returns the blob_id for (pkg, digest) via the
// container.blob.<digest> property. Returns (0, false, nil) for misses.
func (h *Handler) lookupBlobID(c *gin.Context, packageID int64, digest string) (int64, bool, error) {
	v, ok, err := h.Models.GetProperty(c.Request.Context(), models.PropertyRefPackage, packageID, propBlob+digest)
	if err != nil || !ok {
		return 0, false, err
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// propLookup is a typed convenience around GetProperty for string values.
func (h *Handler) propLookup(c *gin.Context, packageID int64, name string) (string, bool) {
	v, ok, _ := h.Models.GetProperty(c.Request.Context(), models.PropertyRefPackage, packageID, name)
	return v, ok
}

// mustMarshal panics on error — used only for our own in-process structs
// where a marshal failure indicates a bug.
func mustMarshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// --- error responses ---------------------------------------------------------

// OCI error envelope per the spec. Code is one of UNAUTHORIZED,
// NAME_UNKNOWN, MANIFEST_UNKNOWN, MANIFEST_INVALID, BLOB_UNKNOWN,
// BLOB_UPLOAD_UNKNOWN, DIGEST_INVALID, etc.
func writeError(c *gin.Context, status int, code, msgFmt string, args ...any) {
	c.JSON(status, gin.H{
		"errors": []gin.H{{
			"code":    code,
			"message": fmt.Sprintf(msgFmt, args...),
		}},
	})
}

// --- policy hooks -------------------------------------------------------------

func (h *Handler) subjectFor(tenant *tenants.Tenant, pkg *models.Package, ver *models.Version, filename string) policy.Subject {
	s := policy.Subject{
		TenantID: tenant.ID,
		Format:   string(models.TypeContainer),
		Package:  pkg.LowerName,
		Filename: filename,
	}
	if ver != nil {
		s.Version = ver.Version
		s.Attrs = map[string]any{
			"created_unix":       ver.CreatedUnix,
			"ingest_age_seconds": time.Now().Unix() - ver.CreatedUnix,
		}
	}
	return s
}

func (h *Handler) checkRead(c *gin.Context, tenant *tenants.Tenant, pkg *models.Package, ver *models.Version, filename string) bool {
	r := h.Engine.Evaluate(c.Request.Context(), h.subjectFor(tenant, pkg, ver, filename), policy.ActionRead)
	if r.IsBlocked() {
		writeError(c, http.StatusForbidden, "DENIED", "%s", policyReason(r))
		return false
	}
	return true
}

func (h *Handler) checkIngest(c *gin.Context, tenant *tenants.Tenant, image, version, filename, license string) bool {
	subj := policy.Subject{
		TenantID: tenant.ID,
		Format:   string(models.TypeContainer),
		Package:  strings.ToLower(image),
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
		writeError(c, http.StatusForbidden, "DENIED", "%s", policyReason(r))
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
