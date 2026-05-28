//go:build blackbox

package pypi_blackbox_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

// buildWheel constructs a minimal PEP 427 universal wheel
// (foo-<version>-py3-none-any.whl) for a single-module Python package.
// Wheels are pre-built and need no build backend, so they install cleanly
// in stripped-down python images that lack setuptools.
//
// Layout:
//
//	<name>/__init__.py
//	<name>-<version>.dist-info/METADATA
//	<name>-<version>.dist-info/WHEEL
//	<name>-<version>.dist-info/RECORD
func buildWheel(t *testing.T, name, version string) []byte {
	t.Helper()
	distInfo := fmt.Sprintf("%s-%s.dist-info", name, version)

	files := []struct {
		path    string
		content []byte
	}{
		{
			path: name + "/__init__.py",
			content: []byte(fmt.Sprintf(
				"NAME = %q\n\ndef greet():\n    return \"hello from \" + NAME\n", name)),
		},
		{
			path: distInfo + "/METADATA",
			content: []byte(fmt.Sprintf(
				"Metadata-Version: 2.1\nName: %s\nVersion: %s\nSummary: blackbox fixture\n",
				name, version)),
		},
		{
			path: distInfo + "/WHEEL",
			content: []byte(
				"Wheel-Version: 1.0\nGenerator: pkgmirror-blackbox\nRoot-Is-Purelib: true\nTag: py3-none-any\n"),
		},
	}

	// Build the RECORD content — each non-RECORD file contributes a
	// "path,sha256=<unpadded base64>,<size>" line. RECORD itself has
	// empty hash/size by convention.
	var rec strings.Builder
	for _, f := range files {
		sum := sha256.Sum256(f.content)
		hash := strings.TrimRight(base64.URLEncoding.EncodeToString(sum[:]), "=")
		fmt.Fprintf(&rec, "%s,sha256=%s,%d\n", f.path, hash, len(f.content))
	}
	fmt.Fprintf(&rec, "%s/RECORD,,\n", distInfo)

	files = append(files, struct {
		path    string
		content []byte
	}{
		path:    distInfo + "/RECORD",
		content: []byte(rec.String()),
	})

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		w, err := zw.Create(f.path)
		if err != nil {
			t.Fatalf("zip create %q: %v", f.path, err)
		}
		if _, err := w.Write(f.content); err != nil {
			t.Fatalf("zip write %q: %v", f.path, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}
