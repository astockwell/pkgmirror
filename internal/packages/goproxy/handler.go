// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2023 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// The HTTP endpoint shape (list / info / mod / zip / @latest), the
// {Version, Time} info response struct, the resolve() pattern with
// "latest" special-casing, and the upload flow are modeled on
// forgejo/routers/api/packages/goproxy/goproxy.go from the Forgejo project,
// which is itself MIT licensed.

package goproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/astockwell/pkgmirror/internal/models"
	pkgsvc "github.com/astockwell/pkgmirror/internal/packages"

	"github.com/gin-gonic/gin"
)

// Handler implements the HTTP endpoints of the Go module proxy protocol plus
// a non-standard upload endpoint for populating the mirror.
type Handler struct {
	Service *pkgsvc.Service
	Models  *models.Store
}

// NewHandler constructs a Handler.
func NewHandler(svc *pkgsvc.Service, m *models.Store) *Handler {
	return &Handler{Service: svc, Models: m}
}

// Register attaches the Go proxy routes to the given group. Endpoints:
//
//	GET  /<module>/@v/list
//	GET  /<module>/@v/<version>.info
//	GET  /<module>/@v/<version>.mod
//	GET  /<module>/@v/<version>.zip
//	GET  /<module>/@latest
//	PUT  /upload                (non-standard, mirror population)
func (h *Handler) Register(g *gin.RouterGroup) {
	g.PUT("/upload", h.upload)

	// Catch-all for the proxy protocol. Gin's tree can't match
	// "<arbitrary-segments>/@v/<file>" with a single pattern, so we use
	// *path and split in code.
	g.GET("/*path", h.proxy)
}

// proxy dispatches a single GET into one of the protocol operations.
func (h *Handler) proxy(c *gin.Context) {
	raw := strings.TrimPrefix(c.Param("path"), "/")
	if raw == "" {
		c.String(http.StatusNotFound, "not found")
		return
	}

	// @latest: /<module>/@latest
	if i := strings.LastIndex(raw, "/@latest"); i != -1 && i+len("/@latest") == len(raw) {
		h.latest(c, raw[:i])
		return
	}

	// @v: /<module>/@v/<rest>
	atV := "/@v/"
	i := strings.LastIndex(raw, atV)
	if i == -1 {
		c.String(http.StatusNotFound, "not found")
		return
	}
	module := raw[:i]
	rest := raw[i+len(atV):]
	if module == "" || rest == "" {
		c.String(http.StatusNotFound, "not found")
		return
	}

	switch {
	case rest == "list":
		h.list(c, module)
	case strings.HasSuffix(rest, ".info"):
		h.info(c, module, strings.TrimSuffix(rest, ".info"))
	case strings.HasSuffix(rest, ".mod"):
		h.mod(c, module, strings.TrimSuffix(rest, ".mod"))
	case strings.HasSuffix(rest, ".zip"):
		h.zip(c, module, strings.TrimSuffix(rest, ".zip"))
	default:
		c.String(http.StatusNotFound, "not found")
	}
}

// list: GET <module>/@v/list — newline-separated versions.
func (h *Handler) list(c *gin.Context, module string) {
	pkg, err := h.Models.GetPackage(c.Request.Context(), models.TypeGo, module)
	if err != nil {
		h.notFoundOrError(c, err)
		return
	}
	versions, err := h.Models.ListVersions(c.Request.Context(), pkg.ID)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].CreatedUnix < versions[j].CreatedUnix })
	c.Header("Content-Type", "text/plain; charset=utf-8")
	for _, v := range versions {
		fmt.Fprintln(c.Writer, v.Version)
	}
}

// info: GET <module>/@v/<version>.info — JSON {Version, Time}.
func (h *Handler) info(c *gin.Context, module, version string) {
	pkg, ver, err := h.resolve(c.Request.Context(), module, version)
	if err != nil {
		h.notFoundOrError(c, err)
		return
	}
	_ = pkg
	c.JSON(http.StatusOK, struct {
		Version string    `json:"Version"`
		Time    time.Time `json:"Time"`
	}{
		Version: ver.Version,
		Time:    time.Unix(ver.CreatedUnix, 0).UTC(),
	})
}

