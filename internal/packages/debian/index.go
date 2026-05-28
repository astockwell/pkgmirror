// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2023 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// Index and signing-key builders for the Debian repository format.
// Modeled on forgejo/services/packages/debian/repository.go (MIT). The
// `Packages` / `Packages.gz` / `Packages.xz` text format, the
// `Release` paragraph layout, the clearsigned `InRelease` /
// detached-signed `Release.gpg` pair, and the per-tenant
// OpenPGP keypair generation are all preserved byte-shape-compatible
// with forgejo (and therefore byte-shape-compatible with the
// Debian Repository Format that real `apt` expects).
//
// Differences from upstream:
//
//   - We build indices on demand per request, rather than persisting
//     them as package_file rows on a synthetic `_debian` package.
//     See docs/adding-a-format.md "On-demand vs cached index
//     generation".
//   - Per-tenant key material is stored as properties on a synthetic
//     `_debian` package row (matches our Alpine pattern) rather than
//     in a `user_setting` table.

package debian

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/astockwell/pkgmirror/internal/models"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/ulikunitz/xz"
)

// Origin is the repository identity stamped into Release files. apt
// shows this to users in some failure messages and `apt-key`-style
// tooling but doesn't otherwise depend on the value. We use a fixed
// string rather than the tenant name because operators may rename
// tenants; the Origin can't change without breaking apt's repo
// fingerprint cache.
const Origin = "pkgmirror"

// IndexEntry is a single artifact row materialized into the Packages
// index. The caller (handler.go) loads these from the DB and hands
// them to BuildPackagesIndices.
type IndexEntry struct {
	Pkg     *models.Package
	Ver     *models.Version
	Blob    *models.Blob
	File    *models.File
	Control string // verbatim debian.control property
}

// PackagesIndices holds the three encoded forms of the Packages index
// (plain, gzip, xz) that apt may request and the SHA hashes apt
// expects to find in Release. All three are computed in lockstep
// from the same source bytes.
type PackagesIndices struct {
	Plain     []byte
	Gzip      []byte
	Xz        []byte
	PlainHash IndexHashes
	GzipHash  IndexHashes
	XzHash    IndexHashes
}

// IndexHashes is the size + four checksums apt's Release file
// references for every index file. Computed by hashAll.
type IndexHashes struct {
	Size                          int64
	MD5, SHA1, SHA256, SHA512Hash string
}

// BuildPackagesIndices serializes entries into the Debian on-disk
// Packages text format and produces gzip + xz encodings of the same
// bytes plus their per-format hashes (for inclusion in Release).
//
// The text layout — verbatim control paragraph, followed by
// Filename:, Size:, MD5sum:, SHA1:, SHA256:, SHA512: — matches
// forgejo and the Debian Repository Format reference. apt parses
// it directly with no tolerance for missing fields.
func BuildPackagesIndices(distribution, component string, entries []*IndexEntry) (PackagesIndices, error) {
	var plain bytes.Buffer
	for i, e := range entries {
		if i > 0 {
			fmt.Fprintln(&plain)
		}
		// Trim trailing whitespace on the control block — some
		// .deb authors leave a final \n that would create a blank
		// line and confuse apt's paragraph splitter.
		fmt.Fprintf(&plain, "%s\n", strings.TrimSpace(e.Control))
		fmt.Fprintf(&plain, "Filename: pool/%s/%s/%s\n", distribution, component, e.File.Name)
		fmt.Fprintf(&plain, "Size: %d\n", e.Blob.Size)
		fmt.Fprintf(&plain, "MD5sum: %s\n", e.Blob.HashMD5)
		fmt.Fprintf(&plain, "SHA1: %s\n", e.Blob.HashSHA1)
		fmt.Fprintf(&plain, "SHA256: %s\n", e.Blob.HashSHA256)
		fmt.Fprintf(&plain, "SHA512: %s\n", e.Blob.HashSHA512)
	}

	var gzbuf bytes.Buffer
	gzw := gzip.NewWriter(&gzbuf)
	if _, err := gzw.Write(plain.Bytes()); err != nil {
		return PackagesIndices{}, err
	}
	if err := gzw.Close(); err != nil {
		return PackagesIndices{}, err
	}

	var xzbuf bytes.Buffer
	xzw, err := xz.NewWriter(&xzbuf)
	if err != nil {
		return PackagesIndices{}, err
	}
	if _, err := xzw.Write(plain.Bytes()); err != nil {
		return PackagesIndices{}, err
	}
	if err := xzw.Close(); err != nil {
		return PackagesIndices{}, err
	}

	return PackagesIndices{
		Plain:     plain.Bytes(),
		Gzip:      gzbuf.Bytes(),
		Xz:        xzbuf.Bytes(),
		PlainHash: hashAll(plain.Bytes()),
		GzipHash:  hashAll(gzbuf.Bytes()),
		XzHash:    hashAll(xzbuf.Bytes()),
	}, nil
}

