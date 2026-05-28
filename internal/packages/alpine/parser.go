// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2023 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// The .apk on-disk format (concatenated gzipped tar streams; PKGINFO
// key=value text in an early stream; "Q1" + base64(sha1) checksum over
// the stream's gz bytes) and the per-field PKGINFO key set are modeled
// on forgejo/modules/packages/alpine/metadata.go (MIT). We keep the
// shapes byte-for-byte compatible so real `apk` clients have nothing to
// special-case about our registry.

package alpine

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"io"
	"net/url"
	"strconv"
	"strings"
)

// Errors mirror forgejo's modules/packages/alpine; callers compare with
// errors.Is to map them to 400-class HTTP responses.
var (
	ErrMissingPKGINFOFile = errors.New("PKGINFO file is missing")
	ErrInvalidName        = errors.New("package name is invalid")
	ErrInvalidVersion     = errors.New("package version is invalid")
)

// Property and pseudo-package names. We match forgejo's wire convention
// so an operator can introspect a mirror DB and recognise it.
const (
	PropertyMetadata     = "alpine.metadata"
	PropertyBranch       = "alpine.branch"
	PropertyRepository   = "alpine.repository"
	PropertyArchitecture = "alpine.architecture"

	// SettingKeyPrivate / SettingKeyPublic store the per-tenant RSA
	// keypair used to sign APKINDEX.tar.gz. We attach them as
	// properties on the synthetic RepositoryPackage row since we
	// don't have a separate tenant_settings table.
	SettingKeyPrivate = "alpine.key.private"
	SettingKeyPublic  = "alpine.key.public"

	// RepositoryPackage is the synthetic package name used to anchor
	// per-tenant alpine repo metadata (signing keys, etc.). Underscore
	// prefix mirrors forgejo's "_alpine" convention so neither system
	// will collide with a real package name.
	RepositoryPackage = "_alpine"
	RepositoryVersion = "_repository"

	// DefaultArchitecture is what we fall back to when a "noarch"
	// package is uploaded into a repo with no existing arches.
	// Matches forgejo.
	DefaultArchitecture = "x86_64"

	// ArchNoArch is the sentinel value used in PKGINFO's `arch` field
	// for arch-independent packages. We fan these out across every
	// architecture the repo already has on upload.
	ArchNoArch = "noarch"
)

// Package is the parsed shape of a single .apk upload.
type Package struct {
	Name            string
	Version         string
	VersionMetadata VersionMetadata
	FileMetadata    FileMetadata
}

// VersionMetadata is the subset of PKGINFO that's pinned to the (name,
// version) tuple — values we expect to be identical across all
// architecture builds of the same version.
type VersionMetadata struct {
	Description string `json:"description,omitempty"`
	License     string `json:"license,omitempty"`
	ProjectURL  string `json:"project_url,omitempty"`
	Maintainer  string `json:"maintainer,omitempty"`
}

// FileMetadata is the subset of PKGINFO that varies per build artifact.
// The Checksum is the "Q1<base64(sha1)>" value alpine uses everywhere;
// we compute it during parsing.
type FileMetadata struct {
	Checksum         string   `json:"checksum"`
	Packager         string   `json:"packager,omitempty"`
	BuildDate        int64    `json:"build_date,omitempty"`
	Size             int64    `json:"size,omitempty"`
	Architecture     string   `json:"architecture,omitempty"`
	Origin           string   `json:"origin,omitempty"`
	CommitHash       string   `json:"commit_hash,omitempty"`
	InstallIf        string   `json:"install_if,omitempty"`
	Provides         []string `json:"provides,omitempty"`
	Dependencies     []string `json:"dependencies,omitempty"`
	ProviderPriority int64    `json:"provider_priority,omitempty"`
}

