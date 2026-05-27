// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2021-2023 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// HashedBuffer is modeled on forgejo/modules/packages/hashed_buffer.go and
// modules/packages/multi_hasher.go; CreatePackageAndAddFile mirrors the
// orchestration of forgejo/services/packages/packages.go. The upstream code
// is MIT licensed.

// Package packages provides the format-agnostic service layer for ingesting
// package uploads: a multi-hashing buffer and a CreatePackageAndAddFile
// orchestration function.
package packages

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"

	"github.com/astockwell/pkgmirror/internal/models"
	"github.com/astockwell/pkgmirror/internal/storage"
)

// HashedBuffer buffers an upload to a temp file while computing MD5, SHA-1,
// SHA-256, and SHA-512 of the streamed bytes. It implements io.ReaderAt so
// parsers (e.g. archive/zip) can read it without an extra copy.
type HashedBuffer struct {
	file *os.File
	size int64

	md5    hash.Hash
	sha1   hash.Hash
	sha256 hash.Hash
	sha512 hash.Hash
}

// NewHashedBufferFromReader drains r into a temp file, hashing as it goes.
func NewHashedBufferFromReader(r io.Reader) (*HashedBuffer, error) {
	f, err := os.CreateTemp("", "pkgmirror-upload-*")
	if err != nil {
		return nil, fmt.Errorf("create temp buffer: %w", err)
	}

	hb := &HashedBuffer{
		file:   f,
		md5:    md5.New(),
		sha1:   sha1.New(),
		sha256: sha256.New(),
		sha512: sha512.New(),
	}

	mw := io.MultiWriter(f, hb.md5, hb.sha1, hb.sha256, hb.sha512)
	n, err := io.Copy(mw, r)
	if err != nil {
		hb.Close()
		return nil, fmt.Errorf("buffer upload: %w", err)
	}
	hb.size = n
	return hb, nil
}

// Size returns the number of bytes buffered.
func (b *HashedBuffer) Size() int64 { return b.size }

// ReadAt implements io.ReaderAt against the temp file.
func (b *HashedBuffer) ReadAt(p []byte, off int64) (int, error) {
	return b.file.ReadAt(p, off)
}

// Seek rewinds the underlying file (used before streaming bytes into storage).
func (b *HashedBuffer) Seek(offset int64, whence int) (int64, error) {
	return b.file.Seek(offset, whence)
}

// Read implements io.Reader.
func (b *HashedBuffer) Read(p []byte) (int, error) {
	return b.file.Read(p)
}

// Sums returns the hex-encoded digests.
func (b *HashedBuffer) Sums() (md5Hex, sha1Hex, sha256Hex, sha512Hex string) {
	return hex.EncodeToString(b.md5.Sum(nil)),
		hex.EncodeToString(b.sha1.Sum(nil)),
		hex.EncodeToString(b.sha256.Sum(nil)),
		hex.EncodeToString(b.sha512.Sum(nil))
}

// Close releases the temp file.
func (b *HashedBuffer) Close() error {
	if b.file == nil {
		return nil
	}
	name := b.file.Name()
	err := b.file.Close()
	_ = os.Remove(name)
	b.file = nil
	return err
}

// Service orchestrates creating packages, versions, blobs, and files using a
// metadata store and a blob backend.
type Service struct {
	Models  *models.Store
	Storage storage.Backend
}

// NewService constructs a Service.
func NewService(m *models.Store, s storage.Backend) *Service {
	return &Service{Models: m, Storage: s}
}

// CreationInfo carries everything needed to ingest a single uploaded file.
type CreationInfo struct {
	TenantID            int64
	PackageType         models.Type
	PackageName         string
	Version             string
	VersionProperties   map[string]string
	VersionMetadataJSON string
	Filename            string
	IsLead              bool
}

// CreatePackageAndAddFile creates (or fetches) the package, creates the
// version (failing with models.ErrDuplicatePackageVersion if it already
// exists), stores the bytes, and links them with a file row.
func (s *Service) CreatePackageAndAddFile(ctx context.Context, info CreationInfo, buf *HashedBuffer) (*models.Package, *models.Version, *models.File, error) {
	if info.TenantID == 0 {
		return nil, nil, nil, fmt.Errorf("CreatePackageAndAddFile: TenantID is required")
	}
	pkg, err := s.Models.GetOrCreatePackage(ctx, info.TenantID, info.PackageType, info.PackageName)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create package: %w", err)
	}

	ver, err := s.Models.CreateVersion(ctx, pkg.ID, info.Version, info.VersionMetadataJSON)
	if err != nil {
		return pkg, nil, nil, err
	}

	for k, v := range info.VersionProperties {
		if err := s.Models.SetProperty(ctx, models.PropertyRefVersion, ver.ID, k, v); err != nil {
			return pkg, ver, nil, fmt.Errorf("set version property %q: %w", k, err)
		}
	}

	md5Hex, sha1Hex, sha256Hex, sha512Hex := buf.Sums()

	if _, err := buf.Seek(0, io.SeekStart); err != nil {
		return pkg, ver, nil, fmt.Errorf("rewind upload buffer: %w", err)
	}
	if err := s.Storage.Put(sha256Hex, buf); err != nil {
		return pkg, ver, nil, fmt.Errorf("store blob: %w", err)
	}

	blob, err := s.Models.GetOrCreateBlob(ctx, models.Blob{
		Size:       buf.Size(),
		HashMD5:    md5Hex,
		HashSHA1:   sha1Hex,
		HashSHA256: sha256Hex,
		HashSHA512: sha512Hex,
	})
	if err != nil {
		return pkg, ver, nil, fmt.Errorf("record blob: %w", err)
	}

	file, err := s.Models.CreateFile(ctx, models.File{
		VersionID: ver.ID,
		BlobID:    blob.ID,
		Name:      info.Filename,
		IsLead:    info.IsLead,
	})
	if err != nil {
		return pkg, ver, nil, fmt.Errorf("create file: %w", err)
	}
	return pkg, ver, file, nil
}

// OpenFile returns a reader for the blob backing the given file row.
func (s *Service) OpenFile(ctx context.Context, f *models.File) (io.ReadSeekCloser, *models.Blob, error) {
	blob, err := s.openBlob(ctx, f.BlobID)
	if err != nil {
		return nil, nil, err
	}
	rc, err := s.Storage.Open(blob.HashSHA256)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, blob, fmt.Errorf("blob %d (sha256=%s) missing from storage", blob.ID, blob.HashSHA256)
		}
		return nil, blob, err
	}
	return rc, blob, nil
}

func (s *Service) openBlob(ctx context.Context, id int64) (*models.Blob, error) {
	row := s.Models.DB.QueryRowContext(ctx,
		`SELECT id, size, hash_md5, hash_sha1, hash_sha256, hash_sha512, created_unix
		   FROM package_blobs WHERE id = ?`, id)
	b := &models.Blob{}
	if err := row.Scan(&b.ID, &b.Size, &b.HashMD5, &b.HashSHA1, &b.HashSHA256, &b.HashSHA512, &b.CreatedUnix); err != nil {
		return nil, fmt.Errorf("load blob %d: %w", id, err)
	}
	return b, nil
}