// PackagesPathInRelease is the relative path apt looks for in
// Release's MD5Sum/SHA*/etc sections. Matches the standard Debian
// layout.
func PackagesPathInRelease(component, architecture, filename string) string {
	return fmt.Sprintf("%s/binary-%s/%s", component, architecture, filename)
}

// ReleaseFiles holds the Release text plus the clearsigned InRelease
// and detached Release.gpg signature variants.
type ReleaseFiles struct {
	Release   []byte
	InRelease []byte
	GPG       []byte
}

// PerArchPackagesIndex bundles a Packages index with the
// (component, architecture) coordinate it covers — used to populate
// the per-arch entries inside Release's hash sections.
type PerArchPackagesIndex struct {
	Component    string
	Architecture string
	Indices      PackagesIndices
}

// BuildReleaseFiles produces Release + InRelease + Release.gpg. The
// hash entries cover every (component, arch) Packages variant in
// `indices`. We sign with `privPEM` (the per-tenant armored OpenPGP
// private key) — apt validates against the matching public key the
// operator has dropped in /etc/apt/keyrings/.
//
// The Release `Date:` line is derived from `releaseDate` (which the
// caller computes from `max(file.created_unix)` across the
// distribution) rather than `time.Now()`. This is load-bearing: GET
// /Release and GET /Release.gpg are served by independent requests
// that each rebuild the Release bytes from scratch. If Date varied
// between the two, the detached signature would not validate against
// the live Release body. Deriving Date from the data state keeps the
// two requests' bytes identical without any caching coordination.
//
// Acquire-By-Hash is advertised but the by-hash routes themselves
// are NOT implemented yet — apt falls back to direct path lookups
// on the absence of those routes. Adding by-hash later is purely
// additive.
func BuildReleaseFiles(distribution string, components, architectures []string, indices []PerArchPackagesIndex, privPEM string, releaseDate time.Time) (*ReleaseFiles, error) {
	entity, err := parseArmoredEntity(privPEM)
	if err != nil {
		return nil, err
	}

	sort.Strings(components)
	sort.Strings(architectures)

	// Plain Release first — we'll then clearsign and detach-sign it.
	var release bytes.Buffer
	fmt.Fprintf(&release, "Origin: %s\n", Origin)
	fmt.Fprintf(&release, "Label: %s\n", Origin)
	fmt.Fprintf(&release, "Suite: %s\n", distribution)
	fmt.Fprintf(&release, "Codename: %s\n", distribution)
	fmt.Fprintf(&release, "Components: %s\n", strings.Join(components, " "))
	fmt.Fprintf(&release, "Architectures: %s\n", strings.Join(architectures, " "))
	fmt.Fprintf(&release, "Date: %s\n", releaseDate.UTC().Format(time.RFC1123))
	fmt.Fprintf(&release, "Acquire-By-Hash: yes\n")

	writeHashSection(&release, "MD5Sum", indices, func(h IndexHashes) string { return h.MD5 })
	writeHashSection(&release, "SHA1", indices, func(h IndexHashes) string { return h.SHA1 })
	writeHashSection(&release, "SHA256", indices, func(h IndexHashes) string { return h.SHA256 })
	writeHashSection(&release, "SHA512", indices, func(h IndexHashes) string { return h.SHA512Hash })

	// Detached signature → Release.gpg
	var detached bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&detached, entity, bytes.NewReader(release.Bytes()), nil); err != nil {
		return nil, fmt.Errorf("detached sign Release: %w", err)
	}

	// Clearsigned inline → InRelease
	var inRelease bytes.Buffer
	sw, err := clearsign.Encode(&inRelease, entity.PrivateKey, nil)
	if err != nil {
		return nil, fmt.Errorf("clearsign InRelease: %w", err)
	}
	if _, err := sw.Write(release.Bytes()); err != nil {
		return nil, err
	}
	if err := sw.Close(); err != nil {
		return nil, err
	}

	return &ReleaseFiles{
		Release:   release.Bytes(),
		InRelease: inRelease.Bytes(),
		GPG:       detached.Bytes(),
	}, nil
}

