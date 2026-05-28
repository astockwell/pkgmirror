// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2023 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// Direct transliteration of forgejo/modules/packages/debian/metadata.go
// (MIT). Differences from upstream:
//
//   - errors via stdlib errors.New + sentinel vars (no
//     forgejo.org/modules/util)
//   - url validation via stdlib net/url (no
//     forgejo.org/modules/validation)
//   - zstd via github.com/klauspost/compress/zstd (no
//     forgejo.org/modules/zstd)
//
// The on-disk .deb format (ar archive containing debian-binary,
// control.tar[.gz|.xz|.zst], data.tar.*), the control-file scanner
// rules (RFC 822-ish with field continuation by leading whitespace),
// and the maintainer-address heuristic are preserved verbatim so real
// `apt` clients have nothing to special-case about our registry.

package debian

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"io"
	"net/mail"
	"net/url"
	"regexp"
	"strings"

	"github.com/blakesmith/ar"
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// Property and pseudo-package names. We match forgejo's wire
// convention so an operator inspecting a mirror DB recognizes it.
const (
	PropertyDistribution = "debian.distribution"
	PropertyComponent    = "debian.component"
	PropertyArchitecture = "debian.architecture"
	PropertyControl      = "debian.control"

	// SettingKeyPrivate / SettingKeyPublic are properties on the
	// synthetic _debian repository package row; we don't have a
	// separate user_settings table.
	SettingKeyPrivate = "debian.key.private"
	SettingKeyPublic  = "debian.key.public"

	// RepositoryPackage is the synthetic package name used to anchor
	// per-tenant Debian repo metadata (GPG signing keys). Matches
	// forgejo's "_debian" convention.
	RepositoryPackage = "_debian"
	RepositoryVersion = "_repository"

	controlTar = "control.tar"
)

// Errors returned by the parser. Callers compare with errors.Is to
// map them to 400-class responses.
var (
	ErrMissingControlFile     = errors.New("debian: control file is missing")
	ErrUnsupportedCompression = errors.New("debian: unsupported compression algorithm")
	ErrInvalidName            = errors.New("debian: package name is invalid")
	ErrInvalidVersion         = errors.New("debian: package version is invalid")
	ErrInvalidArchitecture    = errors.New("debian: package architecture is invalid")
)

var (
	// https://www.debian.org/doc/debian-policy/ch-controlfields.html#source
	namePattern = regexp.MustCompile(`\A[a-z0-9][a-z0-9+\-.]+\z`)
	// https://www.debian.org/doc/debian-policy/ch-controlfields.html#version
	versionPattern = regexp.MustCompile(`\A(?:[1-9]?[0-9]:)?[a-zA-Z0-9.+~]+(?:-[a-zA-Z0-9.+\-~]+)?\z`)
)

// Package is the parsed shape of a single .deb upload.
type Package struct {
	Name         string
	Version      string
	Architecture string
	Control      string
	Metadata     *Metadata
}

// Metadata holds the subset of control fields we expose to the policy
// engine + the UI.
type Metadata struct {
	Maintainer   string   `json:"maintainer,omitempty"`
	ProjectURL   string   `json:"project_url,omitempty"`
	Description  string   `json:"description,omitempty"`
	Dependencies []string `json:"dependencies,omitempty"`
}

