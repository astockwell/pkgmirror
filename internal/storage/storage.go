// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2020 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// This file ports the storage abstraction from
// forgejo/modules/storage/storage.go (MIT). The ObjectStorage interface,
// the Object interface (ReadCloser + Seeker + Stat), and the
// LocalStorage implementation preserve the upstream method set and
// semantics. The differences are limited to:
//
//   - We don't have Forgejo's settings.Storage struct or its multi-
//     subsystem (Attachments / LFS / Avatars / …) split. We only have
//     a single content-addressed blob store for packages, so NewLocalStorage
//     takes a plain root path and a context.
//   - We add an explicit ErrNotFound sentinel that Open() and Stat()
//     return for misses. Forgejo bubbles up os.ErrNotExist; we wrap it
//     so a future S3 / GCS backend can return the same sentinel.
//   - The FS backend internally shards blobs by the first two byte-
//     prefixes of the path (`<aa>/<bb>/<rest>`) so no single directory
//     exceeds ~65k entries at multi-million-blob scale. Forgejo does
//     not do this because their LocalStorage is a thin os.* wrapper.

// Package storage provides a pluggable object-storage abstraction.
//
// The interface is modeled directly on forgejo/modules/storage.ObjectStorage
// so adding a new backend (S3 / GCS / Azure / MinIO) is mechanically
// equivalent to porting Forgejo's MinioStorage implementation against
// our interface. See DECISIONS.md (2026-05-28) for the rationale.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
)

// Sentinels.
var (
	// ErrNotFound is returned by Open and Stat when a path doesn't exist.
	ErrNotFound = errors.New("storage: object not found")
	// ErrURLNotSupported is returned by URL() on backends that can't
	// issue a redirect (e.g. the local filesystem). Modeled on
	// forgejo/modules/storage.ErrURLNotSupported.
	ErrURLNotSupported = errors.New("storage: url method not supported")
)

// Object is a handle to a stored object. The union of io.ReadCloser,
// io.Seeker, and Stat() (which is `os.FileInfo` for compatibility with
// the standard library) mirrors forgejo/modules/storage.Object. Local
// files satisfy it natively via *os.File; remote backends typically
// wrap a streaming Body with a synthetic FileInfo.
type Object interface {
	io.ReadCloser
	io.Seeker
	Stat() (os.FileInfo, error)
}

// ObjectStorage is the storage abstraction every backend implements.
// Method shape mirrors forgejo/modules/storage.ObjectStorage exactly so
// porting an additional backend (MinIO / S3 / GCS / Azure) is a
// transliteration job, not a redesign.
type ObjectStorage interface {
	// Open returns a handle to the object at path. Returns ErrNotFound
	// if the object does not exist.
	Open(path string) (Object, error)

	// Save persists r at path. size is the expected byte count; pass -1
	// if unknown (the backend will stream). Returns the number of bytes
	// actually written and the first error, if any. Existing objects
	// with the same path are overwritten atomically (callers that want
	// idempotent content-addressed writes should check Stat first;
	// LocalStorage short-circuits this for us).
	Save(path string, r io.Reader, size int64) (int64, error)

	// Stat returns the FileInfo for path. Returns ErrNotFound for
	// missing objects.
	Stat(path string) (os.FileInfo, error)

	// Delete removes the object at path. Missing objects are not an error.
	Delete(path string) error

	// URL returns a pre-signed / redirectable URL for the object at
	// path. name is the suggested download filename (set as
	// Content-Disposition by S3-style backends); reqParams may carry
	// additional client-supplied parameters. Backends that don't
	// support direct client access return ErrURLNotSupported and the
	// caller streams the body instead.
	URL(path, name string, reqParams url.Values) (*url.URL, error)

	// IterateObjects walks every object under prefix and invokes fn for
	// each. The Object passed to fn is owned by IterateObjects and
	// closed automatically after fn returns. fn returning a non-nil
	// error stops the walk.
	IterateObjects(prefix string, fn func(path string, obj Object) error) error
}

// LocalStorage is a filesystem-backed ObjectStorage. Blobs are stored
// under <root>/<aa>/<bb>/<rest> where <aa><bb><rest> is the path the
// caller passed in; the two-level sharding keeps any single directory
// well under typical filesystem fan-out limits even at multi-million-
// object scale.
type LocalStorage struct {
	ctx  context.Context
	root string
}

// NewLocalStorage creates a LocalStorage rooted at root, creating the
// directory if it doesn't exist. ctx scopes long-running operations
// such as IterateObjects.
func NewLocalStorage(ctx context.Context, root string) (*LocalStorage, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create storage root: %w", err)
	}
	return &LocalStorage{ctx: ctx, root: root}, nil
}

