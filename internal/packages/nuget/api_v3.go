// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2021 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// V3 response shapes for the NuGet HTTP API. Direct transliteration
// of forgejo/routers/api/packages/nuget/api_v3.go (MIT) plus the
// `linkBuilder` from links.go. Field shapes and JSON tags are
// preserved verbatim so a stock `dotnet` client sees byte-compatible
// documents.
//
// pkgmirror intentionally ships V3 only. The legacy V2 (OData/Atom)
// protocol that Forgejo also exposes is needed only for very old
// `nuget.exe` clients; the modern `dotnet nuget` CLI and Visual
// Studio 2017+ speak V3 exclusively.

package nuget

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/astockwell/pkgmirror/internal/models"
)

// ServiceIndexResponseV3 — https://docs.microsoft.com/en-us/nuget/api/service-index#resources
type ServiceIndexResponseV3 struct {
	Version   string            `json:"version"`
	Resources []ServiceResource `json:"resources"`
}

// ServiceResource — one entry in the service index.
type ServiceResource struct {
	ID   string `json:"@id"`
	Type string `json:"@type"`
}

// linkBuilder constructs absolute URLs for all V3 response @id
// fields. `Base` is the absolute URL of the NuGet endpoint root for
// the current tenant (e.g. https://example.com/api/packages/foo/nuget),
// re-derived per-request from the incoming Host header + TLS state so
// the rewriter works without explicit base-URL configuration.
type linkBuilder struct {
	Base string
}

func (l *linkBuilder) ServiceIndex() *ServiceIndexResponseV3 {
	return &ServiceIndexResponseV3{
		Version: "3.0.0",
		Resources: []ServiceResource{
			{ID: l.Base + "/query", Type: "SearchQueryService"},
			{ID: l.Base + "/query", Type: "SearchQueryService/3.0.0-beta"},
			{ID: l.Base + "/query", Type: "SearchQueryService/3.0.0-rc"},
			{ID: l.Base + "/registration", Type: "RegistrationsBaseUrl"},
			{ID: l.Base + "/registration", Type: "RegistrationsBaseUrl/3.0.0-beta"},
			{ID: l.Base + "/registration", Type: "RegistrationsBaseUrl/3.0.0-rc"},
			{ID: l.Base + "/package", Type: "PackageBaseAddress/3.0.0"},
			{ID: l.Base, Type: "PackagePublish/2.0.0"},
		},
	}
}

func (l *linkBuilder) RegistrationIndexURL(id string) string {
	return fmt.Sprintf("%s/registration/%s/index.json", l.Base, strings.ToLower(id))
}

func (l *linkBuilder) RegistrationLeafURL(id, version string) string {
	return fmt.Sprintf("%s/registration/%s/%s.json", l.Base, strings.ToLower(id), strings.ToLower(version))
}

func (l *linkBuilder) PackageDownloadURL(id, version string) string {
	lid := strings.ToLower(id)
	lver := strings.ToLower(version)
	return fmt.Sprintf("%s/package/%s/%s/%s.%s.nupkg", l.Base, lid, lver, lid, lver)
}

// RegistrationIndexResponse — https://docs.microsoft.com/en-us/nuget/api/registration-base-url-resource#response
type RegistrationIndexResponse struct {
	RegistrationIndexURL string                   `json:"@id"`
	Type                 []string                 `json:"@type"`
	Count                int                      `json:"count"`
	Pages                []*RegistrationIndexPage `json:"items"`
}

// RegistrationIndexPage — https://docs.microsoft.com/en-us/nuget/api/registration-base-url-resource#registration-page-object
type RegistrationIndexPage struct {
	RegistrationPageURL string                       `json:"@id"`
	Lower               string                       `json:"lower"`
	Upper               string                       `json:"upper"`
	Count               int                          `json:"count"`
	Items               []*RegistrationIndexPageItem `json:"items"`
}

// RegistrationIndexPageItem — https://docs.microsoft.com/en-us/nuget/api/registration-base-url-resource#registration-leaf-object-in-a-page
type RegistrationIndexPageItem struct {
	RegistrationLeafURL string        `json:"@id"`
	PackageContentURL   string        `json:"packageContent"`
	CatalogEntry        *CatalogEntry `json:"catalogEntry"`
}

// CatalogEntry — https://docs.microsoft.com/en-us/nuget/api/registration-base-url-resource#catalog-entry
type CatalogEntry struct {
	CatalogLeafURL           string                    `json:"@id"`
	PackageContentURL        string                    `json:"packageContent"`
	ID                       string                    `json:"id"`
	Version                  string                    `json:"version"`
	Description              string                    `json:"description"`
	ReleaseNotes             string                    `json:"releaseNotes"`
	Authors                  string                    `json:"authors"`
	RequireLicenseAcceptance bool                      `json:"requireLicenseAcceptance"`
	ProjectURL               string                    `json:"projectURL"`
	DependencyGroups         []*PackageDependencyGroup `json:"dependencyGroups"`
}

