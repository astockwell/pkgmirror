// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2021 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// This file is a transliteration of forgejo/modules/packages/rubygems/metadata.go
// from the Forgejo project (https://codeberg.org/forgejo/forgejo), which is
// itself MIT licensed. The parser preserves the upstream gemspec YAML
// shape and Ruby-style dependency handling. Differences are limited to:
//
//   - Sentinel errors are plain errors.New() instead of forgejo.util's
//     util.NewInvalidArgumentErrorf. Calling code uses errors.Is to
//     route them to HTTP 400.
//   - URL validation is a small inline helper (we don't need Forgejo's
//     full validation module).
//   - "go.yaml.in/yaml/v3" is replaced by "gopkg.in/yaml.v3" which is
//     already a transitive dep of the policy YAML loader.

package rubygems

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"net/url"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Sentinels. Callers use errors.Is to map these to 400 Bad Request.
var (
	// ErrMissingMetadataFile indicates the uploaded .gem does not contain
	// the required inner metadata.gz entry.
	ErrMissingMetadataFile = errors.New("rubygems: metadata.gz file is missing")
	// ErrInvalidName indicates the gem name is missing or contains a path
	// separator.
	ErrInvalidName = errors.New("rubygems: package name is invalid")
	// ErrInvalidVersion indicates the gem version does not match the
	// permitted shape.
	ErrInvalidVersion = errors.New("rubygems: package version is invalid")
)

// versionMatcher matches RubyGems version strings per
// https://guides.rubygems.org/patterns/#semantic-versioning. We deliberately
// allow non-SemVer-strict shapes (e.g. "1.0.0.beta1") because gem clients
// happily produce them in the wild.
var versionMatcher = regexp.MustCompile(`\A[0-9]+(?:\.[0-9a-zA-Z]+)*(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?\z`)

// Package represents a parsed .gem upload.
type Package struct {
	Name     string
	Version  string
	Metadata *Metadata
}

// Metadata is the structured form we persist into package_versions.metadata
// (as JSON). It's also surfaced from /info/<gem> and the Marshal-encoded
// /specs.4.8.gz responses.
type Metadata struct {
	Platform                string               `json:"platform,omitempty"`
	Description             string               `json:"description,omitempty"`
	Summary                 string               `json:"summary,omitempty"`
	Authors                 []string             `json:"authors,omitempty"`
	Licenses                []string             `json:"licenses,omitempty"`
	RequiredRubyVersion     []VersionRequirement `json:"required_ruby_version,omitempty"`
	RequiredRubygemsVersion []VersionRequirement `json:"required_rubygems_version,omitempty"`
	ProjectURL              string               `json:"project_url,omitempty"`
	RuntimeDependencies     []Dependency         `json:"runtime_dependencies,omitempty"`
	DevelopmentDependencies []Dependency         `json:"development_dependencies,omitempty"`
}

// VersionRequirement is a single (restriction, version) pair like
// ("~>", "1.2"). RubyGems represents version constraints as a list of these.
type VersionRequirement struct {
	Restriction string `json:"restriction"`
	Version     string `json:"version"`
}

// Dependency is a single runtime or development dependency.
type Dependency struct {
	Name    string               `json:"name"`
	Version []VersionRequirement `json:"version"`
}

// gemspec is the YAML shape produced by `Gem::Specification#to_yaml`.
type gemspec struct {
	Name    string `yaml:"name"`
	Version struct {
		Version string `yaml:"version"`
	} `yaml:"version"`
	Platform     string   `yaml:"platform"`
	Authors      []string `yaml:"authors"`
	Autorequire  any      `yaml:"autorequire"`
	Bindir       string   `yaml:"bindir"`
	CertChain    []any    `yaml:"cert_chain"`
	Date         string   `yaml:"date"`
	Dependencies []struct {
		Name                string      `yaml:"name"`
		Requirement         requirement `yaml:"requirement"`
		Type                string      `yaml:"type"`
		Prerelease          bool        `yaml:"prerelease"`
		VersionRequirements requirement `yaml:"version_requirements"`
	} `yaml:"dependencies"`
	Description    string   `yaml:"description"`
	Executables    []string `yaml:"executables"`
	Extensions     []any    `yaml:"extensions"`
	ExtraRdocFiles []string `yaml:"extra_rdoc_files"`
	Files          []string `yaml:"files"`
	Homepage       string   `yaml:"homepage"`
	Licenses       []string `yaml:"licenses"`
	Metadata       struct {
		BugTrackerURI    string `yaml:"bug_tracker_uri"`
		ChangelogURI     string `yaml:"changelog_uri"`
		DocumentationURI string `yaml:"documentation_uri"`
		SourceCodeURI    string `yaml:"source_code_uri"`
	} `yaml:"metadata"`
	PostInstallMessage      any         `yaml:"post_install_message"`
	RdocOptions             []any       `yaml:"rdoc_options"`
	RequirePaths            []string    `yaml:"require_paths"`
	RequiredRubyVersion     requirement `yaml:"required_ruby_version"`
	RequiredRubygemsVersion requirement `yaml:"required_rubygems_version"`
	Requirements            []any       `yaml:"requirements"`
	RubygemsVersion         string      `yaml:"rubygems_version"`
	SigningKey              any         `yaml:"signing_key"`
	SpecificationVersion    int         `yaml:"specification_version"`
	Summary                 string      `yaml:"summary"`
	TestFiles               []any       `yaml:"test_files"`
}

