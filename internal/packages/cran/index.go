// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2023 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// PACKAGES index builder. The on-wire format is the Debian-control-
// style document R's `available.packages()` and `install.packages()`
// parse — one paragraph per package, fields separated by `field:
// value` lines, paragraphs separated by a blank line. Modeled on
// the index emission loop in
// forgejo/routers/api/packages/cran/cran.go (MIT).
//
// Differences from upstream:
//
//   - The loop writes into a *bytes.Buffer so the handler can
//     synthesize either the plain or gzip variant from the same
//     bytes (matches our on-demand stable-output posture).
//   - Field order matches what `tools::write_PACKAGES()` emits:
//     Package, Version, Depends, Imports, LinkingTo, Suggests,
//     License, NeedsCompilation, MD5sum. R parses any field order
//     but stable output makes the test assertions trivial.

package cran

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"strings"
)

// buildPackagesIndex renders one entry per package as a Debian
// control-file paragraph. Empty fields are skipped to match
// upstream behavior and reduce wire bloat.
func buildPackagesIndex(entries []*indexEntry) []byte {
	var buf bytes.Buffer
	for i, e := range entries {
		if i > 0 {
			fmt.Fprintln(&buf)
		}
		fmt.Fprintln(&buf, "Package:", e.Name)
		fmt.Fprintln(&buf, "Version:", e.Version)
		if e.Metadata != nil {
			if len(e.Metadata.Depends) > 0 {
				fmt.Fprintln(&buf, "Depends:", strings.Join(e.Metadata.Depends, ", "))
			}
			if len(e.Metadata.Imports) > 0 {
				fmt.Fprintln(&buf, "Imports:", strings.Join(e.Metadata.Imports, ", "))
			}
			if len(e.Metadata.LinkingTo) > 0 {
				fmt.Fprintln(&buf, "LinkingTo:", strings.Join(e.Metadata.LinkingTo, ", "))
			}
			if len(e.Metadata.Suggests) > 0 {
				fmt.Fprintln(&buf, "Suggests:", strings.Join(e.Metadata.Suggests, ", "))
			}
			if e.Metadata.License != "" {
				fmt.Fprintln(&buf, "License:", e.Metadata.License)
			}
			needs := "no"
			if e.Metadata.NeedsCompilation {
				needs = "yes"
			}
			fmt.Fprintln(&buf, "NeedsCompilation:", needs)
		} else {
			fmt.Fprintln(&buf, "NeedsCompilation: no")
		}
		// MD5sum is what R verifies post-download; it's load-bearing
		// for `install.packages` to consider the file intact.
		fmt.Fprintln(&buf, "MD5sum:", e.MD5)
	}
	return buf.Bytes()
}

// gzipBytes wraps body in a gzip stream. Used by the .gz variants of
// the PACKAGES route.
func gzipBytes(body []byte) []byte {
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	_, _ = gzw.Write(body)
	_ = gzw.Close()
	return buf.Bytes()
}
