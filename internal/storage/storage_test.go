package storage_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/astockwell/pkgmirror/internal/storage"
)

// hex64 returns a 64-character lowercase hex string by repeating prefix
// — convenient for synthesizing sha256-shaped "keys" in tests without
// caring about the actual hash of any content.
func hex64(prefix string) string {
	out := prefix
	for len(out) < 64 {
		out += "0"
	}
	return out[:64]
}

func newStore(t *testing.T) (*storage.LocalStorage, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := storage.NewLocalStorage(context.Background(), dir)
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}
	return s, dir
}

func TestLocalStorage_SaveOpenStat(t *testing.T) {
	s, _ := newStore(t)
	key := hex64("aabbccdd")
	body := []byte("hello world")

	n, err := s.Save(key, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if n != int64(len(body)) {
		t.Fatalf("Save returned %d, want %d", n, len(body))
	}

	// Stat without opening.
	fi, err := s.Stat(key)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Size() != int64(len(body)) {
		t.Fatalf("Stat size: %d", fi.Size())
	}

	// Open + read.
	obj, err := s.Open(key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer obj.Close()

	got, err := io.ReadAll(obj)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("body differs: got %q want %q", got, body)
	}

	// Stat on the open Object.
	objFI, err := obj.Stat()
	if err != nil {
		t.Fatalf("obj.Stat: %v", err)
	}
	if objFI.Size() != int64(len(body)) {
		t.Fatalf("obj.Stat size: %d", objFI.Size())
	}
}

func TestLocalStorage_OpenMissingReturnsErrNotFound(t *testing.T) {
	s, _ := newStore(t)
	_, err := s.Open(hex64("missing0"))
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Open missing: got %v, want ErrNotFound", err)
	}
}

func TestLocalStorage_StatMissingReturnsErrNotFound(t *testing.T) {
	s, _ := newStore(t)
	_, err := s.Stat(hex64("missing0"))
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Stat missing: got %v, want ErrNotFound", err)
	}
}

func TestLocalStorage_DeleteIsIdempotent(t *testing.T) {
	s, _ := newStore(t)
	key := hex64("11223344")
	_, _ = s.Save(key, bytes.NewReader([]byte("x")), 1)
	if err := s.Delete(key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Second delete must not error.
	if err := s.Delete(key); err != nil {
		t.Fatalf("Delete idempotency: %v", err)
	}
	if _, err := s.Open(key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("post-delete Open: %v", err)
	}
}

func TestLocalStorage_SaveOverwritesAtomically(t *testing.T) {
	s, _ := newStore(t)
	key := hex64("deadbeef")
	if _, err := s.Save(key, bytes.NewReader([]byte("first")), 5); err != nil {
		t.Fatalf("Save first: %v", err)
	}
	if _, err := s.Save(key, bytes.NewReader([]byte("second-write")), 12); err != nil {
		t.Fatalf("Save second: %v", err)
	}
	obj, _ := s.Open(key)
	defer obj.Close()
	got, _ := io.ReadAll(obj)
	if string(got) != "second-write" {
		t.Fatalf("expected overwrite, got %q", got)
	}
}

func TestLocalStorage_SaveAcceptsUnknownSize(t *testing.T) {
	// Forgejo's Save contract: size=-1 means stream. LocalStorage ignores
	// the value so this is mostly a smoke test that the call signature
	// is honored.
	s, _ := newStore(t)
	key := hex64("feedface")
	body := strings.Repeat("x", 4096)
	n, err := s.Save(key, strings.NewReader(body), -1)
	if err != nil {
		t.Fatalf("Save -1: %v", err)
	}
	if n != int64(len(body)) {
		t.Fatalf("n: %d", n)
	}
}

func TestLocalStorage_URL_ReturnsErrURLNotSupported(t *testing.T) {
	s, _ := newStore(t)
	u, err := s.URL("anykey", "any.bin", url.Values{})
	if !errors.Is(err, storage.ErrURLNotSupported) {
		t.Fatalf("URL: got (%v, %v), want ErrURLNotSupported", u, err)
	}
	if u != nil {
		t.Fatalf("URL: expected nil URL on LocalStorage, got %v", u)
	}
}

func TestLocalStorage_IterateObjects(t *testing.T) {
	s, dir := newStore(t)
	want := map[string]string{
		hex64("11"): "alpha",
		hex64("22"): "bravo",
		hex64("33"): "charlie",
	}
	for k, v := range want {
		if _, err := s.Save(k, strings.NewReader(v), int64(len(v))); err != nil {
			t.Fatalf("Save %s: %v", k, err)
		}
	}

	seen := map[string]string{}
	err := s.IterateObjects("", func(path string, obj storage.Object) error {
		body, err := io.ReadAll(obj)
		if err != nil {
			return err
		}
		seen[path] = string(body)
		return nil
	})
	if err != nil {
		t.Fatalf("IterateObjects: %v", err)
	}
	if len(seen) != len(want) {
		t.Fatalf("expected %d objects, got %d (dir=%s):\n%+v", len(want), len(seen), dir, seen)
	}
	for k, v := range want {
		if got, ok := seen[k]; !ok || got != v {
			t.Errorf("missing or wrong: %s = %q (want %q)", k, got, v)
		}
	}

	// Sanity: stable ordering when sorted.
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	if len(paths) != 3 {
		t.Fatalf("paths: %+v", paths)
	}
}

func TestLocalStorage_IterateObjects_PropagatesCallbackError(t *testing.T) {
	s, _ := newStore(t)
	_, _ = s.Save(hex64("aa"), strings.NewReader("data"), 4)
	sentinel := errors.New("stop")
	err := s.IterateObjects("", func(path string, obj storage.Object) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel, got %v", err)
	}
}

func TestLocalStorage_IterateObjects_RespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()
	s, err := storage.NewLocalStorage(ctx, dir)
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}
	for i := 0; i < 4; i++ {
		k := hex64(string(rune('a' + i)))
		_, _ = s.Save(k, strings.NewReader("x"), 1)
	}
	cancel()
	err = s.IterateObjects("", func(path string, obj storage.Object) error {
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestLocalStorage_SaveSurvivesInterruptedWrite(t *testing.T) {
	// Atomic write contract: a reader that races with a Save must see
	// either the old bytes or the new bytes, never a half-written file.
	// We can't easily race in a unit test; instead we verify the on-disk
	// invariant that no .tmp-* files are left behind after a successful
	// Save.
	s, dir := newStore(t)
	key := hex64("55667788")
	if _, err := s.Save(key, strings.NewReader("body"), 4); err != nil {
		t.Fatalf("Save: %v", err)
	}
	var leftover []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Base(p)[:1] == "." {
			leftover = append(leftover, p)
		}
		return nil
	})
	if len(leftover) != 0 {
		t.Fatalf("leftover temp files: %v", leftover)
	}
}

func TestLocalStorage_OpenReturnsSeekableObject(t *testing.T) {
	// The Object contract embeds io.Seeker; verify it actually works
	// (S3-style backends will need to provide this too).
	s, _ := newStore(t)
	key := hex64("a1a2a3a4")
	body := []byte("0123456789")
	_, _ = s.Save(key, bytes.NewReader(body), int64(len(body)))

	obj, _ := s.Open(key)
	defer obj.Close()
	if _, err := obj.Seek(5, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got, _ := io.ReadAll(obj)
	if string(got) != "56789" {
		t.Fatalf("seeked read: got %q want %q", got, "56789")
	}
}
