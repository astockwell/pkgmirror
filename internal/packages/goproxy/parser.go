// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2023 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// This file is a transliteration of forgejo/modules/packages/goproxy/metadata.go
// from the Forgejo project (https://codeberg.org/forgejo/forgejo), which is
// itself MIT licensed. The Parse function below preserves the upstream
// algorithm; differences are limited to error types and minor stylistic
// cleanup.

// Package goproxy implements parsing and HTTP handlers for the Go module
// proxy protocol (https://go.dev/ref/mod#goproxy-protocol) and Go module
// zip format (https://go.dev/ref/mod#zip-files).
package goproxy

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// PropertyGoMod is the property key under which we store the go.mod contents
// for a version.
const PropertyGoMod = "go.mod"

// maxGoModFileSize matches the limit documented at
// https://go.dev/ref/mod#zip-path-size-constraints.
const maxGoModFileSize = 16 * 1024 * 1024

// ErrInvalidStructure is returned when the uploaded zip does not match the
// Go module zip layout (no <module>@<version>/ root directory).
var ErrInvalidStructure = errors.New("goproxy: package has invalid structure")

// ErrGoModFileTooLarge is returned when a go.mod inside the zip exceeds the
// 16 MiB limit.
var ErrGoModFileTooLarge = errors.New("goproxy: go.mod file is too large")

// Package is the parsed metadata for a Go module zip.
type Package struct {
	// Name is the module path, e.g. "example.com/foo".
	Name string
	// Version is the SemVer-ish version string, e.g. "v1.0.0".
	Version string
	// GoMod is the contents of the go.mod found at the module root, or a
	// synthesized one-line module declaration if no go.mod was present.
	GoMod string
}

// Parse reads a Go module zip from r and extracts its name, version, and
// go.mod contents.
func Parse(r io.ReaderAt, size int64) (*Package, error) {
	archive, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("read zip: %w", err)
	}

	var p *Package

	for _, file := range archive.File {
		nameAndVersion := path.Dir(file.Name)

		parts := strings.SplitN(nameAndVersion, "@", 2)
		if len(parts) != 2 {
			continue
		}

		versionParts := strings.SplitN(parts[1], "/", 2)

		if p == nil {
			p = &Package{
				Name:    strings.TrimSuffix(nameAndVersion, "@"+parts[1]),
				Version: versionParts[0],
			}
		}

		if len(versionParts) > 1 {
			// File lives under a subdirectory of the module root — not the
			// root-level go.mod we are looking for.
			continue
		}

		if path.Base(file.Name) == "go.mod" {
			if file.UncompressedSize64 > maxGoModFileSize {
				return nil, ErrGoModFileTooLarge
			}
			f, err := archive.Open(file.Name)
			if err != nil {
				return nil, fmt.Errorf("open go.mod: %w", err)
			}
			bytes, err := io.ReadAll(&io.LimitedReader{R: f, N: maxGoModFileSize})
			_ = f.Close()
			if err != nil {
				return nil, fmt.Errorf("read go.mod: %w", err)
			}
			p.GoMod = string(bytes)
			return p, nil
		}
	}

	if p == nil {
		return nil, ErrInvalidStructure
	}

	p.GoMod = fmt.Sprintf("module %s", p.Name)
	return p, nil
}
