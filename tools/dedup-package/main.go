// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// SPDX-License-Identifier: MIT
//
// dedup-package detects (and optionally fixes) Go source files that
// contain a duplicate top-level `package X` declaration. Some
// auto-formatter / paste handler in the local dev environment
// occasionally re-prepends an MIT header + a second `package X` line
// inside a file that already has one, producing a non-compilable
// state like:
//
//	// <original header>
//	package syncutil
//	// <duplicated header fragment>
//	package syncutil   <-- bad
//	import (
//	  ...
//	)
//
// We can't pin down which tool causes it (probably a VS Code Go
// extension misfire), so instead of chasing the trigger we guard the
// repository: this checker runs in `make verify` and CI before
// `go vet`, so a duplicate declaration never reaches a green build.
// Run with -fix to remove the spurious block in place.
//
// Usage:
//
//	dedup-package [-fix] <root>
//
// Walks `root` (default ".") recursively, examining every `.go`
// file tracked by the working tree. Skips the vendor/ tree.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	fix := flag.Bool("fix", false, "rewrite affected files in place")
	flag.Parse()

	root := "."
	if flag.NArg() > 0 {
		root = flag.Arg(0)
	}

	var bad []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Skip vendor/, .git/, node_modules/, tests/blackbox/harness's
		// downloaded artifacts — anything we don't author directly.
		if d.IsDir() {
			name := d.Name()
			if name == "vendor" || name == ".git" || name == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		fixed, hadDup, err := checkOrFix(path, *fix)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if hadDup {
			bad = append(bad, path)
			if *fix && fixed {
				fmt.Fprintf(os.Stderr, "fixed: %s\n", path)
			} else {
				fmt.Fprintln(os.Stderr, path)
			}
		}
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "walk:", err)
		os.Exit(2)
	}
	if len(bad) > 0 && !*fix {
		fmt.Fprintf(os.Stderr,
			"\n%d file(s) contain duplicate `package` declarations.\n"+
				"Run `make fix-package-dupes` to repair them.\n", len(bad))
		os.Exit(1)
	}
}

// checkOrFix returns (didFix, hadDup, err).
//
// The duplicate pattern looks like:
//
//	line a:  package X         <-- the legitimate one
//	  ...    (blank + comment lines, no other code)
//	line b:  package X         <-- the spurious one
//
// To "fix," we delete every line from the line after `a` through and
// including `b`, but only when the deleted span contains nothing
// other than blank lines and `//`-comments. That guard protects
// against the unlikely case of a hand-written file with two real
// `package` lines separated by something more substantive — in which
// case we error out rather than risk eating real code.
func checkOrFix(path string, fix bool) (didFix, hadDup bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, false, err
	}

	// Find every real top-level PACKAGE token using go/scanner —
	// regex-on-raw-bytes would also match `package X` inside string
	// literals (this very file's test fixtures are an example). We
	// only care about lexer-level token positions.
	pkgLines, err := scanPackageLines(path, data)
	if err != nil {
		return false, false, err
	}
	if len(pkgLines) < 2 {
		return false, false, nil
	}

	lines := bytes.Split(data, []byte("\n"))

	// Convert 1-based line numbers from go/scanner to 0-based
	// indices into `lines`.
	for i := range pkgLines {
		pkgLines[i]--
	}

	// Validate: every line between the first `package` and the last
	// one must be blank or a `//` comment. If anything else is in
	// the span we refuse to auto-fix; surface as an error so a human
	// inspects it.
	first := pkgLines[0]
	last := pkgLines[len(pkgLines)-1]
	for i := first + 1; i < last; i++ {
		t := bytes.TrimSpace(lines[i])
		if len(t) == 0 {
			continue
		}
		if bytes.HasPrefix(t, []byte("//")) {
			continue
		}
		if i == last {
			continue // the final `package` line itself
		}
		// Allow another `package` line that's between the first and
		// last (we'll collapse all of them).
		if bytes.HasPrefix(t, []byte("package ")) {
			continue
		}
		return false, true, fmt.Errorf("unexpected content between duplicate package lines at line %d: %q", i+1, lines[i])
	}

	if !fix {
		return false, true, nil
	}

	// Rewrite: keep lines [0..first], skip (first+1..last], keep
	// (last+1..end). That collapses the span between the two
	// `package` lines back to a single one, deleting the spurious
	// header fragment and any blank padding.
	out := make([][]byte, 0, len(lines)-(last-first))
	out = append(out, lines[:first+1]...)
	out = append(out, lines[last+1:]...)
	body := bytes.Join(out, []byte("\n"))
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return false, true, err
	}
	return true, true, nil
}

// scanPackageLines returns the 1-based line numbers of every PACKAGE
// token at file scope. go/scanner runs in tolerant mode (errors
// recorded but not fatal), so even a file that won't compile because
// of the duplicate declaration still tokenizes cleanly.
func scanPackageLines(path string, src []byte) ([]int, error) {
	fset := token.NewFileSet()
	f := fset.AddFile(path, fset.Base(), len(src))
	var s scanner.Scanner
	// Ignore scanner errors; we only care about token positions.
	s.Init(f, src, func(pos token.Position, msg string) {}, scanner.ScanComments)

	var lines []int
	for {
		pos, tok, _ := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.PACKAGE {
			lines = append(lines, fset.Position(pos).Line)
		}
	}
	return lines, nil
}
