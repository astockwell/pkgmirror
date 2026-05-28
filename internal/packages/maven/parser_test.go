// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2021 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// Ported from forgejo/modules/packages/maven/metadata_test.go (MIT).

package maven

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/encoding/charmap"
)

const (
	groupID              = "org.gitea"
	parentGroupID        = "org.gitea.parent"
	artifactID           = "my-project"
	versionStr           = "1.0.1"
	nameStr              = "My Gitea Project"
	descriptionStr       = "Package Description"
	projectURL           = "https://gitea.io"
	licenseName          = "MIT"
	dependencyGroupID    = "org.gitea.core"
	dependencyArtifactID = "git"
	dependencyVersion    = "5.0.0"
)

const pomContent = `<?xml version="1.0"?>
<project xsi:schemaLocation="http://maven.apache.org/POM/4.0.0 http://maven.apache.org/xsd/maven-4.0.0.xsd">
  <parent>
    <groupId>` + parentGroupID + `</groupId>
    <artifactId>parent-project</artifactId>
    <version>1.0.0</version>
  </parent>
  <groupId>` + groupID + `</groupId>
  <artifactId>` + artifactID + `</artifactId>
  <version>` + versionStr + `</version>
  <name>` + nameStr + `</name>
  <description>` + descriptionStr + `</description>
  <url>` + projectURL + `</url>
  <licenses>
    <license>
      <name>` + licenseName + `</name>
    </license>
  </licenses>
  <dependencies>
    <dependency>
      <groupId>` + dependencyGroupID + `</groupId>
      <artifactId>` + dependencyArtifactID + `</artifactId>
      <version>` + dependencyVersion + `</version>
    </dependency>
  </dependencies>
</project>`

const pomWithParentGroupID = `<?xml version="1.0"?>
<project xsi:schemaLocation="http://maven.apache.org/POM/4.0.0 http://maven.apache.org/xsd/maven-4.0.0.xsd">
  <parent>
    <groupId>` + parentGroupID + `</groupId>
    <artifactId>parent-project</artifactId>
    <version>1.0.0</version>
  </parent>

  <artifactId>` + artifactID + `</artifactId>
  <version>` + versionStr + `</version>
</project>`

const pomWithMissingGroupID = `<?xml version="1.0"?>
<project xsi:schemaLocation="http://maven.apache.org/POM/4.0.0 http://maven.apache.org/xsd/maven-4.0.0.xsd">
  <artifactId>` + artifactID + `</artifactId>
  <version>` + versionStr + `</version>
</project>`

func TestParsePackageMetaData(t *testing.T) {
	t.Run("InvalidFile", func(t *testing.T) {
		m, err := ParsePackageMetaData(strings.NewReader(""))
		assert.Nil(t, m)
		require.Error(t, err)
	})

	t.Run("Valid", func(t *testing.T) {
		m, err := ParsePackageMetaData(strings.NewReader(pomContent))
		require.NoError(t, err)
		require.NotNil(t, m)

		assert.Equal(t, groupID, m.GroupID)
		assert.Equal(t, artifactID, m.ArtifactID)
		assert.Equal(t, nameStr, m.Name)
		assert.Equal(t, descriptionStr, m.Description)
		assert.Equal(t, projectURL, m.ProjectURL)
		assert.Equal(t, []string{licenseName}, m.Licenses)
		assert.Len(t, m.Dependencies, 1)
		assert.Equal(t, dependencyGroupID, m.Dependencies[0].GroupID)
		assert.Equal(t, dependencyArtifactID, m.Dependencies[0].ArtifactID)
		assert.Equal(t, dependencyVersion, m.Dependencies[0].Version)
	})

	t.Run("Encoding", func(t *testing.T) {
		// POMs in the wild ship in many character encodings — Maven
		// Central allows ISO-8859-1, Windows-1252, etc. The XML
		// decoder's CharsetReader is what makes this work; without
		// it golang.org/x/net/html/charset, the decode fails on the
		// first non-ASCII byte.
		pomContent8859, err := charmap.ISO8859_1.NewEncoder().String(
			strings.ReplaceAll(
				pomContent,
				`<?xml version="1.0"?>`,
				`<?xml version="1.0" encoding="ISO-8859-1"?>`,
			),
		)
		require.NoError(t, err)

		m, err := ParsePackageMetaData(strings.NewReader(pomContent8859))
		require.NoError(t, err)
		require.NotNil(t, m)
	})

	t.Run("UseParentGroupID", func(t *testing.T) {
		// When the POM omits its own <groupId>, the spec says
		// inherit from <parent><groupId>. Multi-module Maven
		// projects rely on this constantly.
		m, err := ParsePackageMetaData(strings.NewReader(pomWithParentGroupID))
		require.NoError(t, err)
		require.NotNil(t, m)
		assert.Equal(t, parentGroupID, m.GroupID)
	})

	t.Run("MissingGroupIDNoParent", func(t *testing.T) {
		m, err := ParsePackageMetaData(strings.NewReader(pomWithMissingGroupID))
		assert.Nil(t, m)
		require.ErrorIs(t, err, ErrNoGroupID)
	})

	t.Run("InvalidProjectURLIsDropped", func(t *testing.T) {
		// Same hygiene as the alpine + pypi parsers: a non-http
		// URL in <url> is silently cleared rather than failing the
		// upload. We've seen "TODO" and "n/a" and bare hostnames
		// in real-world POMs.
		raw := strings.Replace(pomContent, projectURL, "not-a-url", 1)
		m, err := ParsePackageMetaData(strings.NewReader(raw))
		require.NoError(t, err)
		assert.Empty(t, m.ProjectURL)
	})
}
