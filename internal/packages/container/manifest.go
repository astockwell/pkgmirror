// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2022 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// The OCI manifest JSON shape is defined by the OCI Image Spec at
// https://github.com/opencontainers/image-spec/blob/main/manifest.md.
// The Manifest struct and the ParseManifest helper below are inspired by
// forgejo/modules/packages/container/metadata.go (MIT). We use only the
// subset needed for upload validation (config + layers + media types);
// the richer label-extraction Forgejo does on the image config blob is
// out of scope for this MVP and left for a follow-up.

package container

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Media types accepted on /v2/<name>/manifests/<ref> uploads. The list is
// the union of the OCI Image Spec and the legacy Docker Image Manifest v2.
const (
	MediaTypeOCIManifest      = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeOCIIndex         = "application/vnd.oci.image.index.v1+json"
	MediaTypeDockerManifest   = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDockerManifestV1 = "application/vnd.docker.distribution.manifest.v1+json"
	MediaTypeDockerList       = "application/vnd.docker.distribution.manifest.list.v2+json"
)

// IsManifestMediaType reports whether mt is a recognized manifest media type.
func IsManifestMediaType(mt string) bool {
	switch mt {
	case MediaTypeOCIManifest,
		MediaTypeOCIIndex,
		MediaTypeDockerManifest,
		MediaTypeDockerManifestV1,
		MediaTypeDockerList:
		return true
	}
	return false
}

// Descriptor matches the OCI spec's descriptor object: a (mediaType, digest,
// size) tuple plus optional fields we don't care about.
type Descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// Manifest is the subset of the OCI / Docker v2 manifest we look at. The
// MVP only validates that all referenced blob digests are present in our
// store; the rest of the manifest is passed through as the response body.
type Manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType,omitempty"`
	Config        Descriptor   `json:"config"`
	Layers        []Descriptor `json:"layers"`
	// Manifests is set when this document is itself an index (the
	// fat-manifest case) — each entry points at a platform-specific
	// child manifest.
	Manifests []Descriptor `json:"manifests,omitempty"`
}

// ErrInvalidManifest is returned when the body isn't valid JSON or doesn't
// look like an OCI manifest.
var ErrInvalidManifest = errors.New("container: invalid manifest")

// ParseManifest decodes r as JSON and returns the parsed manifest.
// Caller is responsible for validating media-type compatibility against
// the Content-Type header (we don't reject mismatches because some
// clients send slightly off content-types — mediaType inside the JSON
// is authoritative for OCI).
func ParseManifest(r io.Reader) (*Manifest, error) {
	var m Manifest
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	if m.SchemaVersion == 0 {
		return nil, fmt.Errorf("%w: missing schemaVersion", ErrInvalidManifest)
	}
	return &m, nil
}

// ReferencedDigests returns every blob digest referenced by the manifest:
// the config + each layer + each child manifest (for image indexes).
// Used by the upload handler to verify all blobs are present before
// committing the manifest.
func (m *Manifest) ReferencedDigests() []string {
	out := make([]string, 0, 1+len(m.Layers)+len(m.Manifests))
	if m.Config.Digest != "" {
		out = append(out, m.Config.Digest)
	}
	for _, l := range m.Layers {
		if l.Digest != "" {
			out = append(out, l.Digest)
		}
	}
	for _, ch := range m.Manifests {
		if ch.Digest != "" {
			out = append(out, ch.Digest)
		}
	}
	return out
}

// IsIndex reports whether this is a multi-platform index manifest
// rather than a single image manifest. Index manifests reference other
// manifests; image manifests reference config + layers.
func (m *Manifest) IsIndex() bool {
	return len(m.Manifests) > 0
}

// ValidateDigest reports whether s parses as a "sha256:<hex>" or
// "sha512:<hex>" digest string. We accept sha256 (the universal case)
// and sha512 (occasionally used). All other algorithms are rejected.
func ValidateDigest(s string) bool {
	for _, prefix := range []string{"sha256:", "sha512:"} {
		if strings.HasPrefix(s, prefix) {
			rest := s[len(prefix):]
			if rest == "" {
				return false
			}
			for _, c := range rest {
				if !isHexDigit(c) {
					return false
				}
			}
			return true
		}
	}
	return false
}

// DigestSHA256 returns just the hex portion of a "sha256:..." digest,
// or "" if s isn't a sha256 digest. Used to bridge between OCI digest
// strings and our blob table's hash_sha256 column.
func DigestSHA256(s string) string {
	if strings.HasPrefix(s, "sha256:") {
		return s[len("sha256:"):]
	}
	return ""
}

// HexToDigest is the inverse of DigestSHA256.
func HexToDigest(hex string) string { return "sha256:" + hex }

func isHexDigit(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}
