// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// SPDX-License-Identifier: MIT
//
// In-memory tracker for in-progress OCI blob uploads. The OCI
// distribution spec lets a client open an upload session
// (POST /blobs/uploads/), stream bytes into it across one or more PATCH
// requests, and finalize with PUT. Bytes live in a temp file until
// finalize, at which point we move them into the regular content-
// addressed blob store.
//
// State is in-process only — restarting pkgmirror cancels all in-flight
// uploads. This is fine for typical pushes (a `crane push` of an alpine
// image completes in seconds); a future enhancement could persist the
// tracker so multi-hour pushes survive restarts.

package container

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// ErrNoSuchUpload is returned for PATCH/PUT against an unknown upload UUID.
var ErrNoSuchUpload = errors.New("container: no such upload")

// ErrDigestMismatch is returned from Finalize when the client-supplied
// digest disagrees with the bytes actually received.
var ErrDigestMismatch = errors.New("container: digest mismatch")

// upload is one in-progress blob upload.
type upload struct {
	uuid string
	file *os.File
	size int64
	// sha256 over the bytes written so far. We compute this incrementally
	// so finalize doesn't have to re-read the temp file.
	hasher interface {
		io.Writer
		Sum([]byte) []byte
	}
}

// UploadTracker is an in-memory registry of in-flight uploads keyed by UUID.
// Safe for concurrent use.
type UploadTracker struct {
	mu      sync.Mutex
	uploads map[string]*upload
}

// NewUploadTracker returns an empty tracker.
func NewUploadTracker() *UploadTracker {
	return &UploadTracker{uploads: map[string]*upload{}}
}

// Begin opens a new upload session and returns its UUID. Callers append
// bytes via Append() and complete via Finalize().
func (t *UploadTracker) Begin() (string, error) {
	id, err := newUUID()
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "pkgmirror-oci-upload-*")
	if err != nil {
		return "", fmt.Errorf("create upload temp: %w", err)
	}
	t.mu.Lock()
	t.uploads[id] = &upload{uuid: id, file: f, hasher: sha256.New()}
	t.mu.Unlock()
	return id, nil
}

// Append writes r into the upload session identified by uuid. Returns
// the new total size. Used by PATCH and by PUT-with-body.
func (t *UploadTracker) Append(uuid string, r io.Reader) (int64, error) {
	t.mu.Lock()
	u, ok := t.uploads[uuid]
	t.mu.Unlock()
	if !ok {
		return 0, ErrNoSuchUpload
	}
	// Tee writes into both the temp file and the running hash so we don't
	// need a second pass at finalize time.
	mw := io.MultiWriter(u.file, u.hasher)
	n, err := io.Copy(mw, r)
	if err != nil {
		return u.size, err
	}
	u.size += n
	return u.size, nil
}

// Size returns the current byte count for an upload session.
func (t *UploadTracker) Size(uuid string) (int64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	u, ok := t.uploads[uuid]
	if !ok {
		return 0, ErrNoSuchUpload
	}
	return u.size, nil
}

// Finalize closes the upload session, verifies that the assembled bytes
// hash to wantDigest (must be "sha256:<hex>"), and returns a reader on
// the staged temp file. Callers MUST drain the reader and Close it; the
// tracker entry is removed before this returns, so the file is the
// caller's responsibility from here on.
//
// On digest mismatch the file is closed and removed and ErrDigestMismatch
// is returned.
func (t *UploadTracker) Finalize(uuid, wantDigest string) (*os.File, int64, string, error) {
	t.mu.Lock()
	u, ok := t.uploads[uuid]
	if ok {
		delete(t.uploads, uuid)
	}
	t.mu.Unlock()
	if !ok {
		return nil, 0, "", ErrNoSuchUpload
	}

	gotDigest := "sha256:" + hex.EncodeToString(u.hasher.Sum(nil))
	if wantDigest != "" && wantDigest != gotDigest {
		_ = u.file.Close()
		_ = os.Remove(u.file.Name())
		return nil, 0, "", fmt.Errorf("%w: got %s, want %s", ErrDigestMismatch, gotDigest, wantDigest)
	}

	// Rewind for the caller to stream into permanent storage.
	if _, err := u.file.Seek(0, io.SeekStart); err != nil {
		_ = u.file.Close()
		_ = os.Remove(u.file.Name())
		return nil, 0, "", err
	}
	return u.file, u.size, gotDigest, nil
}

// Cancel discards an in-progress upload. Idempotent.
func (t *UploadTracker) Cancel(uuid string) {
	t.mu.Lock()
	u, ok := t.uploads[uuid]
	if ok {
		delete(t.uploads, uuid)
	}
	t.mu.Unlock()
	if ok {
		_ = u.file.Close()
		_ = os.Remove(u.file.Name())
	}
}

// newUUID returns a random 32-hex-character identifier. We don't strictly
// need RFC 4122 UUIDs — the OCI spec just calls for "an opaque
// identifier" — but a 16-byte random hex value is shorter than v4
// formatted UUIDs and just as collision-resistant.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