// PackageDependencyGroup — https://docs.microsoft.com/en-us/nuget/api/registration-base-url-resource#package-dependency-group
type PackageDependencyGroup struct {
	TargetFramework string               `json:"targetFramework"`
	Dependencies    []*PackageDependency `json:"dependencies"`
}

// PackageDependency — https://docs.microsoft.com/en-us/nuget/api/registration-base-url-resource#package-dependency
type PackageDependency struct {
	ID    string `json:"id"`
	Range string `json:"range"`
}

// RegistrationLeafResponse — https://docs.microsoft.com/en-us/nuget/api/registration-base-url-resource#registration-leaf
type RegistrationLeafResponse struct {
	RegistrationLeafURL  string `json:"@id"`
	Type                 []string `json:"@type"`
	Listed               bool   `json:"listed"`
	PackageContentURL    string `json:"packageContent"`
	Published            string `json:"published"`
	RegistrationIndexURL string `json:"registration"`
}

// PackageVersionsResponse — https://docs.microsoft.com/en-us/nuget/api/package-base-address-resource#response
type PackageVersionsResponse struct {
	Versions []string `json:"versions"`
}

// SearchResultResponse — https://docs.microsoft.com/en-us/nuget/api/search-query-service-resource#response
type SearchResultResponse struct {
	TotalHits int64           `json:"totalHits"`
	Data      []*SearchResult `json:"data"`
}

// SearchResult — https://docs.microsoft.com/en-us/nuget/api/search-query-service-resource#search-result
type SearchResult struct {
	ID                   string                 `json:"id"`
	Version              string                 `json:"version"`
	Versions             []*SearchResultVersion `json:"versions"`
	Description          string                 `json:"description"`
	Authors              string                 `json:"authors"`
	ProjectURL           string                 `json:"projectURL"`
	RegistrationIndexURL string                 `json:"registration"`
}

// SearchResultVersion — one entry under SearchResult.Versions.
type SearchResultVersion struct {
	RegistrationLeafURL string `json:"@id"`
	Version             string `json:"version"`
	Downloads           int64  `json:"downloads"`
}

// versionEntry pairs a NuGet-normalized version string with the
// underlying pkgmirror Version + parsed Metadata. The builders below
// consume slices of these; the handler is responsible for assembling
// them from DB rows.
type versionEntry struct {
	ID       string // package id (case-preserved per first upload)
	Version  string // NuGet-normalized version
	Metadata *Metadata
	Ver      *models.Version
}

// sortByVersion orders entries oldest-first by created_unix, which
// matches the contract documented in
// docs/adding-a-format.md#implicit-contracts-of-the-shared-layer.
// Registration indices need ascending order; the search result
// flattening uses created_unix ties to break case-insensitive id
// ties when grouping.
func sortByVersion(entries []*versionEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Ver.CreatedUnix < entries[j].Ver.CreatedUnix
	})
}

// buildRegistrationIndex flattens the per-package version list into a
// V3 registration index document. All URL fields are absolute and
// rooted at l.Base.
func buildRegistrationIndex(l *linkBuilder, id string, entries []*versionEntry) *RegistrationIndexResponse {
	sortByVersion(entries)
	items := make([]*RegistrationIndexPageItem, 0, len(entries))
	for _, e := range entries {
		items = append(items, &RegistrationIndexPageItem{
			RegistrationLeafURL: l.RegistrationLeafURL(id, e.Version),
			PackageContentURL:   l.PackageDownloadURL(id, e.Version),
			CatalogEntry: &CatalogEntry{
				CatalogLeafURL:           l.RegistrationLeafURL(id, e.Version),
				PackageContentURL:        l.PackageDownloadURL(id, e.Version),
				ID:                       id,
				Version:                  e.Version,
				Description:              metadataString(e.Metadata, func(m *Metadata) string { return m.Description }),
				ReleaseNotes:             metadataString(e.Metadata, func(m *Metadata) string { return m.ReleaseNotes }),
				Authors:                  metadataString(e.Metadata, func(m *Metadata) string { return m.Authors }),
				ProjectURL:               metadataString(e.Metadata, func(m *Metadata) string { return m.ProjectURL }),
				RequireLicenseAcceptance: e.Metadata != nil && e.Metadata.RequireLicenseAcceptance,
				DependencyGroups:         buildDependencyGroups(e.Metadata),
			},
		})
	}
	return &RegistrationIndexResponse{
		RegistrationIndexURL: l.RegistrationIndexURL(id),
		Type:                 []string{"catalog:CatalogRoot", "PackageRegistration", "catalog:Permalink"},
		Count:                1,
		Pages: []*RegistrationIndexPage{
			{
				RegistrationPageURL: l.RegistrationIndexURL(id),
				Count:               len(entries),
				Lower:               entries[0].Version,
				Upper:               entries[len(entries)-1].Version,
				Items:               items,
			},
		},
	}
}

