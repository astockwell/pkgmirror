// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2021 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// This file is a transliteration of
// forgejo/modules/packages/nuget/metadata_test.go. The fixtures and
// test names mirror upstream so future cross-references stay legible.

package nuget

import (
	"archive/zip"
	"bytes"
	"errors"
	"testing"
)

const (
	id                = "System.Pkgmirror"
	title             = "Package Title"
	language          = "Package Language"
	semver            = "1.0.1"
	authors           = "pkgmirror Authors"
	owners            = "Package Owners"
	copyright         = "Package Copyright"
	projectURL        = "https://example.com"
	licenseURL        = "https://example.com/license"
	iconURL           = "https://example.com/icon.png"
	description       = "Package Description"
	releaseNotes      = "Package Release Notes"
	readme            = "Readme contents"
	tags              = "tag_1 tag_2 tag_3"
	minClientVersion  = "1.0.0.0"
	repositoryURL     = "https://example.com/repo"
	targetFramework   = ".NETStandard2.1"
	dependencyID      = "System.Text.Json"
	dependencyVersion = "5.0.0"
)

const nuspecContent = `<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://schemas.microsoft.com/packaging/2013/05/nuspec.xsd">
  <metadata minClientVersion="` + minClientVersion + `">
    <id>` + id + `</id>
    <title>` + title + `</title>
    <language>` + language + `</language>
    <version>` + semver + `</version>
    <authors>` + authors + `</authors>
    <owners>` + owners + `</owners>
    <copyright>` + copyright + `</copyright>
    <developmentDependency>true</developmentDependency>
    <requireLicenseAcceptance>true</requireLicenseAcceptance>
    <projectUrl>` + projectURL + `</projectUrl>
    <licenseUrl>` + licenseURL + `</licenseUrl>
    <iconUrl>` + iconURL + `</iconUrl>
    <description>` + description + `</description>
    <releaseNotes>` + releaseNotes + `</releaseNotes>
    <repository url="` + repositoryURL + `" />
    <readme>README.md</readme>
    <tags>` + tags + `</tags>
    <dependencies>
      <group targetFramework="` + targetFramework + `">
        <dependency id="` + dependencyID + `" version="` + dependencyVersion + `" exclude="Build,Analyzers" />
      </group>
    </dependencies>
  </metadata>
</package>`

const symbolsNuspecContent = `<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://schemas.microsoft.com/packaging/2013/05/nuspec.xsd">
  <metadata>
    <id>` + id + `</id>
    <version>` + semver + `</version>
    <description>` + description + `</description>
    <packageTypes>
      <packageType name="SymbolsPackage" />
    </packageTypes>
    <dependencies>
      <group targetFramework="` + targetFramework + `" />
    </dependencies>
  </metadata>
</package>`

func createArchive(files map[string]string) []byte {
	var buf bytes.Buffer
	archive := zip.NewWriter(&buf)
	for name, content := range files {
		w, _ := archive.Create(name)
		_, _ = w.Write([]byte(content))
	}
	_ = archive.Close()
	return buf.Bytes()
}

func TestParsePackage_MissingNuspecFile(t *testing.T) {
	data := createArchive(map[string]string{"dummy.txt": ""})
	np, err := ParsePackage(bytes.NewReader(data), int64(len(data)))
	if np != nil {
		t.Fatalf("expected nil package, got %+v", np)
	}
	if !errors.Is(err, ErrMissingNuspecFile) {
		t.Fatalf("expected ErrMissingNuspecFile, got %v", err)
	}
}

func TestParsePackage_NuspecOutsideRoot(t *testing.T) {
	data := createArchive(map[string]string{"sub/package.nuspec": ""})
	np, err := ParsePackage(bytes.NewReader(data), int64(len(data)))
	if np != nil {
		t.Fatalf("expected nil package, got %+v", np)
	}
	if !errors.Is(err, ErrMissingNuspecFile) {
		t.Fatalf("expected ErrMissingNuspecFile, got %v", err)
	}
}

