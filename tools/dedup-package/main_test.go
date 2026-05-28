// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const cleanFile = `// Copyright 2026 example
// SPDX-License-Identifier: MIT

package syncutil

import "sync"

func F() { _ = sync.Mutex{} }
`

// duplicatedFile is exactly the shape we keep observing in the wild:
// the auto-formatter prepends a shorter header + a second `package`
// line right after the legitimate one.
const duplicatedFile = `// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2016 The Gogs Authors.
// SPDX-License-Identifier: MIT
//
// Ported from upstream.

package syncutil
// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// SPDX-License-Identifier: MIT

package syncutil

import "sync"

func F() { _ = sync.Mutex{} }
`

// expectedAfterFix is what the fixer should produce from
// duplicatedFile.
const expectedAfterFix = `// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2016 The Gogs Authors.
// SPDX-License-Identifier: MIT
//
// Ported from upstream.

package syncutil

import "sync"

func F() { _ = sync.Mutex{} }
`

// ambiguousFile has two `package` lines with REAL code between them.
// The fixer must refuse rather than silently delete a function.
const ambiguousFile = `package foo

func realCode() {}

package foo

import "sync"
var _ = sync.Mutex{}
`

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckOrFix_Clean(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "clean.go", cleanFile)
	didFix, hadDup, err := checkOrFix(p, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hadDup || didFix {
		t.Fatalf("clean file flagged: hadDup=%v didFix=%v", hadDup, didFix)
	}
	got, _ := os.ReadFile(p)
	if string(got) != cleanFile {
		t.Fatalf("clean file modified:\n--- got ---\n%s\n--- want ---\n%s", got, cleanFile)
	}
}

func TestCheckOrFix_DetectsDuplicate(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "dup.go", duplicatedFile)
	_, hadDup, err := checkOrFix(p, false /* check only */)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hadDup {
		t.Fatal("duplicate not detected")
	}
	got, _ := os.ReadFile(p)
	if string(got) != duplicatedFile {
		t.Fatalf("file modified in check-only mode")
	}
}

func TestCheckOrFix_FixesDuplicate(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "dup.go", duplicatedFile)
	didFix, hadDup, err := checkOrFix(p, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hadDup || !didFix {
		t.Fatalf("expected hadDup=true didFix=true, got hadDup=%v didFix=%v", hadDup, didFix)
	}
	got, _ := os.ReadFile(p)
	if string(got) != expectedAfterFix {
		t.Fatalf("fix produced wrong output:\n--- got ---\n%s\n--- want ---\n%s", got, expectedAfterFix)
	}
	// Idempotency: running fix again on the fixed file is a no-op.
	didFix, hadDup, err = checkOrFix(p, true)
	if err != nil || hadDup || didFix {
		t.Fatalf("second run modified file: err=%v hadDup=%v didFix=%v", err, hadDup, didFix)
	}
}

func TestCheckOrFix_RefusesAmbiguous(t *testing.T) {
	// When the span between two `package` lines contains real code
	// rather than just comments + blanks, the fixer must refuse —
	// we'd be deleting code otherwise. Surfacing as an error lets a
	// human decide.
	dir := t.TempDir()
	p := write(t, dir, "amb.go", ambiguousFile)
	_, hadDup, err := checkOrFix(p, true)
	if err == nil {
		t.Fatal("expected error on ambiguous duplicate")
	}
	if !hadDup {
		t.Fatal("ambiguous file should still report hadDup")
	}
	if !strings.Contains(err.Error(), "unexpected content") {
		t.Fatalf("error should mention unexpected content, got: %v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != ambiguousFile {
		t.Fatal("ambiguous file was modified despite refusal")
	}
}