// mod: GET <module>/@v/<version>.mod — go.mod contents.
func (h *Handler) mod(c *gin.Context, module, version string) {
	_, ver, err := h.resolve(c.Request.Context(), module, version)
	if err != nil {
		h.notFoundOrError(c, err)
		return
	}
	goMod, ok, err := h.Models.GetProperty(c.Request.Context(), models.PropertyRefVersion, ver.ID, PropertyGoMod)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	if !ok {
		c.String(http.StatusNotFound, "go.mod not found")
		return
	}
	c.Header("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(c.Writer, goMod)
}

// zip: GET <module>/@v/<version>.zip — module zip bytes.
func (h *Handler) zip(c *gin.Context, module, version string) {
	_, ver, err := h.resolve(c.Request.Context(), module, version)
	if err != nil {
		h.notFoundOrError(c, err)
		return
	}
	files, err := h.Models.ListFilesByVersion(c.Request.Context(), ver.ID)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	if len(files) == 0 {
		c.String(http.StatusNotFound, "no files")
		return
	}
	// Convention: Go modules have a single lead .zip file per version.
	var f *models.File
	for _, candidate := range files {
		if candidate.IsLead {
			f = candidate
			break
		}
	}
	if f == nil {
		f = files[0]
	}
	rc, _, err := h.Service.OpenFile(c.Request.Context(), f)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	defer rc.Close()
	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, f.Name))
	_, _ = io.Copy(c.Writer, rc)
}

// latest: GET <module>/@latest — JSON {Version, Time} for newest version.
func (h *Handler) latest(c *gin.Context, module string) {
	pkg, err := h.Models.GetPackage(c.Request.Context(), models.TypeGo, module)
	if err != nil {
		h.notFoundOrError(c, err)
		return
	}
	ver, err := h.Models.GetLatestVersion(c.Request.Context(), pkg.ID)
	if err != nil {
		h.notFoundOrError(c, err)
		return
	}
	c.JSON(http.StatusOK, struct {
		Version string    `json:"Version"`
		Time    time.Time `json:"Time"`
	}{
		Version: ver.Version,
		Time:    time.Unix(ver.CreatedUnix, 0).UTC(),
	})
}

// upload: PUT /upload — ingest a module zip and create package/version/file.
func (h *Handler) upload(c *gin.Context) {
	buf, err := pkgsvc.NewHashedBufferFromReader(c.Request.Body)
	if err != nil {
		c.String(http.StatusInternalServerError, "buffer upload: %v", err)
		return
	}
	defer buf.Close()

	pkg, err := Parse(buf, buf.Size())
	if err != nil {
		if errors.Is(err, ErrInvalidStructure) || errors.Is(err, ErrGoModFileTooLarge) {
			c.String(http.StatusBadRequest, "%v", err)
			return
		}
		c.String(http.StatusBadRequest, "parse zip: %v", err)
		return
	}

	_, _, _, err = h.Service.CreatePackageAndAddFile(c.Request.Context(), pkgsvc.CreationInfo{
		PackageType: models.TypeGo,
		PackageName: pkg.Name,
		Version:     pkg.Version,
		VersionProperties: map[string]string{
			PropertyGoMod: pkg.GoMod,
		},
		Filename: fmt.Sprintf("%s.zip", pkg.Version),
		IsLead:   true,
	}, buf)
	if err != nil {
		if errors.Is(err, models.ErrDuplicatePackageVersion) {
			c.String(http.StatusConflict, "version already exists")
			return
		}
		c.String(http.StatusInternalServerError, "ingest: %v", err)
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"module":  pkg.Name,
		"version": pkg.Version,
	})
}

// resolve looks up the package by module path and the version (handling
// "latest" specially), returning the typed records.
func (h *Handler) resolve(ctx context.Context, module, version string) (*models.Package, *models.Version, error) {
	pkg, err := h.Models.GetPackage(ctx, models.TypeGo, module)
	if err != nil {
		return nil, nil, err
	}
	if version == "latest" {
		ver, err := h.Models.GetLatestVersion(ctx, pkg.ID)
		if err != nil {
			return pkg, nil, err
		}
		return pkg, ver, nil
	}
	ver, err := h.Models.GetVersion(ctx, pkg.ID, version)
	if err != nil {
		return pkg, nil, err
	}
	return pkg, ver, nil
}

// notFoundOrError maps known sentinel errors to 404 and unknown errors to 500.
func (h *Handler) notFoundOrError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, models.ErrPackageNotExist),
		errors.Is(err, models.ErrVersionNotExist),
		errors.Is(err, models.ErrFileNotExist):
		c.String(http.StatusNotFound, "%v", err)
	default:
		c.String(http.StatusInternalServerError, "%v", err)
	}
}