func TestParsePackage_InvalidXML(t *testing.T) {
	data := createArchive(map[string]string{"package.nuspec": ""})
	np, err := ParsePackage(bytes.NewReader(data), int64(len(data)))
	if np != nil {
		t.Fatalf("expected nil package, got %+v", np)
	}
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestParsePackage_InvalidPackageId(t *testing.T) {
	data := createArchive(map[string]string{"package.nuspec": `<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://schemas.microsoft.com/packaging/2013/05/nuspec.xsd">
  <metadata></metadata>
</package>`})
	_, err := ParsePackage(bytes.NewReader(data), int64(len(data)))
	if !errors.Is(err, ErrNuspecInvalidID) {
		t.Fatalf("expected ErrNuspecInvalidID, got %v", err)
	}
}

func TestParsePackage_InvalidPackageVersion(t *testing.T) {
	data := createArchive(map[string]string{"package.nuspec": `<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://schemas.microsoft.com/packaging/2013/05/nuspec.xsd">
  <metadata>
    <id>` + id + `</id>
  </metadata>
</package>`})
	_, err := ParsePackage(bytes.NewReader(data), int64(len(data)))
	if !errors.Is(err, ErrNuspecInvalidVersion) {
		t.Fatalf("expected ErrNuspecInvalidVersion, got %v", err)
	}
}

func TestParsePackage_MissingReadme(t *testing.T) {
	data := createArchive(map[string]string{"package.nuspec": nuspecContent})
	np, err := ParsePackage(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if np.Metadata.Readme != "" {
		t.Fatalf("expected empty readme, got %q", np.Metadata.Readme)
	}
}

func TestParsePackage_DependencyPackage(t *testing.T) {
	data := createArchive(map[string]string{
		"package.nuspec": nuspecContent,
		"README.md":      readme,
	})
	np, err := ParsePackage(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if np.PackageType != DependencyPackage {
		t.Fatalf("expected DependencyPackage, got %v", np.PackageType)
	}
	if np.ID != id {
		t.Fatalf("ID: want %q got %q", id, np.ID)
	}
	if np.Metadata.Title != title {
		t.Fatalf("Title: want %q got %q", title, np.Metadata.Title)
	}
	if np.Metadata.Language != language {
		t.Fatalf("Language: want %q got %q", language, np.Metadata.Language)
	}
	if np.Version != semver {
		t.Fatalf("Version: want %q got %q", semver, np.Version)
	}
	if np.Metadata.Authors != authors {
		t.Fatalf("Authors: %q", np.Metadata.Authors)
	}
	if np.Metadata.Owners != owners {
		t.Fatalf("Owners: %q", np.Metadata.Owners)
	}
	if np.Metadata.Copyright != copyright {
		t.Fatalf("Copyright: %q", np.Metadata.Copyright)
	}
	if !np.Metadata.DevelopmentDependency {
		t.Fatal("DevelopmentDependency should be true")
	}
	if !np.Metadata.RequireLicenseAcceptance {
		t.Fatal("RequireLicenseAcceptance should be true")
	}
	if np.Metadata.ProjectURL != projectURL {
		t.Fatalf("ProjectURL: %q", np.Metadata.ProjectURL)
	}
	if np.Metadata.LicenseURL != licenseURL {
		t.Fatalf("LicenseURL: %q", np.Metadata.LicenseURL)
	}
	if np.Metadata.IconURL != iconURL {
		t.Fatalf("IconURL: %q", np.Metadata.IconURL)
	}
	if np.Metadata.Description != description {
		t.Fatalf("Description: %q", np.Metadata.Description)
	}
	if np.Metadata.ReleaseNotes != releaseNotes {
		t.Fatalf("ReleaseNotes: %q", np.Metadata.ReleaseNotes)
	}
	if np.Metadata.Readme != readme {
		t.Fatalf("Readme: %q", np.Metadata.Readme)
	}
	if np.Metadata.Tags != tags {
		t.Fatalf("Tags: %q", np.Metadata.Tags)
	}
	if np.Metadata.MinClientVersion != minClientVersion {
		t.Fatalf("MinClientVersion: %q", np.Metadata.MinClientVersion)
	}
	if np.Metadata.RepositoryURL != repositoryURL {
		t.Fatalf("RepositoryURL: %q", np.Metadata.RepositoryURL)
	}
	deps, ok := np.Metadata.Dependencies[targetFramework]
	if !ok {
		t.Fatalf("expected dependency group %q in %v", targetFramework, np.Metadata.Dependencies)
	}
	if len(deps) != 1 || deps[0].ID != dependencyID || deps[0].Version != dependencyVersion {
		t.Fatalf("dependencies wrong: %+v", deps)
	}
	if np.NuspecContent == nil || np.NuspecContent.Len() == 0 {
		t.Fatal("NuspecContent should be populated")
	}
}

func TestParsePackage_NormalizedVersion(t *testing.T) {
	// NuGet normalizes 4-segment versions: trailing zeros dropped on
	// segment 4, build-metadata stripped, pre-release preserved.
	data := createArchive(map[string]string{"package.nuspec": `<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://schemas.microsoft.com/packaging/2013/05/nuspec.xsd">
  <metadata>
    <id>test</id>
    <version>1.04.5.2.5-rc.1+metadata</version>
  </metadata>
</package>`})
	np, err := ParsePackage(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if np.Version != "1.4.5.2-rc.1" {
		t.Fatalf("normalized version: want %q got %q", "1.4.5.2-rc.1", np.Version)
	}
}

func TestParsePackage_SymbolsPackage(t *testing.T) {
	data := createArchive(map[string]string{"package.nuspec": symbolsNuspecContent})
	np, err := ParsePackage(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if np.PackageType != SymbolsPackage {
		t.Fatalf("expected SymbolsPackage, got %v", np.PackageType)
	}
	if np.ID != id {
		t.Fatalf("ID: %q", np.ID)
	}
	if np.Version != semver {
		t.Fatalf("Version: %q", np.Version)
	}
	if len(np.Metadata.Dependencies) != 0 {
		t.Fatalf("expected no dependencies, got %+v", np.Metadata.Dependencies)
	}
}

func TestParsePackage_InvalidProjectURLDropped(t *testing.T) {
	data := createArchive(map[string]string{"package.nuspec": `<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://schemas.microsoft.com/packaging/2013/05/nuspec.xsd">
  <metadata>
    <id>` + id + `</id>
    <version>` + semver + `</version>
    <description>x</description>
    <projectUrl>not a url</projectUrl>
  </metadata>
</package>`})
	np, err := ParsePackage(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if np.Metadata.ProjectURL != "" {
		t.Fatalf("expected ProjectURL dropped, got %q", np.Metadata.ProjectURL)
	}
}