type requirement struct {
	Requirements [][]any `yaml:"requirements"`
}

// AsVersionRequirement converts the YAML-decoded shape into our typed slice.
// Drops the meaningless ">= 0" wildcard so it doesn't pollute manifests.
func (r requirement) AsVersionRequirement() []VersionRequirement {
	requirements := make([]VersionRequirement, 0, len(r.Requirements))
	for _, req := range r.Requirements {
		if len(req) != 2 {
			continue
		}
		restriction, ok := req[0].(string)
		if !ok {
			continue
		}
		vm, ok := req[1].(map[string]any)
		if !ok {
			continue
		}
		versionInt, ok := vm["version"]
		if !ok {
			continue
		}
		version, ok := versionInt.(string)
		if !ok {
			continue
		}
		if restriction == ">=" && version == "0" {
			continue
		}
		requirements = append(requirements, VersionRequirement{
			Restriction: restriction,
			Version:     version,
		})
	}
	return requirements
}

// ParsePackageMetaData parses the metadata of a .gem package.
// A .gem is a tar archive containing (at minimum) metadata.gz and data.tar.gz;
// we read metadata.gz only.
func ParsePackageMetaData(r io.Reader) (*Package, error) {
	archive := tar.NewReader(r)
	for {
		hdr, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name == "metadata.gz" {
			return parseMetadataFile(archive)
		}
	}
	return nil, ErrMissingMetadataFile
}

func parseMetadataFile(r io.Reader) (*Package, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	var spec gemspec
	if err := yaml.NewDecoder(zr).Decode(&spec); err != nil {
		return nil, err
	}

	if len(spec.Name) == 0 || strings.Contains(spec.Name, "/") {
		return nil, ErrInvalidName
	}
	if !versionMatcher.MatchString(spec.Version.Version) {
		return nil, ErrInvalidVersion
	}

	if !isValidURL(spec.Homepage) {
		spec.Homepage = ""
	}
	if !isValidURL(spec.Metadata.SourceCodeURI) {
		spec.Metadata.SourceCodeURI = ""
	}

	m := &Metadata{
		Platform:                spec.Platform,
		Description:             spec.Description,
		Summary:                 spec.Summary,
		Authors:                 spec.Authors,
		Licenses:                spec.Licenses,
		ProjectURL:              spec.Homepage,
		RequiredRubyVersion:     spec.RequiredRubyVersion.AsVersionRequirement(),
		RequiredRubygemsVersion: spec.RequiredRubygemsVersion.AsVersionRequirement(),
		DevelopmentDependencies: make([]Dependency, 0, 5),
		RuntimeDependencies:     make([]Dependency, 0, 5),
	}

	for _, gemdep := range spec.Dependencies {
		dep := Dependency{
			Name:    gemdep.Name,
			Version: gemdep.Requirement.AsVersionRequirement(),
		}
		// Forgejo's gemspec produces ":runtime" / ":development" with the
		// leading colon because Ruby Symbols are serialized that way by
		// some emitters. We accept both.
		if gemdep.Type == ":runtime" || gemdep.Type == "runtime" {
			m.RuntimeDependencies = append(m.RuntimeDependencies, dep)
		} else {
			m.DevelopmentDependencies = append(m.DevelopmentDependencies, dep)
		}
	}

	return &Package{
		Name:     spec.Name,
		Version:  spec.Version.Version,
		Metadata: m,
	}, nil
}

// isValidURL reports whether s parses as an absolute http(s) URL.
// Modeled on the bits of forgejo/modules/validation we need.
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
	return u.Host != ""
}

// FullName returns "<name>-<version>" or "<name>-<version>-<platform>" for
// non-ruby platforms. Matches Forgejo's getFullName.
func FullName(name, version, platform string) string {
	if platform == "" || platform == "ruby" {
		return name + "-" + version
	}
	return name + "-" + version + "-" + platform
}

// FullFilename returns the canonical .gem filename for a (name, version,
// platform) triple. Always lowercase.
func FullFilename(name, version, platform string) string {
	return strings.ToLower(FullName(name, version, platform)) + ".gem"
}
