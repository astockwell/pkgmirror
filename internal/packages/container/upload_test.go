package container

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// listTempFiles returns the basenames of every regular file in dir.
// Helper used by the cleanup tests to assert the on-disk state without
// poking at private struct fields.
func listTempFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// fixed returns a func compatible with the tracker's `now` field that
// always returns now. Used to make the idle sweeper deterministic in
// tests without touching real time.
func fixed(now time.Time) func() time.Time { return func() time.Time { return now } }

// TestUploadTracker_FinalizeSuccess_DoesNotRemoveFile pins the
// contract that Finalize hands ownership of the temp file to the
// caller on the success path. The caller (the OCI handler) is what
// removes it. This guards against accidentally re-introducing the
// regression where the handler stopped removing the file and we
// leaked one temp file per blob push.
func TestUploadTracker_FinalizeSuccess_DoesNotRemoveFile(t *testing.T) {
	dir := t.TempDir()
	tr := NewUploadTracker(dir)

	uuid, err := tr.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	body := []byte("hello world")
	if _, err := tr.Append(uuid, bytes.NewReader(body)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	want := computeDigest(body)
	file, _, _, err := tr.Finalize(uuid, want)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	// Contract: file exists, is open, and is the caller's to clean up.
	if _, err := os.Stat(file.Name()); err != nil {
		t.Fatalf("file should exist after Finalize: %v", err)
	}
	_ = file.Close()
	_ = os.Remove(file.Name()) // what the handler now does
	if files := listTempFiles(t, dir); len(files) != 0 {
		t.Fatalf("leftover files after caller cleanup: %v", files)
	}
}

// TestUploadTracker_FinalizeDigestMismatch_RemovesFile pins the
// digest-mismatch cleanup contract.
func TestUploadTracker_FinalizeDigestMismatch_RemovesFile(t *testing.T) {
	dir := t.TempDir()
	tr := NewUploadTracker(dir)

	uuid, err := tr.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tr.Append(uuid, bytes.NewReader([]byte("real bytes"))); err != nil {
		t.Fatalf("Append: %v", err)
	}
	_, _, _, err = tr.Finalize(uuid, "sha256:"+strings.Repeat("0", 64))
	if err == nil {
		t.Fatal("expected ErrDigestMismatch, got nil")
	}
	if files := listTempFiles(t, dir); len(files) != 0 {
		t.Fatalf("digest mismatch should have removed staged file; leftover: %v", files)
	}
}

// TestUploadTracker_CancelRemovesFile pins the explicit-cancel contract.
func TestUploadTracker_CancelRemovesFile(t *testing.T) {
	dir := t.TempDir()
	tr := NewUploadTracker(dir)

	uuid, _ := tr.Begin()
	_, _ = tr.Append(uuid, bytes.NewReader([]byte("x")))
	tr.Cancel(uuid)
	if files := listTempFiles(t, dir); len(files) != 0 {
		t.Fatalf("Cancel should have removed staged file; leftover: %v", files)
	}
	// Cancel of unknown uuid is a no-op (idempotent).
	tr.Cancel("does-not-exist")
}

// TestUploadTracker_SweepIdle_RemovesStaleSessions confirms the
// abandoned-session leak (#2) is closed. A session that's been idle
// longer than the timeout is cancelled and its temp file removed; an
// active session is not.
func TestUploadTracker_SweepIdle_RemovesStaleSessions(t *testing.T) {
	dir := t.TempDir()
	tr := NewUploadTracker(dir)

	// Freeze "now" so we can manufacture an old lastActive without
	// sleeping.
	base := time.Now()
	tr.now = fixed(base)

	staleUUID, _ := tr.Begin()
	_, _ = tr.Append(staleUUID, bytes.NewReader([]byte("stale")))

	// Advance time and start a fresh session.
	tr.now = fixed(base.Add(2 * time.Hour))
	freshUUID, _ := tr.Begin()
	_, _ = tr.Append(freshUUID, bytes.NewReader([]byte("fresh")))

	// At base+2h, the stale session's lastActive is base+0; the fresh
	// one's lastActive is base+2h. Sweep with a 1-hour timeout: only
	// the stale one should go.
	tr.now = fixed(base.Add(2 * time.Hour))
	removed := tr.sweepIdle(1 * time.Hour)
	if removed != 1 {
		t.Fatalf("expected 1 swept session, got %d", removed)
	}

	// Stale session is gone from the tracker AND from disk.
	if _, err := tr.Size(staleUUID); err == nil {
		t.Fatal("stale session should be gone from tracker")
	}
	// Fresh session is still here and usable.
	if _, err := tr.Size(freshUUID); err != nil {
		t.Fatalf("fresh session should still be tracked: %v", err)
	}
	// Disk: exactly one file remains (the fresh session's).
	if files := listTempFiles(t, dir); len(files) != 1 {
		t.Fatalf("expected 1 staged file after sweep, got %d: %v", len(files), files)
	}
}

// TestUploadTracker_SweepIdle_RespectsActivity ensures Append updates
// lastActive so a long-running upload that keeps PATCHing doesn't get
// swept out from under itself.
func TestUploadTracker_SweepIdle_RespectsActivity(t *testing.T) {
	dir := t.TempDir()
	tr := NewUploadTracker(dir)
	base := time.Now()
	tr.now = fixed(base)

	uuid, _ := tr.Begin()
	_, _ = tr.Append(uuid, bytes.NewReader([]byte("part1 ")))

	// Time passes. A new PATCH arrives.
	tr.now = fixed(base.Add(30 * time.Minute))
	_, _ = tr.Append(uuid, bytes.NewReader([]byte("part2")))

	// Sweep 1 h later from the second append (= 1h30m from session start).
	// Timeout = 1 h: the session was active 30 min ago, so it must survive.
	tr.now = fixed(base.Add(30*time.Minute + 1*time.Hour))
	if removed := tr.sweepIdle(1 * time.Hour); removed != 0 {
		t.Fatalf("expected 0 swept (recent activity), got %d", removed)
	}
}

// TestSweepOrphans_RemovesPkgmirrorPrefixedFiles confirms the
// startup-janitor contract. Crash-survivor files left in TmpDir get
// cleaned up; files belonging to anything else are left alone.
func TestSweepOrphans_RemovesPkgmirrorPrefixedFiles(t *testing.T) {
	dir := t.TempDir()

	// Manufacture three crash-survivor files (two of ours, one not).
	writeFile := func(name string) {
		t.Helper()
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		_, _ = io.WriteString(f, "leftover")
		_ = f.Close()
	}
	writeFile("pkgmirror-upload-abc123")
	writeFile("pkgmirror-oci-upload-xyz789")
	writeFile("something-else.txt")

	removed, err := SweepOrphans(dir)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if removed != 2 {
		t.Fatalf("expected 2 removed, got %d", removed)
	}
	files := listTempFiles(t, dir)
	if len(files) != 1 || files[0] != "something-else.txt" {
		t.Fatalf("non-prefix file should have survived; got %v", files)
	}
}

// TestSweepOrphans_EmptyDirIsNoOp guards against accidentally erroring
// out on a fresh install (TmpDir created on boot but never populated).
func TestSweepOrphans_EmptyDirIsNoOp(t *testing.T) {
	dir := t.TempDir()
	removed, err := SweepOrphans(dir)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed: %d", removed)
	}
}

// TestSweepOrphans_MissingDirIsNoOp confirms a deleted/never-created
// TmpDir doesn't crash the boot path.
func TestSweepOrphans_MissingDirIsNoOp(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	removed, err := SweepOrphans(dir)
	if err != nil {
		t.Fatalf("missing dir should not error: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed: %d", removed)
	}
}

// TestSweepOrphans_EmptyTmpDirIsSkipped confirms the "" fallback path:
// SweepOrphans on an unconfigured tracker (tmpDir="") is a no-op so we
// don't accidentally delete files belonging to other processes in the
// system /tmp.
func TestSweepOrphans_EmptyTmpDirIsSkipped(t *testing.T) {
	removed, err := SweepOrphans("")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed: %d", removed)
	}
}

// computeDigest helper: matches what UploadTracker uses internally so
// Finalize accepts the digest on the success-path test.
func computeDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
