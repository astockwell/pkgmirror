// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2023 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// APKINDEX.tar.gz construction and per-tenant RSA signing key
// management. Modeled on forgejo/services/packages/alpine/repository.go
// (MIT). The byte format (DESCRIPTION + APKINDEX inside a tar inside a
// gzip, prefixed by a separate signed gzip stream containing the
// detached signature) is what real `apk` clients consume — see
// https://wiki.alpinelinux.org/wiki/Apk_spec#APKINDEX_Format.

package alpine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/astockwell/pkgmirror/internal/models"
)

// IndexFilename is what the APKINDEX file inside the tar archive is
// named — `apk` insists on this byte-for-byte. Don't change.
const (
	IndexFilename        = "APKINDEX"
	IndexArchiveFilename = "APKINDEX.tar.gz"
)

// DefaultRSAKeyBits is the default RSA key length used for signing the
// APKINDEX when a Handler.RSAKeyBits isn't set. 4096 matches forgejo
// and is what we ship in production. Tests override this from
// TestMain to 2048 because the per-fixture key generation otherwise
// dominates suite wall time and can time out under heavy parallel
// `go test ./...` load on slow CI runners.
var DefaultRSAKeyBits = 4096

// indexEntry is the per-package row materialised from DB lookups before
// we serialise to APKINDEX text format.
type indexEntry struct {
	pkg     *models.Package
	ver     *models.Version
	blob    *models.Blob
	verMeta VersionMetadata
	fileMd  FileMetadata
}

// BuildIndexArchive materialises the signed APKINDEX.tar.gz for one
// (tenant, branch, repository, architecture) coordinate. The body of
// each .apk in scope is loaded from `entries`. Returns nil if there are
// no packages — callers should respond 404.
//
// We build the index on-the-fly per request rather than caching it as a
// file. This trades CPU for simplicity (no "rebuild on every
// upload/delete" coordination); the index is small enough that the
// blackbox `apk update` round-trip stays well under a second.
func BuildIndexArchive(entries []*indexEntry, privPEM, ownerLowerName string) ([]byte, error) {
	if len(entries) == 0 {
		return nil, nil
	}

	// Sort-stable order: caller already provides entries in (name,
	// version) order. We don't re-sort here.

	// 1) APKINDEX text body.
	var body bytes.Buffer
	for _, e := range entries {
		writeIndexEntry(&body, e)
	}

	// 2) Wrap APKINDEX in a tar inside a gzip stream.
	// Note: writeGzipStream(addTarEnd=true) writes the standard tar
	// trailing zero blocks. The unsigned stream needs the trailer
	// because it's the "real" archive; the SIGNATURE stream doesn't.
	var unsigned bytes.Buffer
	h := sha1.New()
	if err := writeGzipStream(io.MultiWriter(&unsigned, h), IndexFilename, body.Bytes(), true); err != nil {
		return nil, err
	}

	// 3) Sign the gz bytes of the unsigned stream with RSA-PKCS1-v1.5
	// SHA1 (yes, SHA1 — that's what apk requires; not a choice we
	// can make differently).
	privPemBlock, _ := pem.Decode([]byte(privPEM))
	if privPemBlock == nil {
		return nil, errors.New("alpine: failed to decode signing key PEM")
	}
	privKey, err := x509.ParsePKCS1PrivateKey(privPemBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("alpine: parse signing key: %w", err)
	}
	signature, err := rsa.SignPKCS1v15(rand.Reader, privKey, crypto.SHA1, h.Sum(nil))
	if err != nil {
		return nil, fmt.Errorf("alpine: sign index: %w", err)
	}

	fingerprint, err := publicKeyFingerprint(&privKey.PublicKey)
	if err != nil {
		return nil, err
	}

	// 4) The signed final archive is a separate gz stream containing
	// the detached .SIGN.RSA file, concatenated with the unsigned
	// archive. apk reads the first stream, verifies, then moves on
	// to the next stream which is the real archive.
	var out bytes.Buffer
	sigName := fmt.Sprintf(".SIGN.RSA.%s@%s.rsa.pub", ownerLowerName, hex.EncodeToString(fingerprint))
	if err := writeGzipStream(&out, sigName, signature, false); err != nil {
		return nil, err
	}
	if _, err := out.Write(unsigned.Bytes()); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// writeIndexEntry formats one package's record using the APKINDEX
// field-letter convention. Ordering follows
// https://wiki.alpinelinux.org/wiki/Apk_spec#APKINDEX_Format which apk
// tolerates in any order, but we match what `apk fetch` writes so
// diffs against a real registry are easy to read.
func writeIndexEntry(w *bytes.Buffer, e *indexEntry) {
	fmt.Fprintf(w, "C:%s\n", e.fileMd.Checksum)
	fmt.Fprintf(w, "P:%s\n", e.pkg.Name)
	fmt.Fprintf(w, "V:%s\n", e.ver.Version)
	fmt.Fprintf(w, "A:%s\n", e.fileMd.Architecture)
	if e.verMeta.Description != "" {
		fmt.Fprintf(w, "T:%s\n", e.verMeta.Description)
	}
	if e.verMeta.ProjectURL != "" {
		fmt.Fprintf(w, "U:%s\n", e.verMeta.ProjectURL)
	}
	if e.verMeta.License != "" {
		fmt.Fprintf(w, "L:%s\n", e.verMeta.License)
	}
	fmt.Fprintf(w, "S:%d\n", e.blob.Size)
	fmt.Fprintf(w, "I:%d\n", e.fileMd.Size)
	fmt.Fprintf(w, "o:%s\n", e.fileMd.Origin)
	fmt.Fprintf(w, "m:%s\n", e.verMeta.Maintainer)
	fmt.Fprintf(w, "t:%d\n", e.fileMd.BuildDate)
	if e.fileMd.CommitHash != "" {
		fmt.Fprintf(w, "c:%s\n", e.fileMd.CommitHash)
	}
	if len(e.fileMd.Dependencies) > 0 {
		fmt.Fprintf(w, "D:%s\n", strings.Join(e.fileMd.Dependencies, " "))
	}
	if len(e.fileMd.Provides) > 0 {
		fmt.Fprintf(w, "p:%s\n", strings.Join(e.fileMd.Provides, " "))
	}
	if e.fileMd.InstallIf != "" {
		fmt.Fprintf(w, "i:%s\n", e.fileMd.InstallIf)
	}
	if e.fileMd.ProviderPriority > 0 {
		fmt.Fprintf(w, "k:%d\n", e.fileMd.ProviderPriority)
	}
	w.WriteByte('\n')
}

// writeGzipStream writes one tar header + body inside one gzip stream.
// If addTarEnd is true, the tar's two trailing zero blocks are written
// (standard archive end). For detached signature streams apk wants the
// tar trailer omitted so the next stream concatenates cleanly.
//
// We always call tw.Flush() (when not Closing) to write the body's
// trailing 512-byte-block padding. tar.Writer only pads implicitly on
// WriteHeader-of-next-entry or Close; without one of those, a body of
// size N produces a stream of exactly 512+N bytes — missing the
// padding-to-block-boundary that the tar format requires. apk happens
// to tolerate this because it reads the signature body by explicit
// byte count and never seeks past it, but the bytes are still
// technically malformed and break any reader that *does* try to walk
// the concatenated tars (our test helper, downstream tools, etc.).
// This was masked by 4096-bit RSA keys (512-byte signature = exactly
// one tar block, no padding needed) and surfaced only when we tried
// 2048-bit keys in tests.
//
// Ported from forgejo/services/packages/alpine/repository.go, with the
// Flush() fix applied on top.
func writeGzipStream(w io.Writer, filename string, content []byte, addTarEnd bool) error {
	zw := gzip.NewWriter(w)
	defer zw.Close()

	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{
		Name: filename,
		Mode: 0o600,
		Size: int64(len(content)),
	}); err != nil {
		return err
	}
	if _, err := tw.Write(content); err != nil {
		return err
	}
	if addTarEnd {
		// Close writes body padding + two trailing zero blocks.
		return tw.Close()
	}
	// Flush writes body padding only. No trailing zero blocks, so
	// the next gzip stream's tar can be concatenated cleanly.
	return tw.Flush()
}

