// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2021 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// POM (pom.xml) metadata parser. Direct transliteration of
// forgejo/modules/packages/maven/metadata.go (MIT), which is itself
// the upstream Gitea code. Differences from upstream:
//
//   - we replace `forgejo.org/modules/util` and `validation` with
//     stdlib equivalents (errors.New for the sentinel, net/url for
//     project-url validation)
//   - we don't pull in `forgejo.org/modules/validation.IsValidURL`
//     and instead inline a minimal http/https check
//
// The POM parsing semantics (charset-aware XML decoder, optional
// parent-inheritance of groupId, license/dependency extraction) are
// preserved byte-for-byte so real `mvn` and `gradle` clients see the
// same metadata pkgmirror generates as they would from any other
// Maven repo.

package maven

import (
	"encoding/xml"
	"errors"
	"io"
	"net/url"

	"golang.org/x/net/html/charset"
)

// Metadata is the per-version metadata extracted from a pom.xml. The
// shape matches forgejo's so JSON-encoded values are easy to compare.
type Metadata struct {
	GroupID      string        `json:"group_id,omitempty"`
	ArtifactID   string        `json:"artifact_id,omitempty"`
	Name         string        `json:"name,omitempty"`
	Description  string        `json:"description,omitempty"`
	ProjectURL   string        `json:"project_url,omitempty"`
	Licenses     []string      `json:"licenses,omitempty"`
	Dependencies []*Dependency `json:"dependencies,omitempty"`
}

// Dependency is a single <dependency> entry from the POM.
type Dependency struct {
	GroupID    string `json:"group_id,omitempty"`
	ArtifactID string `json:"artifact_id,omitempty"`
	Version    string `json:"version,omitempty"`
}

// pomStruct is the on-wire XML shape we decode into.
type pomStruct struct {
	XMLName     xml.Name `xml:"project"`
	GroupID     string   `xml:"groupId"`
	ArtifactID  string   `xml:"artifactId"`
	Version     string   `xml:"version"`
	Name        string   `xml:"name"`
	Description string   `xml:"description"`
	URL         string   `xml:"url"`
	Licenses    []struct {
		Name         string `xml:"name"`
		URL          string `xml:"url"`
		Distribution string `xml:"distribution"`
	} `xml:"licenses>license"`
	Dependencies []struct {
		GroupID    string `xml:"groupId"`
		ArtifactID string `xml:"artifactId"`
		Version    string `xml:"version"`
		Scope      string `xml:"scope"`
	} `xml:"dependencies>dependency"`
	Parent struct {
		GroupID      string `xml:"groupId"`
		ArtifactID   string `xml:"artifactId"`
		Version      string `xml:"version"`
		RelativePath string `xml:"relativePath"`
	} `xml:"parent"`
}

// ErrNoGroupID is returned when a pom.xml declares no groupId and has
// no <parent> from which to inherit one. Without a groupId we can't
// address the artifact in the repository layout, so the upload is
// rejected.
var ErrNoGroupID = errors.New("maven: group ID is missing")

// ParsePackageMetaData decodes a pom.xml from r and returns the
// extracted metadata. The XML decoder is charset-aware (Maven POMs
// in the wild ship in UTF-8, ISO-8859-1, and the occasional Windows-1252).
//
// If <groupId> is absent the value falls back to <parent><groupId>
// per https://maven.apache.org/pom.html#Inheritance.
func ParsePackageMetaData(r io.Reader) (*Metadata, error) {
	var pom pomStruct
	dec := xml.NewDecoder(r)
	dec.CharsetReader = charset.NewReaderLabel
	if err := dec.Decode(&pom); err != nil {
		return nil, err
	}

	// Strip non-http(s) project URLs; same hygiene step alpine and
	// pypi parsers apply. The POM in the wild sometimes carries
	// "no" or "TODO" or similar free-form text here.
	if !isValidProjectURL(pom.URL) {
		pom.URL = ""
	}

	groupID := pom.GroupID
	if groupID == "" {
		if pom.Parent.GroupID == "" {
			return nil, ErrNoGroupID
		}
		groupID = pom.Parent.GroupID
	}

	licenses := make([]string, 0, len(pom.Licenses))
	for _, l := range pom.Licenses {
		if l.Name != "" {
			licenses = append(licenses, l.Name)
		}
	}

	dependencies := make([]*Dependency, 0, len(pom.Dependencies))
	for _, d := range pom.Dependencies {
		dependencies = append(dependencies, &Dependency{
			GroupID:    d.GroupID,
			ArtifactID: d.ArtifactID,
			Version:    d.Version,
		})
	}

	return &Metadata{
		GroupID:      groupID,
		ArtifactID:   pom.ArtifactID,
		Name:         pom.Name,
		Description:  pom.Description,
		ProjectURL:   pom.URL,
		Licenses:     licenses,
		Dependencies: dependencies,
	}, nil
}

func isValidProjectURL(s string) bool {
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