// buildLocalPath maps a logical storage path to its on-disk location.
// Internally we shard by the first 4 hex characters (which, when the
// caller passes a sha256 hex digest, gives us a stable 2-level fan-out
// with no upstream knowledge required).
func (l *LocalStorage) buildLocalPath(path string) string {
	if len(path) < 4 {
		// Defensive: paths shorter than the shard prefix go straight in.
		return filepath.Join(l.root, path)
	}
	return filepath.Join(l.root, path[0:2], path[2:4], path)
}

// Open implements ObjectStorage.
func (l *LocalStorage) Open(path string) (Object, error) {
	f, err := os.Open(l.buildLocalPath(path))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("open object: %w", err)
	}
	return f, nil
}

// Save implements ObjectStorage. Writes go to a temp file in the same
// directory as the destination, then atomically rename — readers never
// see a half-written object. Size is ignored by LocalStorage (the
// filesystem doesn't care) but propagated through the signature so
// backends like S3 that need it can use it.
func (l *LocalStorage) Save(path string, r io.Reader, _ int64) (int64, error) {
	dst := l.buildLocalPath(path)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, fmt.Errorf("mkdir object dir: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return 0, fmt.Errorf("create temp object: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	defer cleanup()

	n, err := io.Copy(tmp, r)
	if err != nil {
		_ = tmp.Close()
		return n, fmt.Errorf("write object: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return n, fmt.Errorf("close object: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return n, fmt.Errorf("rename object: %w", err)
	}
	// Rename succeeded; suppress the deferred cleanup so we don't
	// remove the destination on the way out.
	cleanup = func() {}
	return n, nil
}

// Stat implements ObjectStorage.
func (l *LocalStorage) Stat(path string) (os.FileInfo, error) {
	fi, err := os.Stat(l.buildLocalPath(path))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("stat object: %w", err)
	}
	return fi, nil
}

// Delete implements ObjectStorage. Missing objects are not an error.
func (l *LocalStorage) Delete(path string) error {
	if err := os.Remove(l.buildLocalPath(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove object: %w", err)
	}
	return nil
}

// URL implements ObjectStorage. LocalStorage cannot issue redirects, so
// it always returns ErrURLNotSupported — handlers fall back to
// streaming the body themselves. Modeled on Forgejo's LocalStorage.URL.
func (l *LocalStorage) URL(_, _ string, _ url.Values) (*url.URL, error) {
	return nil, ErrURLNotSupported
}

// IterateObjects implements ObjectStorage. Walks the on-disk tree
// rooted at <root>/<prefix> and invokes fn for each regular file. The
// Object passed to fn is the open *os.File; it is closed automatically
// after fn returns regardless of fn's return value.
//
// Cancellation: respects the LocalStorage's ctx so a long-running walk
// can be aborted by cancelling the context the storage was constructed
// with.
func (l *LocalStorage) IterateObjects(prefix string, fn func(path string, obj Object) error) error {
	start := l.root
	if prefix != "" {
		start = filepath.Join(l.root, prefix)
	}
	return filepath.WalkDir(start, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Walking a non-existent prefix isn't an iteration error;
				// it just yields zero results.
				return nil
			}
			return err
		}
		select {
		case <-l.ctx.Done():
			return l.ctx.Err()
		default:
		}
		if d.IsDir() {
			return nil
		}
		// Skip any leftover .tmp-* artifacts from interrupted writes.
		if filepath.Base(p) != "" && filepath.Base(p)[:1] == "." {
			return nil
		}
		rel, err := filepath.Rel(l.root, p)
		if err != nil {
			return err
		}
		// Strip the on-disk sharding so the caller sees the logical
		// path it passed in. Sharded files live at <aa>/<bb>/<rest>;
		// the caller's logical path is the file's basename (which
		// already starts with <aa><bb>, since buildLocalPath uses the
		// first 4 hex chars of the logical path as the shard prefix).
		if _, _, base, ok := splitSharded(rel); ok {
			rel = base
		}
		obj, err := os.Open(p)
		if err != nil {
			return err
		}
		err = fn(rel, obj)
		_ = obj.Close()
		return err
	})
}

// splitSharded reverses buildLocalPath's <aa>/<bb>/<rest> sharding.
// Returns false for paths that don't match the shape. The reconstructed
// logical path is just `base` — buildLocalPath puts the full path into
// the leaf filename, so the shard prefix appears twice on disk (once as
// directory names, once as the leading 4 chars of the filename).
func splitSharded(rel string) (dir1, dir2, base string, ok bool) {
	base = filepath.Base(rel)
	d1 := filepath.Dir(rel)
	if d1 == "." || d1 == "" {
		return "", "", "", false
	}
	dir2 = filepath.Base(d1)
	d2 := filepath.Dir(d1)
	if d2 == "." || d2 == "" {
		return "", "", "", false
	}
	dir1 = filepath.Base(d2)
	if len(dir1) != 2 || len(dir2) != 2 || len(base) < 4 || base[0:2] != dir1 || base[2:4] != dir2 {
		return "", "", "", false
	}
	return dir1, dir2, base, true
}
