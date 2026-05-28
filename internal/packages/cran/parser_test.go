// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2023 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// Direct port of forgejo/modules/packages/cran/metadata_test.go.
// Fixtures and test names mirror upstream so cross-references stay
// legible. Adapted to standard `testing` (Forgejo uses
// testify/assert).

package cran

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"sort"
	"testing"
)

const (
	pkgName        = "pkgmirror"
	pkgVersion     = "1.0.1"
	pkgAuthor      = "KN4CK3R"
	pkgDescription = "Package Description"
	pkgProjectURL  = "https://example.com"
	pkgLicense     = "GPL (>= 2)"
)

// createDescription writes a DESCRIPTION matching Forgejo's test
// fixture: the multi-line Description value (continuation line),
// the multi-value Imports field, NeedsCompilation: yes, etc.
func createDescription(name, version string) *bytes.Buffer {
	var buf bytes.Buffer
	fmt.Fprintln(&buf, "Package:", name)
	fmt.Fprintln(&buf, "Version:", version)
	fmt.Fprintln(&buf, "Description:", "Package\n\n  Description")
	fmt.Fprintln(&buf, "URL:", pkgProjectURL)
	fmt.Fprintln(&buf, "Imports: abc,\n123")
	fmt.Fprintln(&buf, "NeedsCompilation: yes")
	fmt.Fprintln(&buf, "License:", pkgLicense)
	fmt.Fprintln(&buf, "Author:", pkgAuthor)
	return &buf
}

func newTarGz(t *testing.T, filename string, content []byte) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{Name: filename, Mode: 0o600, Size: int64(len(content))}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	_ = tw.Close()
	_ = gw.Close()
	return bytes.NewReader(buf.Bytes())
}

func newZip(t *testing.T, filename string, content []byte) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create(filename)
	_, _ = w.Write(content)
	_ = zw.Close()
	return bytes.NewReader(buf.Bytes())
}

func TestParsePackage_TarGz_MissingDescription(t *testing.T) {
	buf := newTarGz(t, "dummy.txt", nil)
	p, err := ParsePackage(buf, buf.Size())
	if p != nil {
		t.Fatalf("expected nil package")
	}
	if !errors.Is(err, ErrMissingDescriptionFile) {
		t.Fatalf("want ErrMissingDescriptionFile, got %v", err)
	}
}

func TestParsePackage_TarGz_Valid(t *testing.T) {
	buf := newTarGz(t, "package/DESCRIPTION", createDescription(pkgName, pkgVersion).Bytes())
	p, err := ParsePackage(buf, buf.Size())
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if p.Name != pkgName || p.Version != pkgVersion {
		t.Fatalf("got %q %q", p.Name, p.Version)
	}
	if p.FileExtension != ".tar.gz" {
		t.Fatalf("ext: %q", p.FileExtension)
	}
}

func TestParsePackage_Zip_MissingDescription(t *testing.T) {
	buf := newZip(t, "dummy.txt", nil)
	p, err := ParsePackage(buf, buf.Size())
	if p != nil {
		t.Fatalf("expected nil package")
	}
	if !errors.Is(err, ErrMissingDescriptionFile) {
		t.Fatalf("want ErrMissingDescriptionFile, got %v", err)
	}
}

func TestParsePackage_Zip_Valid(t *testing.T) {
	buf := newZip(t, "package/DESCRIPTION", createDescription(pkgName, pkgVersion).Bytes())
	p, err := ParsePackage(buf, buf.Size())
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if p.Name != pkgName || p.Version != pkgVersion {
		t.Fatalf("got %q %q", p.Name, p.Version)
	}
	if p.FileExtension != ".zip" {
		t.Fatalf("ext: %q", p.FileExtension)
	}
}

func TestParseDescription_InvalidName(t *testing.T) {
	for _, name := range []string{"123abc", "ab-cd", "ab cd", "ab/cd"} {
		_, err := ParseDescription(createDescription(name, pkgVersion))
		if !errors.Is(err, ErrInvalidName) {
			t.Fatalf("name %q: want ErrInvalidName, got %v", name, err)
		}
	}
}

func TestParseDescription_InvalidVersion(t *testing.T) {
	for _, v := range []string{"1", "1 0", "1.2.3.4.5", "1-2-3-4-5", "1.", "1.0.", "1-", "1-0-"} {
		_, err := ParseDescription(createDescription(pkgName, v))
		if !errors.Is(err, ErrInvalidVersion) {
			t.Fatalf("version %q: want ErrInvalidVersion, got %v", v, err)
		}
	}
}

func TestParseDescription_Valid(t *testing.T) {
	p, err := ParseDescription(createDescription(pkgName, pkgVersion))
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if p.Name != pkgName || p.Version != pkgVersion {
		t.Fatalf("got %q %q", p.Name, p.Version)
	}
	if p.Metadata.Description != pkgDescription {
		t.Fatalf("Description: %q", p.Metadata.Description)
	}
	if !equalSets(p.Metadata.ProjectURL, []string{pkgProjectURL}) {
		t.Fatalf("URL: %v", p.Metadata.ProjectURL)
	}
	if !equalSets(p.Metadata.Authors, []string{pkgAuthor}) {
		t.Fatalf("Author: %v", p.Metadata.Authors)
	}
	if p.Metadata.License != pkgLicense {
		t.Fatalf("License: %q", p.Metadata.License)
	}
	if !equalSets(p.Metadata.Imports, []string{"abc", "123"}) {
		t.Fatalf("Imports: %v", p.Metadata.Imports)
	}
	if !p.Metadata.NeedsCompilation {
		t.Fatal("NeedsCompilation should be true")
	}
}

func equalSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]string(nil), a...)
	bb := append([]string(nil), b...)
	sort.Strings(aa)
	sort.Strings(bb)
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}
