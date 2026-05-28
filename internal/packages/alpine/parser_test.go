// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2023 The Gitea Authors.
// SPDX-License-Identifier: MIT

package alpine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	tPackageName        = "gitea"
	tPackageVersion     = "1.0.1"
	tPackageDescription = "Package Description"
	tPackageProjectURL  = "https://gitea.io"
	tPackageMaintainer  = "KN4CK3R <dummy@gitea.io>"
)

// createPKGINFOContent builds a minimally complete .PKGINFO body for
// testing. Mirrors the fixture in
// forgejo/modules/packages/alpine/metadata_test.go.
func createPKGINFOContent(name, version string) []byte {
	return []byte(`pkgname = ` + name + `
pkgver = ` + version + `
pkgdesc = ` + tPackageDescription + `
url = ` + tPackageProjectURL + `
# comment
builddate = 1678834800
packager = Gitea <pack@ag.er>
size = 123456
arch = aarch64
origin = origin
commit = 1111e709613fbc979651b09ac2bc27c6591a9999
maintainer = ` + tPackageMaintainer + `
license = MIT
depend = common
install_if = value
depend = gitea
provides = common
provides = gitea`)
}

// createPackage builds a synthetic .apk: two concatenated gzip streams,
// each containing a tar with one entry. We need this to exercise the
// multistream walker — a real .apk has at least two streams.
func createPackage(t *testing.T, name string, content []byte) io.Reader {
	t.Helper()
	names := []string{"first.stream", name}
	contents := [][]byte{{0}, content}

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)

	for i := range names {
		if i != 0 {
			require.NoError(t, zw.Close())
			zw.Reset(&buf)
		}

		tw := tar.NewWriter(zw)
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name: names[i],
			Mode: 0o600,
			Size: int64(len(contents[i])),
		}))
		_, err := tw.Write(contents[i])
		require.NoError(t, err)
		require.NoError(t, tw.Close())
	}
	require.NoError(t, zw.Close())
	return &buf
}

func TestParsePackage(t *testing.T) {
	t.Run("MissingPKGINFOFile", func(t *testing.T) {
		data := createPackage(t, "dummy.txt", []byte{})
		pp, err := ParsePackage(data)
		assert.Nil(t, pp)
		require.ErrorIs(t, err, ErrMissingPKGINFOFile)
	})

	t.Run("InvalidPKGINFOFile", func(t *testing.T) {
		data := createPackage(t, ".PKGINFO", []byte{})
		pp, err := ParsePackage(data)
		assert.Nil(t, pp)
		require.ErrorIs(t, err, ErrInvalidName)
	})

	t.Run("Valid", func(t *testing.T) {
		data := createPackage(t, ".PKGINFO", createPKGINFOContent(tPackageName, tPackageVersion))
		p, err := ParsePackage(data)
		require.NoError(t, err)
		require.NotNil(t, p)
		// Q1<base64(sha1)> over the gz bytes of the second stream.
		// Locked to the byte-equal fixture so any reader-tap
		// regression (e.g. dropping the gzip header) is caught.
		assert.Equal(t, "Q1SRYURM5+uQDqfHSwTnNIOIuuDVQ=", p.FileMetadata.Checksum)
	})
}

func TestParsePackageInfo(t *testing.T) {
	t.Run("InvalidName", func(t *testing.T) {
		p, err := ParsePackageInfo(bytes.NewReader(createPKGINFOContent("", tPackageVersion)))
		assert.Nil(t, p)
		require.ErrorIs(t, err, ErrInvalidName)
	})

	t.Run("InvalidVersion", func(t *testing.T) {
		p, err := ParsePackageInfo(bytes.NewReader(createPKGINFOContent(tPackageName, "")))
		assert.Nil(t, p)
		require.ErrorIs(t, err, ErrInvalidVersion)
	})

	t.Run("Valid", func(t *testing.T) {
		p, err := ParsePackageInfo(bytes.NewReader(createPKGINFOContent(tPackageName, tPackageVersion)))
		require.NoError(t, err)
		require.NotNil(t, p)

		assert.Equal(t, tPackageName, p.Name)
		assert.Equal(t, tPackageVersion, p.Version)
		assert.Equal(t, tPackageDescription, p.VersionMetadata.Description)
		assert.Equal(t, tPackageMaintainer, p.VersionMetadata.Maintainer)
		assert.Equal(t, tPackageProjectURL, p.VersionMetadata.ProjectURL)
		assert.Equal(t, "MIT", p.VersionMetadata.License)
		assert.Empty(t, p.FileMetadata.Checksum)
		assert.Equal(t, "Gitea <pack@ag.er>", p.FileMetadata.Packager)
		assert.EqualValues(t, 1678834800, p.FileMetadata.BuildDate)
		assert.EqualValues(t, 123456, p.FileMetadata.Size)
		assert.Equal(t, "aarch64", p.FileMetadata.Architecture)
		assert.Equal(t, "origin", p.FileMetadata.Origin)
		assert.Equal(t, "1111e709613fbc979651b09ac2bc27c6591a9999", p.FileMetadata.CommitHash)
		assert.Equal(t, "value", p.FileMetadata.InstallIf)
		assert.ElementsMatch(t, []string{"common", "gitea"}, p.FileMetadata.Provides)
		assert.ElementsMatch(t, []string{"common", "gitea"}, p.FileMetadata.Dependencies)
	})

	t.Run("RejectsNonHTTPProjectURL", func(t *testing.T) {
		// Mirrors forgejo's behavior: a non-HTTP url is silently
		// dropped rather than failing the upload. We want this
		// because PKGINFO sometimes carries `url = unknown` or
		// other free-form text from upstream tooling.
		raw := []byte("pkgname = foo\npkgver = 1.0\nurl = not-a-url\n")
		p, err := ParsePackageInfo(bytes.NewReader(raw))
		require.NoError(t, err)
		assert.Empty(t, p.VersionMetadata.ProjectURL)
	})
}
