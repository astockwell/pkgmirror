package npm
// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2021 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// ParsePackage and the surrounding types are a transliteration of
// forgejo/modules/packages/npm/creator.go and metadata.go from the
// Forgejo project (https://codeberg.org/forgejo/forgejo), which is
// itself MIT licensed. Behavior is preserved exactly; minor differences
// are package layout, the standard-library JSON encoder (not Forgejo's
// wrapper), and our own error sentinels.

// Package npm implements the npm registry protocol — package metadata
// (the "packument"), tarball serving, publish, and dist-tags. See
// https://github.com/npm/registry/blob/master/docs/REGISTRY-API.md.
package npm

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	semver "github.com/hashicorp/go-version"
)

// Sentinel errors returned by ParsePackage.
var (
	ErrInvalidPackage        = errors.New("npm: package is invalid")
	ErrInvalidPackageName    = errors.New("npm: package name is invalid")
	ErrInvalidPackageVersion = errors.New("npm: package version is invalid")
	ErrInvalidAttachment     = errors.New("npm: attachment is invalid")
	ErrInvalidIntegrity      = errors.New("npm: integrity validation failed")
)

// TagProperty is the per-version property key under which dist-tags are
// stored in package_properties. The value is the tag name (e.g. "latest").
// A version may carry multiple TagProperty rows.
const TagProperty = "npm.tag"

// nameMatcher implements the npm registry's name validation rules
// (https://github.com/npm/registry/blob/master/docs/REGISTRY-API.md#package-name).
var nameMatcher = regexp.MustCompile(`^(@[a-z0-9-][a-z0-9-._]*/)?[a-z0-9-][a-z0-9-._]*$`)

// Package is the parsed result of a publish payload.
type Package struct {
	// Name is the full package name including scope, e.g. "@acme/foo".
	Name string
	// Version is the canonical semver string.
	Version string
	// DistTags are dist-tags declared with the publish, e.g. ["latest"].
	DistTags []string
	// Metadata is the per-version metadata persisted to package_properties.
	Metadata Metadata
	// Filename is the tarball filename: "<unscoped-name>-<version>.tgz".
	Filename string
	// Data is the decoded tarball bytes.
	Data []byte
}

// Metadata is per-version metadata. Stored as JSON in
// package_versions.metadata_json.
type Metadata struct {
	Scope                   string            `json:"scope,omitempty"`
	Name                    string            `json:"name,omitempty"`
	Description             string            `json:"description,omitempty"`
	Author                  string            `json:"author,omitempty"`
	License                 string            `json:"license,omitempty"`
	ProjectURL              string            `json:"project_url,omitempty"`
	Keywords                []string          `json:"keywords,omitempty"`
	Dependencies            map[string]string `json:"dependencies,omitempty"`
	BundleDependencies      []string          `json:"bundleDependencies,omitempty"`
	DevelopmentDependencies map[string]string `json:"development_dependencies,omitempty"`
	PeerDependencies        map[string]string `json:"peer_dependencies,omitempty"`
	OptionalDependencies    map[string]string `json:"optional_dependencies,omitempty"`
	Bin                     map[string]string `json:"bin,omitempty"`
	Readme                  string            `json:"readme,omitempty"`
	Repository              Repository        `json:"repository"`
}

// Repository is the repo block from package.json.
type Repository struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

// User is a publish-payload "user" field. npm encodes these as either a
// bare string ("John Doe <john@example.com>") or an object.
type User struct {
	Username string `json:"username,omitempty"`
	Name     string `json:"name"`
	Email    string `json:"email,omitempty"`
	URL      string `json:"url,omitempty"`
}

// UnmarshalJSON handles both forms.
func (u *User) UnmarshalJSON(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	switch data[0] {
	case '"':
		return json.Unmarshal(data, &u.Name)
	case '{':
		var tmp struct {
			Username string `json:"username"`
			Name     string `json:"name"`
			Email    string `json:"email"`
			URL      string `json:"url"`
		}
		if err := json.Unmarshal(data, &tmp); err != nil {
			return err
		}
		u.Username, u.Name, u.Email, u.URL = tmp.Username, tmp.Name, tmp.Email, tmp.URL
		return nil
	}
	return nil
}

// --- on-the-wire types -----------------------------------------------------

type PackageDistribution struct {
	Integrity    string `json:"integrity"`
	Shasum       string `json:"shasum"`
	Tarball      string `json:"tarball"`
	FileCount    int    `json:"fileCount,omitempty"`
	UnpackedSize int    `json:"unpackedSize,omitempty"`
	NpmSignature string `json:"npm-signature,omitempty"`
}

// PackageMetadataVersion is the per-version block in a packument response.
type PackageMetadataVersion struct {
	ID                   string              `json:"_id"`
	Name                 string              `json:"name"`
	Version              string              `json:"version"`
	Description          string              `json:"description,omitempty"`
	Author               User                `json:"author"`
	Homepage             string              `json:"homepage,omitempty"`
	License              string              `json:"license,omitempty"`
	Repository           Repository          `json:"repository"`
	Keywords             []string            `json:"keywords,omitempty"`
	Dependencies         map[string]string   `json:"dependencies,omitempty"`
	BundleDependencies   []string            `json:"bundleDependencies,omitempty"`
	DevDependencies      map[string]string   `json:"devDependencies,omitempty"`
	PeerDependencies     map[string]string   `json:"peerDependencies,omitempty"`
	Bin                  map[string]string   `json:"bin,omitempty"`
	OptionalDependencies map[string]string   `json:"optionalDependencies,omitempty"`
	Readme               string              `json:"readme,omitempty"`
	Dist                 PackageDistribution `json:"dist"`
}