func writeHashSection(w io.Writer, label string, indices []PerArchPackagesIndex, pick func(IndexHashes) string) {
	fmt.Fprintf(w, "%s:\n", label)
	// Deterministic ordering across (component, arch, filename).
	rows := make([]struct{ Hash, Path string; Size int64 }, 0, len(indices)*3)
	for _, idx := range indices {
		for _, f := range []struct {
			name string
			h    IndexHashes
		}{
			{"Packages", idx.Indices.PlainHash},
			{"Packages.gz", idx.Indices.GzipHash},
			{"Packages.xz", idx.Indices.XzHash},
		} {
			rows = append(rows, struct{ Hash, Path string; Size int64 }{
				Hash: pick(f.h),
				Path: PackagesPathInRelease(idx.Component, idx.Architecture, f.name),
				Size: f.h.Size,
			})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })
	for _, r := range rows {
		fmt.Fprintf(w, " %s %d %s\n", r.Hash, r.Size, r.Path)
	}
}

// --- GPG key management ----------------------------------------------------

// GenerateKeyPair builds a fresh OpenPGP keypair encoded as armored
// PEM-ish strings, matching what forgejo emits. apt validates
// against the armored public key dropped in /etc/apt/keyrings/.
func GenerateKeyPair() (privatePEM, publicPEM string, err error) {
	// Name + comment + email mirror forgejo's call shape.
	e, err := openpgp.NewEntity("", "Debian Registry", "", nil)
	if err != nil {
		return "", "", err
	}

	var priv, pub strings.Builder
	pw, err := armor.Encode(&priv, openpgp.PrivateKeyType, nil)
	if err != nil {
		return "", "", err
	}
	if err := e.SerializePrivate(pw, nil); err != nil {
		return "", "", err
	}
	_ = pw.Close()

	uw, err := armor.Encode(&pub, openpgp.PublicKeyType, nil)
	if err != nil {
		return "", "", err
	}
	if err := e.Serialize(uw); err != nil {
		return "", "", err
	}
	_ = uw.Close()

	return priv.String(), pub.String(), nil
}

// GetOrCreateKeyPair returns the per-tenant OpenPGP keypair as armored
// strings, generating one on first call. Keys are stored as properties
// on a synthetic `_debian` package row scoped to the tenant — the
// same pattern Alpine uses. Concurrent first-time requests are
// serialized externally by the handler (via ExclusivePool); this
// function does not lock.
func GetOrCreateKeyPair(ctx context.Context, m *models.Store, tenantID int64) (privatePEM, publicPEM string, err error) {
	pkg, err := m.GetOrCreatePackage(ctx, tenantID, models.TypeDebian, RepositoryPackage)
	if err != nil {
		return "", "", err
	}

	priv, hasPriv, err := m.GetProperty(ctx, models.PropertyRefPackage, pkg.ID, SettingKeyPrivate)
	if err != nil {
		return "", "", err
	}
	pub, hasPub, err := m.GetProperty(ctx, models.PropertyRefPackage, pkg.ID, SettingKeyPublic)
	if err != nil {
		return "", "", err
	}
	if hasPriv && hasPub && priv != "" && pub != "" {
		return priv, pub, nil
	}

	priv, pub, err = GenerateKeyPair()
	if err != nil {
		return "", "", err
	}
	if err := m.SetProperty(ctx, models.PropertyRefPackage, pkg.ID, SettingKeyPrivate, priv); err != nil {
		return "", "", err
	}
	if err := m.SetProperty(ctx, models.PropertyRefPackage, pkg.ID, SettingKeyPublic, pub); err != nil {
		return "", "", err
	}
	return priv, pub, nil
}

func parseArmoredEntity(privPEM string) (*openpgp.Entity, error) {
	block, err := armor.Decode(strings.NewReader(privPEM))
	if err != nil {
		return nil, fmt.Errorf("decode armored private key: %w", err)
	}
	if block.Type != openpgp.PrivateKeyType {
		return nil, errors.New("armored block is not an openpgp private key")
	}
	return openpgp.ReadEntity(packet.NewReader(block.Body))
}