func buildDependencyGroups(m *Metadata) []*PackageDependencyGroup {
	if m == nil {
		return nil
	}
	groups := make([]*PackageDependencyGroup, 0, len(m.Dependencies))
	// Iterate in sorted key order so output bytes are stable across
	// calls (important for any caller that hashes the response).
	keys := make([]string, 0, len(m.Dependencies))
	for k := range m.Dependencies {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		deps := make([]*PackageDependency, 0, len(m.Dependencies[k]))
		for _, d := range m.Dependencies[k] {
			deps = append(deps, &PackageDependency{ID: d.ID, Range: d.Version})
		}
		groups = append(groups, &PackageDependencyGroup{TargetFramework: k, Dependencies: deps})
	}
	return groups
}

// metadataString safely deref's Metadata without panicking on
// versions ingested before we tightened the upload path.
func metadataString(m *Metadata, get func(*Metadata) string) string {
	if m == nil {
		return ""
	}
	return get(m)
}

// buildRegistrationLeaf produces the per-version leaf document.
func buildRegistrationLeaf(l *linkBuilder, id string, e *versionEntry) *RegistrationLeafResponse {
	return &RegistrationLeafResponse{
		Type:                 []string{"Package", "http://schema.nuget.org/catalog#Permalink"},
		Listed:               true,
		Published:            iso8601Unix(e.Ver.CreatedUnix),
		RegistrationLeafURL:  l.RegistrationLeafURL(id, e.Version),
		PackageContentURL:    l.PackageDownloadURL(id, e.Version),
		RegistrationIndexURL: l.RegistrationIndexURL(id),
	}
}

// buildPackageVersions returns the package-base-address index for a
// package (the `/package/<id>/index.json` document). Versions are
// emitted oldest-first; dotnet sorts client-side so the wire order
// doesn't actually matter for correctness.
func buildPackageVersions(entries []*versionEntry) *PackageVersionsResponse {
	sortByVersion(entries)
	versions := make([]string, 0, len(entries))
	for _, e := range entries {
		versions = append(versions, e.Version)
	}
	return &PackageVersionsResponse{Versions: versions}
}

// buildSearchResults flattens entries grouped by package id into the
// V3 search response document. `total` is the count of distinct
// packages (Forgejo's totalHits semantic).
func buildSearchResults(l *linkBuilder, total int64, grouped map[string][]*versionEntry) *SearchResultResponse {
	keys := make([]string, 0, len(grouped))
	for k := range grouped {
		keys = append(keys, k)
	}
	// Case-insensitive lexical order so paged responses are stable;
	// we don't currently page but the order is part of the wire
	// contract clients display.
	sort.Slice(keys, func(i, j int) bool {
		return lowerFold(keys[i]) < lowerFold(keys[j])
	})
	data := make([]*SearchResult, 0, len(keys))
	for _, k := range keys {
		es := grouped[k]
		sortByVersion(es)
		latest := es[len(es)-1]
		versions := make([]*SearchResultVersion, 0, len(es))
		for _, e := range es {
			versions = append(versions, &SearchResultVersion{
				RegistrationLeafURL: l.RegistrationLeafURL(latest.ID, e.Version),
				Version:             e.Version,
			})
		}
		data = append(data, &SearchResult{
			ID:                   latest.ID,
			Version:              latest.Version,
			Versions:             versions,
			Description:          metadataString(latest.Metadata, func(m *Metadata) string { return m.Description }),
			Authors:              metadataString(latest.Metadata, func(m *Metadata) string { return m.Authors }),
			ProjectURL:           metadataString(latest.Metadata, func(m *Metadata) string { return m.ProjectURL }),
			RegistrationIndexURL: l.RegistrationIndexURL(latest.ID),
		})
	}
	return &SearchResultResponse{TotalHits: total, Data: data}
}

// iso8601Unix renders a UNIX timestamp in the RFC 3339 / ISO 8601
// shape NuGet's V3 spec mandates for `published`.
func iso8601Unix(unix int64) string {
	return time.Unix(unix, 0).UTC().Format("2006-01-02T15:04:05.000Z")
}

// lowerFold is a thin alias for strings.ToLower used in the search
// flattening loop. Centralizing the call site keeps the sort closure
// readable.
func lowerFold(s string) string { return strings.ToLower(s) }