// ParsePackage decodes a .deb file. The .deb format is an `ar`
// archive containing at minimum:
//   - debian-binary       (version line, e.g. "2.0\n")
//   - control.tar[.<comp>] (tarball with control + maintainer scripts)
//   - data.tar.<comp>     (the actual files to install)
//
// We need only the control.tar's `control` file to extract the
// metadata; data.tar is opaque to us.
//
// Ported from forgejo/modules/packages/debian/metadata.go.
func ParsePackage(r io.Reader) (*Package, error) {
	arr := ar.NewReader(r)
	for {
		hd, err := arr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		if !strings.HasPrefix(hd.Name, controlTar) {
			continue
		}

		// dpkg 1.15.6+ may emit a trailing slash on the ar
		// member name. https://man7.org/linux/man-pages/man5/deb-split.5.html#FORMAT
		ext := strings.TrimSuffix(hd.Name[len(controlTar):], "/")

		var inner io.Reader
		switch ext {
		case "":
			inner = arr
		case ".gz":
			gzr, err := gzip.NewReader(arr)
			if err != nil {
				return nil, err
			}
			defer gzr.Close()
			inner = gzr
		case ".xz":
			xzr, err := xz.NewReader(arr)
			if err != nil {
				return nil, err
			}
			inner = xzr
		case ".zst":
			zr, err := zstd.NewReader(arr)
			if err != nil {
				return nil, err
			}
			defer zr.Close()
			inner = zr
		default:
			return nil, ErrUnsupportedCompression
		}

		tr := tar.NewReader(inner)
		for {
			thd, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			if thd.Typeflag != tar.TypeReg {
				continue
			}
			if thd.FileInfo().Name() == "control" {
				return ParseControlFile(tr)
			}
		}
	}
	return nil, ErrMissingControlFile
}

// ParseControlFile parses a Debian control file (a paragraph of
// `Key: value` lines with continuation by leading whitespace) and
// returns the metadata. Keys we recognize: Package, Version,
// Architecture, Maintainer, Description, Depends, Homepage.
//
// The full control text is also captured (TeeReader) and stored on
// the returned Package — the Packages index serves it back to apt
// clients verbatim.
//
// Ported from forgejo/modules/packages/debian/metadata.go.
func ParseControlFile(r io.Reader) (*Package, error) {
	p := &Package{Metadata: &Metadata{}}

	var key string
	var depends strings.Builder
	var control strings.Builder

	s := bufio.NewScanner(io.TeeReader(r, &control))
	for s.Scan() {
		line := s.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// Continuation lines start with space or tab and apply to
		// the previous key. The two we care about preserving across
		// continuations are Description (multi-line) and Depends
		// (comma-separated, sometimes wrapped).
		if line[0] == ' ' || line[0] == '\t' {
			switch key {
			case "Description":
				p.Metadata.Description += line
			case "Depends":
				depends.WriteString(trimmed)
			}
			continue
		}

		parts := strings.SplitN(trimmed, ":", 2)
		if len(parts) < 2 {
			continue
		}
		key = parts[0]
		value := strings.TrimSpace(parts[1])

		switch key {
		case "Package":
			p.Name = value
		case "Version":
			p.Version = value
		case "Architecture":
			p.Architecture = value
		case "Maintainer":
			// Maintainer typically reads `Name <email>`. Strip the
			// email for the cataloging metadata; if mail.ParseAddress
			// fails or returns no name we keep the raw string.
			a, err := mail.ParseAddress(value)
			if err != nil || a.Name == "" {
				p.Metadata.Maintainer = value
			} else {
				p.Metadata.Maintainer = a.Name
			}
		case "Description":
			p.Metadata.Description = value
		case "Depends":
			depends.WriteString(value)
		case "Homepage":
			if isValidProjectURL(value) {
				p.Metadata.ProjectURL = value
			}
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}

	if !namePattern.MatchString(p.Name) {
		return nil, ErrInvalidName
	}
	if !versionPattern.MatchString(p.Version) {
		return nil, ErrInvalidVersion
	}
	if p.Architecture == "" {
		return nil, ErrInvalidArchitecture
	}

	deps := strings.Split(depends.String(), ",")
	for i := range deps {
		deps[i] = strings.TrimSpace(deps[i])
	}
	p.Metadata.Dependencies = deps

	p.Control = strings.TrimSpace(control.String())
	return p, nil
}

// isValidProjectURL keeps the same hygiene as the alpine + maven
// parsers: drop a non-http(s) URL rather than fail the upload. POMs
// and control files in the wild carry "TODO" or bare hostnames.
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