// GenerateKeyPair builds a fresh RSA keypair encoded as PEM strings,
// matching what forgejo/modules/util.GenerateKeyPair returns. The
// private key is PKCS#1 / "RSA PRIVATE KEY"; the public key is PKIX /
// "PUBLIC KEY". apk validates against PKIX-encoded public keys.
func GenerateKeyPair(bits int) (privatePEM, publicPEM string, err error) {
	if bits == 0 {
		bits = DefaultRSAKeyBits
	}
	priv, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		return "", "", err
	}
	privBytes := x509.MarshalPKCS1PrivateKey(priv)
	privBlock := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: privBytes}

	pubBytes, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return "", "", err
	}
	pubBlock := &pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes}

	return string(pem.EncodeToMemory(privBlock)), string(pem.EncodeToMemory(pubBlock)), nil
}

// publicKeyFingerprint returns the SHA1 hash of the DER-encoded PKIX
// public key. This is the same fingerprint forgejo and gitlab use; the
// filename `<owner>@<fingerprint>.rsa.pub` must be byte-equal across
// the public-key endpoint and the APKINDEX signature header for apk to
// trust the signature.
func publicKeyFingerprint(pub *rsa.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	h := sha1.New()
	h.Write(der)
	return h.Sum(nil), nil
}

// GetOrCreateKeyPair returns the per-tenant RSA signing keypair as PEM
// strings, generating one on first call. Keys are stored as properties
// on a synthetic `_alpine` package row scoped to the tenant — we don't
// have a separate tenant-settings table and this matches forgejo's
// "internal package" convention so an operator inspecting the DB will
// recognise the layout.
func GetOrCreateKeyPair(ctx context.Context, m *models.Store, tenantID int64, bits int) (privatePEM, publicPEM string, err error) {
	pkg, err := m.GetOrCreatePackage(ctx, tenantID, models.TypeAlpine, RepositoryPackage, models.CreatedViaUploaded)
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

	priv, pub, err = GenerateKeyPair(bits)
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

// PublicKeyFingerprintHex returns the lowercase-hex SHA1 fingerprint of
// the supplied PEM-encoded PKIX public key. Used to build the
// `<owner>@<fingerprint>.rsa.pub` filename that apk expects in
// /etc/apk/keys/.
func PublicKeyFingerprintHex(publicPEM string) (string, error) {
	block, _ := pem.Decode([]byte(publicPEM))
	if block == nil {
		return "", errors.New("alpine: failed to decode public key PEM")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return "", err
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return "", errors.New("alpine: public key is not RSA")
	}
	fp, err := publicKeyFingerprint(rsaPub)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(fp), nil
}
