// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2021 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// This file is a transliteration of
// forgejo/modules/packages/nuget/metadata.go from the Forgejo project
// (https://codeberg.org/forgejo/forgejo), which is itself MIT
// licensed. The .nuspec XML shape, the package-type discrimination
// (DependencyPackage vs SymbolsPackage), the validation rules (id
// regex, project-URL sanity check, semver normalization) and the
// dependency-group flattening preserve the upstream algorithm
// byte-for-byte. Differences are limited to:
//
//   - we use net/url + a tiny isValidURL helper instead of
//     forgejo.org/modules/validation.IsValidURL;
//   - we don't carry forgejo's util.NewInvalidArgumentErrorf wrapper
//     since pkgmirror's handler maps errors to HTTP status by
//     errors.Is on the sentinels defined here.

package nuget

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hashicorp/go-version"
)

// Sentinel errors mirror Forgejo's. Handlers map them to 400.
var (
	ErrMissingNuspecFile    = errors.New("nuspec file is missing")
	ErrNuspecFileTooLarge   = errors.New("nuspec file is too large")
	ErrNuspecInvalidID      = errors.New("nuspec id is invalid")
	ErrNuspecInvalidVersion = errors.New("nuspec version is invalid")
)

// PackageType discriminates between dependency packages (.nupkg) and
// symbol packages (.snupkg). Pkgmirror currently ingests both but only
// fully indexes dependency packages.
type PackageType int

const (
	DependencyPackage PackageType = iota + 1
	SymbolsPackage
)

// PropertySymbolID is the property name used to associate a symbol
// file (pdb) with its source GUID for the simple-symbol-query
// protocol. Kept for parity with Forgejo; pkgmirror does not currently
// extract portable PDBs.
const PropertySymbolID = "nuget.symbol.id"

// maxNuspecFileSize caps the in-memory nuspec read at 3 MiB; matches
// Forgejo's limit.
const maxNuspecFileSize = 3 * 1024 * 1024

// idmatch is the NuGet package-id grammar: a leading word character
// followed by zero or more `.`- or `-`-separated word groups. Matches
// upstream verbatim.
var idmatch = regexp.MustCompile(`\A\w+(?:[.-]\w+)*\z`)

// Package is the result of parsing a .nupkg/.snupkg.
type Package struct {
	PackageType   PackageType
	ID            string
	Version       string
	Metadata      *Metadata
	NuspecContent *bytes.Buffer
}

// Metadata is the JSON-friendly view of <metadata> in the .nuspec.
// Field shapes match Forgejo's so the same downstream consumers
// (Registration / Search responses) work without translation.
type Metadata struct {
	Title                    string                  `json:"title,omitempty"`
	Language                 string                  `json:"language,omitempty"`
	Description              string                  `json:"description,omitempty"`
	ReleaseNotes             string                  `json:"release_notes,omitempty"`
	Readme                   string                  `json:"readme,omitempty"`
	Authors                  string                  `json:"authors,omitempty"`
	Owners                   string                  `json:"owners,omitempty"`
	Copyright                string                  `json:"copyright,omitempty"`
	ProjectURL               string                  `json:"project_url,omitempty"`
	RepositoryURL            string                  `json:"repository_url,omitempty"`
	LicenseURL               string                  `json:"license_url,omitempty"`
	IconURL                  string                  `json:"icon_url,omitempty"`
	MinClientVersion         string                  `json:"min_client_version,omitempty"`
	Tags                     string                  `json:"tags,omitempty"`
	DevelopmentDependency    bool                    `json:"development_dependency,omitempty"`
	RequireLicenseAcceptance bool                    `json:"require_license_acceptance"`
	Dependencies             map[string][]Dependency `json:"dependencies,omitempty"`
}

// Dependency is a single package-id + version-range pair under one
// targetFramework key in Metadata.Dependencies.
type Dependency struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// nuspecPackage mirrors the .nuspec XML schema documented at
// https://learn.microsoft.com/en-us/nuget/reference/nuspec.
type nuspecPackage struct {
	Metadata struct {
		ID                       string `xml:"id"`
		Title                    string `xml:"title"`
		Language                 string `xml:"language"`
		Version                  string `xml:"version"`
		Authors                  string `xml:"authors"`
		Owners                   string `xml:"owners"`
		Copyright                string `xml:"copyright"`
		DevelopmentDependency    bool   `xml:"developmentDependency"`
		RequireLicenseAcceptance bool   `xml:"requireLicenseAcceptance"`
		ProjectURL               string `xml:"projectUrl"`
		LicenseURL               string `xml:"licenseUrl"`
		IconURL                  string `xml:"iconUrl"`
		Description              string `xml:"description"`
		ReleaseNotes             string `xml:"releaseNotes"`
		Readme                   string `xml:"readme"`
		Tags                     string `xml:"tags"`
		MinClientVersion         string `xml:"minClientVersion,attr"`
		PackageTypes             struct {
			PackageType []struct {
				Name string `xml:"name,attr"`
			} `xml:"packageType"`
		} `xml:"packageTypes"`
		Repository struct {
			URL string `xml:"url,attr"`
		} `xml:"repository"`
		Dependencies struct {
			Dependency []struct {
				ID      string `xml:"id,attr"`
				Version string `xml:"version,attr"`
				Exclude string `xml:"exclude,attr"`
			} `xml:"dependency"`
			Group []struct {
				TargetFramework string `xml:"targetFramework,attr"`
				Dependency      []struct {
					ID      string `xml:"id,attr"`
					Version string `xml:"version,attr"`
					Exclude string `xml:"exclude,attr"`
				} `xml:"dependency"`
			} `xml:"group"`
		} `xml:"dependencies"`
	} `xml:"metadata"`
}

