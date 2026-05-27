// Package storage provides a content-addressed blob storage backend.
package storage

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ErrNotFound is returned when a blob is not present in storage.
var ErrNotFound = errors.New("storage: blob not found")

// Backend is the interface implemented by storage backends.
type Backend interface {
	// Put writes the contents of r to storage under the given SHA-256 hex digest.
	// It is a no-op (and not an error) if the blob already exists.
	Put(sha256Hex string, r io.Reader) error

	// Open returns a ReadSeekCloser for the blob with the given SHA-256 digest.
	// Returns ErrNotFound if the blob does not exist.
	Open(sha256Hex string) (io.ReadSeekCloser, error)

	// Delete removes a blob from storage. Idempotent.
	Delete(sha256Hex string) error
}

// FS is a filesystem-backed blob backend. Blobs are stored under
// <root>/<aa>/<bb>/<full-sha256-hex>.
type FS struct {
	root string
}

// NewFS creates a new filesystem backend rooted at root, creating the
// directory if needed.
func NewFS(root string) (*FS, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create blob root: %w", err)
	}
	return &FS{root: root}, nil
}

func (f *FS) pathFor(sha256Hex string) string {
	if len(sha256Hex) < 4 {
		// Defensive: should never happen with valid sha-256 hex (64 chars).
		return filepath.Join(f.root, sha256Hex)
	}
	return filepath.Join(f.root, sha256Hex[0:2], sha256Hex[2:4], sha256Hex)
}

// Put writes r to the storage location for sha256Hex. If the file already
// exists it is left untouched. Writes happen via a temp file + rename so
// readers never see a half-written blob.
func (f *FS) Put(sha256Hex string, r io.Reader) error {
	dst := f.pathFor(sha256Hex)
	if _, err := os.Stat(dst); err == nil {
		// Already present — content-addressed, so it's the same bytes.
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdir blob dir: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp blob: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// If we still have a path on disk and dst was not created, clean up.
		_ = os.Remove(tmpName)
	}()

	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close blob: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("rename blob: %w", err)
	}
	return nil
}

// Open returns a reader for the blob with the given SHA-256 digest.
func (f *FS) Open(sha256Hex string) (io.ReadSeekCloser, error) {
	fp, err := os.Open(f.pathFor(sha256Hex))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("open blob: %w", err)
	}
	return fp, nil
}

// Delete removes the blob if it exists. Missing files are not an error.
func (f *FS) Delete(sha256Hex string) error {
	if err := os.Remove(f.pathFor(sha256Hex)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove blob: %w", err)
	}
	return nil
}
