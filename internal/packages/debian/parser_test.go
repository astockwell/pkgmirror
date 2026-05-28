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
	"bytes"
	"compress/gzip"
	"io"
	"testing"

	"github.com/blakesmith/ar"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ulikunitz/xz"
)

const (
	tPackageName         = "gitea"
	tPackageVersion      = "0:1.0.1-te~st"
	tPackageArchitecture = "amd64"
	tPackageAuthor       = "KN4CK3R"
	tDescription         = "Description with multiple lines."
	tProjectURL          = "https://gitea.io"
)

func TestParsePackage(t *testing.T) {
	createArchive := func(files map[string][]byte) io.Reader {
		var buf bytes.Buffer
		aw := ar.NewWriter(&buf)
		_ = aw.WriteGlobalHeader()
		for filename, content := range files {
			_ = aw.WriteHeader(&ar.Header{
				Name: filename,
				Mode: 0o600,
				Size: int64(len(content)),
			})
			_, _ = aw.Write(content)
		}
		return &buf
	}

	t.Run("MissingControlFile", func(t *testing.T) {
		data := createArchive(map[string][]byte{"dummy.txt": {}})
		p, err := ParsePackage(data)
		assert.Nil(t, p)
		require.ErrorIs(t, err, ErrMissingControlFile)
	})

	t.Run("Compression", func(t *testing.T) {
		t.Run("Unsupported", func(t *testing.T) {
			data := createArchive(map[string][]byte{"control.tar.foo": {}})
			p, err := ParsePackage(data)
			assert.Nil(t, p)
			require.ErrorIs(t, err, ErrUnsupportedCompression)
		})

		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		_ = tw.WriteHeader(&tar.Header{
			Name: "control",
			Mode: 0o600,
			Size: 50,
		})
		_, _ = tw.Write([]byte("Package: gitea\nVersion: 1.0.0\nArchitecture: amd64\n"))
		_ = tw.Close()

		cases := []struct {
			Extension     string
			WriterFactory func(io.Writer) io.WriteCloser
		}{
			{
				Extension:     "",
				WriterFactory: func(w io.Writer) io.WriteCloser { return nopCloser{w} },
			},
			{
				Extension:     ".gz",
				WriterFactory: func(w io.Writer) io.WriteCloser { return gzip.NewWriter(w) },
			},
			{
				Extension: ".xz",
				WriterFactory: func(w io.Writer) io.WriteCloser {
					xw, _ := xz.NewWriter(w)
					return xw
				},
			},
			{
				Extension: ".zst",
				WriterFactory: func(w io.Writer) io.WriteCloser {
					zw, _ := zstd.NewWriter(w)
					return zw
				},
			},
		}

		for _, c := range cases {
			t.Run(c.Extension, func(t *testing.T) {
				var cbuf bytes.Buffer
				w := c.WriterFactory(&cbuf)
				_, _ = w.Write(buf.Bytes())
				_ = w.Close()

				data := createArchive(map[string][]byte{"control.tar" + c.Extension: cbuf.Bytes()})
				p, err := ParsePackage(data)
				require.NoError(t, err)
				require.NotNil(t, p)
				assert.Equal(t, "gitea", p.Name)

				t.Run("TrailingSlash", func(t *testing.T) {
					// dpkg 1.15.6+ ar emitters may include a
					// trailing slash on the member name.
					data := createArchive(map[string][]byte{"control.tar" + c.Extension + "/": cbuf.Bytes()})
					p, err := ParsePackage(data)
					require.NoError(t, err)
					require.NotNil(t, p)
					assert.Equal(t, "gitea", p.Name)
				})
			})
		}
	})
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

func TestParseControlFile(t *testing.T) {
	buildContent := func(name, version, architecture string) *bytes.Buffer {
		var buf bytes.Buffer
		buf.WriteString("Package: " + name +
			"\nVersion: " + version +
			"\nArchitecture: " + architecture +
			"\nMaintainer: " + tPackageAuthor + " <kn4ck3r@gitea.io>" +
			"\nHomepage: " + tProjectURL +
			"\nDepends: a,\n b" +
			"\nDescription: Description\n with multiple\n lines.")
		return &buf
	}

	t.Run("InvalidName", func(t *testing.T) {
		for _, name := range []string{"", "-cd"} {
			p, err := ParseControlFile(buildContent(name, tPackageVersion, tPackageArchitecture))
			assert.Nil(t, p)
			require.ErrorIs(t, err, ErrInvalidName)
		}
	})

	t.Run("InvalidVersion", func(t *testing.T) {
		for _, version := range []string{"", "1-", ":1.0", "1_0"} {
			p, err := ParseControlFile(buildContent(tPackageName, version, tPackageArchitecture))
			assert.Nil(t, p)
			require.ErrorIs(t, err, ErrInvalidVersion)
		}
	})

	t.Run("InvalidArchitecture", func(t *testing.T) {
		p, err := ParseControlFile(buildContent(tPackageName, tPackageVersion, ""))
		assert.Nil(t, p)
		require.ErrorIs(t, err, ErrInvalidArchitecture)
	})

	t.Run("ValidVersionEpoch", func(t *testing.T) {
		// Debian versions can carry an integer epoch prefix
		// followed by a colon. Single + multi-digit epochs both
		// occur in the wild.
		for _, version := range []string{"0:1.2.3-test", "1:1.2.3-test", "9:1.2.3-test", "10:1.2.3-test", "37:1.2.3-test", "99:1.2.3-test"} {
			p, err := ParseControlFile(buildContent(tPackageName, version, tPackageArchitecture))
			require.NoError(t, err)
			require.NotNil(t, p)
		}
	})

	t.Run("Valid", func(t *testing.T) {
		content := buildContent(tPackageName, tPackageVersion, tPackageArchitecture)
		full := content.String()

		p, err := ParseControlFile(content)
		require.NoError(t, err)
		require.NotNil(t, p)

		assert.Equal(t, tPackageName, p.Name)
		assert.Equal(t, tPackageVersion, p.Version)
		assert.Equal(t, tPackageArchitecture, p.Architecture)
		assert.Equal(t, tDescription, p.Metadata.Description)
		assert.Equal(t, tProjectURL, p.Metadata.ProjectURL)
		assert.Equal(t, tPackageAuthor, p.Metadata.Maintainer)
		assert.Equal(t, []string{"a", "b"}, p.Metadata.Dependencies)
		assert.Equal(t, full, p.Control)
	})

	t.Run("InvalidHomepageDropped", func(t *testing.T) {
		// Same hygiene as alpine + maven + pypi: a non-http(s) URL
		// in <Homepage> is silently cleared rather than failing.
		var buf bytes.Buffer
		buf.WriteString("Package: foo\nVersion: 1.0\nArchitecture: amd64\nHomepage: not-a-url\n")
		p, err := ParseControlFile(&buf)
		require.NoError(t, err)
		assert.Empty(t, p.Metadata.ProjectURL)
	})
}
