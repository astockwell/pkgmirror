// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2023 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// This file is a transliteration of
// forgejo/modules/packages/cran/metadata.go from the Forgejo project
// (https://codeberg.org/forgejo/forgejo), which is itself MIT
// licensed. The DESCRIPTION-file parser (RFC 822-ish with
// leading-whitespace continuation), the .tar.gz / .zip dispatch by
// magic bytes, and the regex-based name + version validation
// preserve the upstream algorithm. Differences are limited to:
//
//   - we drop `util.NewInvalidArgumentErrorf` and use plain
//     errors.New; the handler maps via errors.Is on the sentinels
//     defined here;
//   - we surface the License field on the parsed metadata
//     specifically so the handler can hand it to
//     `models.SetLicense` and the policy engine, matching the
//     pattern in Maven / Alpine / PyPI / npm.

package cran

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"errors"
	"io"
	"path"
	"regexp"
	"strings"
)

// Property keys for the package_properties table. Stored per file
// (PropertyType) and per binary file (Platform + RVersion).
const (
	PropertyType     = "cran.type"
	PropertyPlatform = "cran.platform"
	PropertyRVersion = "cran.rvserion" // matches Forgejo's typo for parity with their schema

	TypeSource = "source"
	TypeBinary = "binary"
)

// Sentinels — handlers map via errors.Is.
var (
	ErrMissingDescriptionFile = errors.New("DESCRIPTION file is missing")
	ErrInvalidName            = errors.New("package name is invalid")
	ErrInvalidVersion         = errors.New("package version is invalid")
)

var (
	fieldPattern         = regexp.MustCompile(`\A\S+:`)
	namePattern          = regexp.MustCompile(`\A[a-zA-Z][a-zA-Z0-9\.]*[a-zA-Z0-9]\z`)
	versionPattern       = regexp.MustCompile(`\A[0-9]+(?:[.\-][0-9]+){1,3}\z`)
	authorReplacePattern = regexp.MustCompile(`[\[\(].+?[\]\)]`)
)

// Package is the result of parsing a CRAN .tar.gz / .zip.
type Package struct {
	Name          string
	Version       string
	FileExtension string // ".tar.gz" for source, ".zip" for binary (Windows)
	Metadata      *Metadata
}

// Metadata captures the DESCRIPTION fields downstream consumers
// (the PACKAGES index builder + the policy engine) care about.
// Same field shapes as Forgejo's `cran_module.Metadata`.
type Metadata struct {
	Title            string   `json:"title,omitempty"`
	Description      string   `json:"description,omitempty"`
	ProjectURL       []string `json:"project_url,omitempty"`
	License          string   `json:"license,omitempty"`
	Authors          []string `json:"authors,omitempty"`
	Depends          []string `json:"depends,omitempty"`
	Imports          []string `json:"imports,omitempty"`
	Suggests         []string `json:"suggests,omitempty"`
	LinkingTo        []string `json:"linking_to,omitempty"`
	NeedsCompilation bool     `json:"needs_compilation"`
}

// ReaderReaderAt is the input contract for ParsePackage — the body
// is seek-able (for zip) and stream-readable (for gzip). pkgmirror's
// HashedBuffer satisfies it.
type ReaderReaderAt interface {
	io.Reader
	io.ReaderAt
}

// ParsePackage sniffs the first two bytes to dispatch between
// gzipped tar (CRAN source packages, .tar.gz) and zip (CRAN Windows
// binary packages, .zip). macOS binary packages are also .tgz —
// covered by the gzip path.
func ParsePackage(r ReaderReaderAt, size int64) (*Package, error) {
	magic := make([]byte, 2)
	if _, err := r.ReadAt(magic, 0); err != nil {
		return nil, err
	}
	if magic[0] == 0x1F && magic[1] == 0x8B {
		return parsePackageTarGz(r)
	}
	return parsePackageZip(r, size)
}

func parsePackageTarGz(r io.Reader) (*Package, error) {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		hd, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hd.Typeflag != tar.TypeReg {
			continue
		}
		// CRAN packages put DESCRIPTION at `<name>/DESCRIPTION` (one
		// level deep). Skip anything nested deeper (R/ subdirs,
		// inst/extdata, etc.).
		if strings.Count(hd.Name, "/") > 1 {
			continue
		}
		if path.Base(hd.Name) == "DESCRIPTION" {
			p, err := ParseDescription(tr)
			if p != nil {
				p.FileExtension = ".tar.gz"
			}
			return p, err
		}
	}
	return nil, ErrMissingDescriptionFile
}

func parsePackageZip(r io.ReaderAt, size int64) (*Package, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, err
	}
	for _, file := range zr.File {
		if strings.Count(file.Name, "/") > 1 {
			continue
		}
		if path.Base(file.Name) == "DESCRIPTION" {
			f, err := zr.Open(file.Name)
			if err != nil {
				return nil, err
			}
			defer f.Close()
			p, err := ParseDescription(f)
			if p != nil {
				p.FileExtension = ".zip"
			}
			return p, err
		}
	}
	return nil, ErrMissingDescriptionFile
}

// ParseDescription consumes one DESCRIPTION file. Lines starting
// with whitespace continue the previous field (RFC 822-ish), so we
// accumulate into a builder and only flush to setField on the next
// field marker (`<word>:`) or EOF.
func ParseDescription(r io.Reader) (*Package, error) {
	p := &Package{Metadata: &Metadata{}}
	scanner := bufio.NewScanner(r)
	// DESCRIPTION fields can be wide (long URLs, long descriptions);
	// bump the scanner's max token size to 1 MiB to be safe.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var b strings.Builder
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !fieldPattern.MatchString(line) {
			// Continuation of the previous field.
			b.WriteRune(' ')
			b.WriteString(line)
			continue
		}
		if err := setField(p, b.String()); err != nil {
			return nil, err
		}
		b.Reset()
		b.WriteString(line)
	}
	if err := setField(p, b.String()); err != nil {
		return nil, err
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return p, nil
}

func setField(p *Package, data string) error {
	if data == "" {
		return nil
	}
	parts := strings.SplitN(data, ":", 2)
	if len(parts) != 2 {
		return nil
	}
	name := strings.TrimSpace(parts[0])
	value := strings.TrimSpace(parts[1])

	switch name {
	case "Package":
		if !namePattern.MatchString(value) {
			return ErrInvalidName
		}
		p.Name = value
	case "Version":
		if !versionPattern.MatchString(value) {
			return ErrInvalidVersion
		}
		p.Version = value
	case "Title":
		p.Metadata.Title = value
	case "Description":
		p.Metadata.Description = value
	case "URL":
		p.Metadata.ProjectURL = splitAndTrim(value)
	case "License":
		p.Metadata.License = value
	case "Author":
		// CRAN Author lines often embed email + role markers in
		// `[<role>] (<email>)` style — strip them so the names are
		// usable as-is.
		p.Metadata.Authors = splitAndTrim(authorReplacePattern.ReplaceAllString(value, ""))
	case "Depends":
		p.Metadata.Depends = splitAndTrim(value)
	case "Imports":
		p.Metadata.Imports = splitAndTrim(value)
	case "Suggests":
		p.Metadata.Suggests = splitAndTrim(value)
	case "LinkingTo":
		p.Metadata.LinkingTo = splitAndTrim(value)
	case "NeedsCompilation":
		p.Metadata.NeedsCompilation = value == "yes"
	}
	return nil
}

func splitAndTrim(s string) []string {
	items := strings.Split(s, ", ")
	for i := range items {
		items[i] = strings.TrimSpace(items[i])
	}
	return items
}
