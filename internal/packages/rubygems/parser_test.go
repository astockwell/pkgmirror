// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2021 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// Ported from forgejo/modules/packages/rubygems/metadata_test.go (MIT).
// The base64 fixtures are upstream-built .gem inner metadata.gz blobs;
// they exercise the gemspec YAML parser including pessimistic version
// constraints ("~> 5.2"), dev/runtime dependency split, and the
// missing-metadata error path.

package rubygems

import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"testing"
)

func makeGemArchive(t *testing.T, filename string, content []byte) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: filename, Mode: 0o600, Size: int64(len(content))}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return &buf
}

func TestParsePackageMetaData_MissingMetadataFile(t *testing.T) {
	data := makeGemArchive(t, "dummy.txt", []byte{0})
	rp, err := ParsePackageMetaData(data)
	if !errors.Is(err, ErrMissingMetadataFile) {
		t.Fatalf("err=%v want ErrMissingMetadataFile", err)
	}
	if rp != nil {
		t.Fatalf("rp = %+v want nil", rp)
	}
}

func TestParsePackageMetaData_ValidShape(t *testing.T) {
	// Minimal valid metadata.gz from upstream test — just confirms the
	// happy-path archive walk + gzip + YAML decode all link up.
	content, _ := base64.StdEncoding.DecodeString("H4sICHC/I2EEAG1ldGFkYXRhAAEeAOH/bmFtZTogZwp2ZXJzaW9uOgogIHZlcnNpb246IDEKWw35Tx4AAAA=")
	data := makeGemArchive(t, "metadata.gz", content)
	rp, err := ParsePackageMetaData(data)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if rp == nil || rp.Name != "g" || rp.Version != "1" {
		t.Fatalf("rp = %+v", rp)
	}
}

// TestParseMetadataFile is the upstream "Gitea package" fixture — a full
// gemspec round-trip with runtime + dev dependencies, required_ruby_version,
// licenses, and project URL.
func TestParseMetadataFile_FullFixture(t *testing.T) {
	content, _ := base64.StdEncoding.DecodeString(`H4sIAMe7I2ECA9VVTW/UMBC9+1eYXvaUbJpSQBZUHJAqDlwK4kCFIseZzZrGH9iTqisEv52Js9nd
0KqggiqRXWnX45n3ZuZ5nCzL+JPQ15ulq7+AQnEORoj3HpReaSVRO8usNCB4qxEku4YQySbuCPo4
bjHOd07HeZGfMt9JXLlgBB9imOxx7UIULOPnCZMMLsDXXgeiYbW2jQ6C0y9TELBSa6kJ6/IzaySS
R1mUx1nxIitPeFGI9M2L6eGfWAMebANWaUgktzN9M3lsKNmxutBb1AYyCibbNhsDFu+q9GK/Tc4z
d2IcLBl9js5eHaXFsLyvXeNz0LQyL/YoLx8EsiCMBZlx46k6sS2PDD5AgA5kJPNKdhH2elWzOv7n
uv9Q9Aau/6ngP84elvNpXh5oRVlB5/yW7BH0+qu0G4gqaI/JdEHBFBS5l+pKtsARIjIwUnfj8Le0
+TrdJLl2DG5A9SjrjgZ1mG+4QbAD+G4ZZBUap6qVnnzGf6Rwp+vliBRqtnYGPBEKvkb0USyXE8mS
dVoR6hj07u0HZgAl3SRS8G/fmXcRK20jyq6rDMSYQFgidamqkXbbuspLXE/0k7GphtKqe67GuRC/
yjAbmt9LsOMp8xMamFkSQ38fP5EFjdz8LA4do2C69VvqWXAJgrPbKZb58/xZXrKoW6ttW13Bhvzi
4ftn7/yUxd4YGcglvTmmY8aGY3ZwRn4CqcWcidUGAAA=`)
	rp, err := parseMetadataFile(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if rp == nil {
		t.Fatal("rp nil")
	}
	if got, want := rp.Name, "gitea"; got != want {
		t.Errorf("Name = %q, want %q", got, want)
	}
	if got, want := rp.Version, "1.0.5"; got != want {
		t.Errorf("Version = %q, want %q", got, want)
	}
	m := rp.Metadata
	if got, want := m.Platform, "ruby"; got != want {
		t.Errorf("Platform = %q, want %q", got, want)
	}
	if got, want := m.Summary, "Gitea package"; got != want {
		t.Errorf("Summary = %q, want %q", got, want)
	}
	if got, want := m.Description, "RubyGems package test"; got != want {
		t.Errorf("Description = %q, want %q", got, want)
	}
	if got, want := m.ProjectURL, "https://gitea.io/"; got != want {
		t.Errorf("ProjectURL = %q, want %q", got, want)
	}
	if got := m.Licenses; len(got) != 1 || got[0] != "MIT" {
		t.Errorf("Licenses = %v, want [MIT]", got)
	}
	if got := m.RequiredRubyVersion; len(got) != 1 || got[0].Restriction != ">=" || got[0].Version != "2.3.0" {
		t.Errorf("RequiredRubyVersion = %+v", got)
	}
	if got := m.RuntimeDependencies; len(got) != 1 || got[0].Name != "runtime-dep" || len(got[0].Version) != 2 {
		t.Errorf("RuntimeDependencies = %+v", got)
	} else {
		if got[0].Version[0].Restriction != ">=" || got[0].Version[0].Version != "1.2.0" {
			t.Errorf("RuntimeDeps[0].Version[0] = %+v", got[0].Version[0])
		}
		if got[0].Version[1].Restriction != "<" || got[0].Version[1].Version != "2.0" {
			t.Errorf("RuntimeDeps[0].Version[1] = %+v", got[0].Version[1])
		}
	}
	if got := m.DevelopmentDependencies; len(got) != 1 || got[0].Name != "dev-dep" {
		t.Errorf("DevelopmentDependencies = %+v", got)
	} else if got[0].Version[0].Restriction != "~>" || got[0].Version[0].Version != "5.2" {
		t.Errorf("DevDeps[0].Version = %+v", got[0].Version)
	}
}

func TestFullFilename(t *testing.T) {
	cases := []struct {
		name, version, platform string
		want                    string
	}{
		{"foo", "1.0.0", "ruby", "foo-1.0.0.gem"},
		{"Foo", "1.0.0", "", "foo-1.0.0.gem"},
		{"foo", "1.0.0", "x86_64-linux", "foo-1.0.0-x86_64-linux.gem"},
	}
	for _, c := range cases {
		got := FullFilename(c.name, c.version, c.platform)
		if got != c.want {
			t.Errorf("FullFilename(%q,%q,%q) = %q, want %q", c.name, c.version, c.platform, got, c.want)
		}
	}
}