// PackageMetadata is the top-level packument response.
type PackageMetadata struct {
	ID          string                             `json:"_id"`
	Name        string                             `json:"name"`
	Description string                             `json:"description,omitempty"`
	DistTags    map[string]string                  `json:"dist-tags,omitempty"`
	Versions    map[string]*PackageMetadataVersion `json:"versions"`
	Readme      string                             `json:"readme,omitempty"`
	Homepage    string                             `json:"homepage,omitempty"`
	Author      User                               `json:"author"`
	Repository  Repository                         `json:"repository"`
	License     string                             `json:"license,omitempty"`
}

// PackageAttachment is the base64-wrapped tarball in a publish payload.
type PackageAttachment struct {
	ContentType string `json:"content_type"`
	Data        string `json:"data"`
	Length      int    `json:"length"`
}

type packageUpload struct {
	ID          string                             `json:"_id"`
	Name        string                             `json:"name"`
	Description string                             `json:"description,omitempty"`
	DistTags    map[string]string                  `json:"dist-tags,omitempty"`
	Versions    map[string]*PackageMetadataVersion `json:"versions"`
	Attachments map[string]*PackageAttachment      `json:"_attachments"`
}

// --- parser ----------------------------------------------------------------

// ParsePackage reads an npm publish payload (a JSON document with a single
// version embedded plus a base64-encoded tarball attachment) and returns
// the canonical Package representation.
func ParsePackage(r io.Reader) (*Package, error) {
	var upload packageUpload
	if err := json.NewDecoder(r).Decode(&upload); err != nil {
		return nil, fmt.Errorf("decode publish payload: %w", err)
	}
	if len(upload.Versions) == 0 {
		return nil, ErrInvalidPackage
	}

	for _, meta := range upload.Versions {
		if !validateName(meta.Name) {
			return nil, ErrInvalidPackageName
		}
		v, err := semver.NewSemver(meta.Version)
		if err != nil {
			return nil, ErrInvalidPackageVersion
		}

		scope, name := splitName(meta.Name)
		p := &Package{
			Name:     meta.Name,
			Version:  v.String(),
			DistTags: make([]string, 0, len(upload.DistTags)),
			Metadata: Metadata{
				Scope:                   scope,
				Name:                    name,
				Description:             meta.Description,
				Author:                  meta.Author.Name,
				License:                 meta.License,
				ProjectURL:              meta.Homepage,
				Keywords:                meta.Keywords,
				Dependencies:            meta.Dependencies,
				BundleDependencies:      meta.BundleDependencies,
				DevelopmentDependencies: meta.DevDependencies,
				PeerDependencies:        meta.PeerDependencies,
				OptionalDependencies:    meta.OptionalDependencies,
				Bin:                     meta.Bin,
				Readme:                  meta.Readme,
				Repository:              meta.Repository,
			},
		}
		for tag := range upload.DistTags {
			p.DistTags = append(p.DistTags, tag)
		}

		p.Filename = strings.ToLower(fmt.Sprintf("%s-%s.tgz", name, p.Version))

		// Pick the (typically single) attachment.
		var att *PackageAttachment
		for _, a := range upload.Attachments {
			att = a
			break
		}
		if att == nil || att.Data == "" {
			return nil, ErrInvalidAttachment
		}
		data, err := base64.StdEncoding.DecodeString(att.Data)
		if err != nil {
			return nil, ErrInvalidAttachment
		}
		p.Data = data

		// integrity is "sha512-<base64>" or "sha1-<base64>". Verify it
		// matches the attachment bytes.
		if err := verifyIntegrity(meta.Dist.Integrity, data); err != nil {
			return nil, err
		}

		return p, nil
	}
	return nil, ErrInvalidPackage
}

// validateName implements the npm registry naming rules (PEP 426-ish
// + scope syntax).
func validateName(name string) bool {
	if strings.TrimSpace(name) != name {
		return false
	}
	if len(name) == 0 || len(name) > 214 {
		return false
	}
	return nameMatcher.MatchString(name)
}

// splitName separates "@scope/name" into ("@scope", "name"). For an
// unscoped name returns ("", name).
func splitName(full string) (scope, name string) {
	parts := strings.SplitN(full, "/", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "", full
}

// verifyIntegrity parses an SRI-style "<algo>-<base64>" integrity string
// and checks it against data.
func verifyIntegrity(integrity string, data []byte) error {
	parts := strings.SplitN(integrity, "-", 2)
	if len(parts) != 2 {
		return ErrInvalidIntegrity
	}
	want, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return ErrInvalidIntegrity
	}
	var got []byte
	switch parts[0] {
	case "sha1":
		s := sha1.Sum(data)
		got = s[:]
	case "sha512":
		s := sha512.Sum512(data)
		got = s[:]
	default:
		return ErrInvalidIntegrity
	}
	if !bytes.Equal(want, got) {
		return ErrInvalidIntegrity
	}
	return nil
}

// Integrity computes "sha512-<base64>" for data. Test fixtures use this.
func Integrity(data []byte) string {
	s := sha512.Sum512(data)
	return "sha512-" + base64.StdEncoding.EncodeToString(s[:])
}