// ParsePackage walks the .apk file (a concatenation of independent
// gzip streams) and returns the package on first sight of a .PKGINFO
// member. We compute the "Q1" checksum over the gz-bytes of whichever
// stream the .PKGINFO lives in, which is what every alpine tool — apk,
// abuild, opkg — uses as the package's content identifier.
//
// Ported from forgejo/modules/packages/alpine/metadata.go.
func ParsePackage(r io.Reader) (*Package, error) {
	// bufio is required because gzip.Reader.Multistream wants a
	// ByteReader to detect stream boundaries cleanly.
	br := bufio.NewReader(r)

	// The sha1 is rebuilt per stream — we only keep the one we end up
	// returning, so resetting at stream boundary is correct.
	h := sha1.New()

	gzr, err := gzip.NewReader(&teeByteReader{r: br, w: h})
	if err != nil {
		return nil, err
	}
	defer gzr.Close()

	for {
		// Stop after the current stream — we want one tar archive
		// per gzip stream, not the concatenated whole.
		gzr.Multistream(false)

		tr := tar.NewReader(gzr)
		for {
			hd, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}

			if hd.Name == ".PKGINFO" {
				p, err := ParsePackageInfo(tr)
				if err != nil {
					return nil, err
				}

				// Drain the rest of this tar so the underlying gzip
				// hasher sees the whole stream before we compute Sum.
				for {
					if _, err := tr.Next(); err != nil {
						break
					}
				}

				p.FileMetadata.Checksum = "Q1" + base64.StdEncoding.EncodeToString(h.Sum(nil))
				return p, nil
			}
		}

		// Next stream — fresh hasher, fresh gzip reader on top of
		// the same buffered source.
		h = sha1.New()
		err = gzr.Reset(&teeByteReader{r: br, w: h})
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}

	return nil, ErrMissingPKGINFOFile
}

// ParsePackageInfo decodes a PKGINFO text stream. Each line is
// `key = value` (with optional surrounding whitespace) and `#` lines
// are comments. Some keys (provides, depend) can repeat.
func ParsePackageInfo(r io.Reader) (*Package, error) {
	p := &Package{}

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.IndexRune(line, '=')
		if i == -1 {
			continue
		}
		key := strings.TrimSpace(line[:i])
		value := strings.TrimSpace(line[i+1:])

		switch key {
		case "pkgname":
			p.Name = value
		case "pkgver":
			p.Version = value
		case "pkgdesc":
			p.VersionMetadata.Description = value
		case "url":
			p.VersionMetadata.ProjectURL = value
		case "builddate":
			if n, err := strconv.ParseInt(value, 10, 64); err == nil {
				p.FileMetadata.BuildDate = n
			}
		case "size":
			if n, err := strconv.ParseInt(value, 10, 64); err == nil {
				p.FileMetadata.Size = n
			}
		case "arch":
			p.FileMetadata.Architecture = value
		case "origin":
			p.FileMetadata.Origin = value
		case "commit":
			p.FileMetadata.CommitHash = value
		case "maintainer":
			p.VersionMetadata.Maintainer = value
		case "packager":
			p.FileMetadata.Packager = value
		case "license":
			p.VersionMetadata.License = value
		case "install_if":
			p.FileMetadata.InstallIf = value
		case "provides":
			if value != "" {
				p.FileMetadata.Provides = append(p.FileMetadata.Provides, value)
			}
		case "depend":
			if value != "" {
				p.FileMetadata.Dependencies = append(p.FileMetadata.Dependencies, value)
			}
		case "provider_priority":
			if n, err := strconv.ParseInt(value, 10, 64); err == nil {
				p.FileMetadata.ProviderPriority = n
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if p.Name == "" {
		return nil, ErrInvalidName
	}
	if p.Version == "" {
		return nil, ErrInvalidVersion
	}
	// Strip non-http(s) URLs the same way forgejo does — they're
	// meaningless to the package index UI and bad data should not
	// fail upload.
	if !isValidProjectURL(p.VersionMetadata.ProjectURL) {
		p.VersionMetadata.ProjectURL = ""
	}
	return p, nil
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

// teeByteReader is an io.Reader that also implements io.ByteReader,
// mirroring io.TeeReader. compress/gzip uses ReadByte for the header
// path, so a plain TeeReader silently drops the gzip header bytes from
// the hash — which would break the "Q1" checksum.
type teeByteReader struct {
	r *bufio.Reader
	w io.Writer
}

func (t *teeByteReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		if _, werr := t.w.Write(p[:n]); werr != nil {
			return n, werr
		}
	}
	return n, err
}

func (t *teeByteReader) ReadByte() (byte, error) {
	b, err := t.r.ReadByte()
	if err == nil {
		if _, werr := t.w.Write([]byte{b}); werr != nil {
			return 0, werr
		}
	}
	return b, err
}
