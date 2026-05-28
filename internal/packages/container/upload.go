// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// SPDX-License-Identifier: MIT
//
// The "upload session as on-disk temp file" model and the three-step
// POST → PATCH → PUT lifecycle below are the same shape Forgejo uses
// in services/packages/container/blob_uploader.go (MIT). The
// in-memory tracker, idle sweeper, and orphan-on-boot sweeper
// implementations are pkgmirror-original.
//
// In-memory tracker for in-progress OCI blob uploads. The OCI
// distribution spec lets a client open an upload session
// (POST /blobs/uploads/), stream bytes into it across one or more PATCH
// requests, and finalize with PUT. Bytes live in a temp file until
// finalize, at which point we move them into the regular content-
// addressed blob store.
//
// State is in-process only — restarting pkgmirror cancels all in-flight
// uploads. A future enhancement could persist the tracker so multi-hour
// pushes survive restarts.
//
// Cleanup story:
//  1. Happy / error paths in Finalize and Cancel close + remove the
//     temp file synchronously.
//  2. Abandoned sessions (POST without follow-up) are swept by a
//     background goroutine that wakes every IdleSweepInterval and
//     cancels any session whose last activity is older than IdleTimeout.
//     Defaults: scan every 5 min, time out at 24 h, mirroring the OCI
//     distribution spec's suggested registry behavior.
//  3. Crash-survivor temp files (process killed mid-upload, files left
//     on disk) are swept by SweepOrphans, called once at boot.

package container

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Defaults for idle session cleanup. Exposed so tests can override.
var (
	IdleSweepInterval = 5 * time.Minute
	IdleTimeout       = 24 * time.Hour
)

// Prefixes used by orphan-scanners. Anything matching is a known
// pkgmirror staging file; non-matching files are left alone.
const (
	ociUploadPrefix      = "pkgmirror-oci-upload-"
	hashedBufferPrefix   = "pkgmirror-upload-"
)

// ErrNoSuchUpload is returned for PATCH/PUT against an unknown upload UUID.
var ErrNoSuchUpload = errors.New("container: no such upload")

// ErrDigestMismatch is returned from Finalize when the client-supplied
// digest disagrees with the bytes actually received.
var ErrDigestMismatch = errors.New("container: digest mismatch")

// upload is one in-progress blob upload.
type upload struct {
	uuid       string
	file       *os.File
	size       int64
	lastActive time.Time
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
	// tmpDir is where Begin() creates staging files. Empty falls back
	// to os.TempDir() (back-compat for tests that don't plumb a dir).
	tmpDir string
	// now is overridable for tests of the idle sweeper. Defaults to
	// time.Now.
	now func() time.Time
}

// NewUploadTracker returns an empty tracker that stages files under
// tmpDir. Pass "" to fall back to the system temp dir; production
// callers should pass Service.TmpDir so staging lands on the same
// filesystem as the blob store.
func NewUploadTracker(tmpDir string) *UploadTracker {
	return &UploadTracker{
		uploads: map[string]*upload{},
		tmpDir:  tmpDir,
		now:     time.Now,
	}
}

// Begin opens a new upload session and returns its UUID. Callers append
// bytes via Append() and complete via Finalize().
func (t *UploadTracker) Begin() (string, error) {
	id, err := newUUID()
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(t.tmpDir, ociUploadPrefix+"*")
	if err != nil {
		return "", fmt.Errorf("create upload temp: %w", err)
	}
	t.mu.Lock()
	t.uploads[id] = &upload{uuid: id, file: f, hasher: sha256.New(), lastActive: t.now()}
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
	t.mu.Lock()
	u.lastActive = t.now()
	t.mu.Unlock()
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

// --- idle cleanup ----------------------------------------------------------

// StartIdleSweeper launches a goroutine that periodically cancels any
// upload session whose last activity is older than IdleTimeout. The
// sweeper exits when ctx is cancelled.
//
// Callers SHOULD start this when constructing the tracker in production;
// tests typically don't bother since they explicitly Cancel() or
// Finalize() every session they create.
func (t *UploadTracker) StartIdleSweeper(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(IdleSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				t.sweepIdle(IdleTimeout)
			}
		}
	}()
}

// sweepIdle cancels any session whose last activity is more than
// timeout ago. Exposed for tests that want to drive the sweeper
// deterministically.
func (t *UploadTracker) sweepIdle(timeout time.Duration) int {
	cutoff := t.now().Add(-timeout)
	var stale []*upload
	t.mu.Lock()
	for id, u := range t.uploads {
		if u.lastActive.Before(cutoff) {
			stale = append(stale, u)
			delete(t.uploads, id)
		}
	}
	t.mu.Unlock()
	for _, u := range stale {
		_ = u.file.Close()
		_ = os.Remove(u.file.Name())
	}
	return len(stale)
}

// SweepOrphans removes any leftover pkgmirror staging files in tmpDir.
// Run once at boot: a previous process crashing mid-upload leaves
// pkgmirror-upload-* (HashedBuffer) and pkgmirror-oci-upload-* (this
// tracker) files behind, and there's no in-memory state pointing at
// them anymore. We only delete files matching our own prefixes so
// nothing else in tmpDir is at risk.
//
// Returns the number of files removed and the first error encountered
// (the scan continues past per-file errors; the caller gets the best-
// effort count and is expected to log the error rather than abort).
func SweepOrphans(tmpDir string) (int, error) {
	if tmpDir == "" {
		// os.TempDir() fallback: don't sweep the system tmp because we
		// might delete files belonging to other processes that happen to
		// share a prefix (extremely unlikely but not worth the risk).
		// Production callers always pass a configured TmpDir.
		return 0, nil
	}
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("read tmpdir: %w", err)
	}
	var removed int
	var firstErr error
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, ociUploadPrefix) && !strings.HasPrefix(name, hashedBufferPrefix) {
			continue
		}
		if err := os.Remove(filepath.Join(tmpDir, name)); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		removed++
	}
	return removed, firstErr
}