// ParsePackage opens r as a zip archive (a .nupkg or .snupkg) and
// returns the parsed metadata. r must be at the start of the archive.
func ParsePackage(r io.ReaderAt, size int64) (*Package, error) {
	archive, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("read zip: %w", err)
	}
	for _, file := range archive.File {
		// .nuspec lives at the archive root; ignore anything under a
		// subdirectory (matches Forgejo and the spec).
		if filepath.Dir(file.Name) != "." {
			continue
		}
		if !strings.HasSuffix(strings.ToLower(file.Name), ".nuspec") {
			continue
		}
		if file.UncompressedSize64 > maxNuspecFileSize {
			return nil, ErrNuspecFileTooLarge
		}
		f, err := archive.Open(file.Name)
		if err != nil {
			return nil, fmt.Errorf("open nuspec: %w", err)
		}
		defer f.Close()
		return parseNuspec(archive, f)
	}
	return nil, ErrMissingNuspecFile
}

func parseNuspec(archive *zip.Reader, r io.Reader) (*Package, error) {
	var nuspecBuf bytes.Buffer
	var p nuspecPackage
	if err := xml.NewDecoder(io.TeeReader(r, &nuspecBuf)).Decode(&p); err != nil {
		return nil, fmt.Errorf("decode nuspec: %w", err)
	}
	if !idmatch.MatchString(p.Metadata.ID) {
		return nil, ErrNuspecInvalidID
	}
	v, err := version.NewSemver(p.Metadata.Version)
	if err != nil {
		return nil, ErrNuspecInvalidVersion
	}
	if !isValidURL(p.Metadata.ProjectURL) {
		p.Metadata.ProjectURL = ""
	}

	packageType := DependencyPackage
	for _, pt := range p.Metadata.PackageTypes.PackageType {
		if pt.Name == "SymbolsPackage" {
			packageType = SymbolsPackage
			break
		}
	}

	m := &Metadata{
		Title:                    p.Metadata.Title,
		Language:                 p.Metadata.Language,
		Description:              p.Metadata.Description,
		ReleaseNotes:             p.Metadata.ReleaseNotes,
		Authors:                  p.Metadata.Authors,
		Owners:                   p.Metadata.Owners,
		Copyright:                p.Metadata.Copyright,
		ProjectURL:               p.Metadata.ProjectURL,
		RepositoryURL:            p.Metadata.Repository.URL,
		LicenseURL:               p.Metadata.LicenseURL,
		IconURL:                  p.Metadata.IconURL,
		MinClientVersion:         p.Metadata.MinClientVersion,
		Tags:                     p.Metadata.Tags,
		DevelopmentDependency:    p.Metadata.DevelopmentDependency,
		RequireLicenseAcceptance: p.Metadata.RequireLicenseAcceptance,
		Dependencies:             make(map[string][]Dependency),
	}

	if p.Metadata.Readme != "" {
		if f, err := archive.Open(p.Metadata.Readme); err == nil {
			buf, _ := io.ReadAll(f)
			m.Readme = string(buf)
			_ = f.Close()
		}
	}

	if len(p.Metadata.Dependencies.Dependency) > 0 {
		deps := make([]Dependency, 0, len(p.Metadata.Dependencies.Dependency))
		for _, dep := range p.Metadata.Dependencies.Dependency {
			if dep.ID == "" || dep.Version == "" {
				continue
			}
			deps = append(deps, Dependency{ID: dep.ID, Version: dep.Version})
		}
		m.Dependencies[""] = deps
	}
	for _, group := range p.Metadata.Dependencies.Group {
		deps := make([]Dependency, 0, len(group.Dependency))
		for _, dep := range group.Dependency {
			if dep.ID == "" || dep.Version == "" {
				continue
			}
			deps = append(deps, Dependency{ID: dep.ID, Version: dep.Version})
		}
		if len(deps) > 0 {
			m.Dependencies[group.TargetFramework] = deps
		}
	}

	return &Package{
		PackageType:   packageType,
		ID:            p.Metadata.ID,
		Version:       toNormalizedVersion(v),
		Metadata:      m,
		NuspecContent: &nuspecBuf,
	}, nil
}

// toNormalizedVersion applies the NuGet "normalized version number"
// rules documented at
// https://learn.microsoft.com/en-us/nuget/concepts/package-versioning#normalized-version-numbers
// and implemented at
// https://github.com/NuGet/NuGet.Client/blob/dccbd304b11103e08b97abf4cf4bcc1499d9235a/src/NuGet.Core/NuGet.Versioning/VersionFormatter.cs#L121
// Build metadata (the +xxx suffix) is dropped; pre-release tags are
// kept; the revision (4th segment) is emitted only when non-zero.
func toNormalizedVersion(v *version.Version) string {
	var buf bytes.Buffer
	segments := v.Segments64()
	fmt.Fprintf(&buf, "%d.%d.%d", segments[0], segments[1], segments[2])
	if len(segments) > 3 && segments[3] > 0 {
		fmt.Fprintf(&buf, ".%d", segments[3])
	}
	if pre := v.Prerelease(); pre != "" {
		fmt.Fprint(&buf, "-", pre)
	}
	return buf.String()
}

// isValidURL reports whether s parses as an absolute http(s) URL.
// Same posture as the rubygems / maven parsers.
func isValidURL(s string) bool {
	if s == "" {
		return false
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.Host == "" {
		return false
	}
	return true
}
