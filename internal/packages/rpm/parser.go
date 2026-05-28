// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2023 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// RPM binary metadata parser. Direct transliteration of
// forgejo/modules/packages/rpm/metadata.go (MIT) with three
// adjustments:
//
//  1. We import github.com/sassoftware/go-rpmutils directly (upstream)
//     instead of forgejo's `code.forgejo.org/forgejo/go-rpmutils` fork
//     — the fork's only addition we care about is RPM signing, which
//     we don't expose for MVP.
//  2. URL validation uses stdlib net/url instead of forgejo's
//     modules/validation package.
//  3. The `repoType` parameter is dropped: pkgmirror only supports
//     standard RPM, not the ALT-Linux variant.
//
// The on-disk RPM format (lead + signature header + main header +
// payload), the NEVRA decomposition, and the per-tag extraction
// (PROVIDENAME / REQUIRENAME / FILES / CHANGELOG / etc.) are
// preserved byte-shape compatible with what real dnf and yum expect
// to find inside primary.xml.

package rpm

import (
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/sassoftware/go-rpmutils"
)

// Property and pseudo-package names. Match forgejo's wire convention
// so an operator inspecting the DB recognizes them.
const (
	PropertyMetadata     = "rpm.metadata"
	PropertyGroup        = "rpm.group"
	PropertyArchitecture = "rpm.architecture"

	SettingKeyPrivate = "rpm.key.private"
	SettingKeyPublic  = "rpm.key.public"

	// RepositoryPackage is the synthetic package name used to anchor
	// per-tenant RPM repo metadata (GPG signing keys). Matches
	// forgejo's `_rpm` convention.
	RepositoryPackage = "_rpm"
	RepositoryVersion = "_repository"
)

// File-mode constants. We can't use syscall versions because they
// vary by platform; the upstream metadata.go hardcodes the same
// POSIX values so we copy them verbatim.
const (
	sIFMT  = 0xf000
	sIFDIR = 0x4000
	sIXUSR = 0x40
	sIXGRP = 0x8
	sIXOTH = 0x1
)

// Package is the parsed shape of a single .rpm upload.
type Package struct {
	Name            string
	Version         string
	VersionMetadata *VersionMetadata
	FileMetadata    *FileMetadata
}

// VersionMetadata is per (name, version): values pinned by package
// identity, shared across architectural builds. Stored on the
// package_versions.metadata_json column.
type VersionMetadata struct {
	License     string `json:"license,omitempty"`
	ProjectURL  string `json:"project_url,omitempty"`
	Summary     string `json:"summary,omitempty"`
	Description string `json:"description,omitempty"`
}

// FileMetadata is per individual .rpm file (varies by arch / build).
// Stored as the `rpm.metadata` file-level property in JSON form so
// the index builder can reproduce primary.xml / filelists.xml /
// other.xml without re-reading the .rpm blob.
type FileMetadata struct {
	Architecture  string `json:"architecture,omitempty"`
	Epoch         string `json:"epoch,omitempty"`
	Version       string `json:"version,omitempty"`
	Release       string `json:"release,omitempty"`
	Vendor        string `json:"vendor,omitempty"`
	Group         string `json:"group,omitempty"`
	Packager      string `json:"packager,omitempty"`
	SourceRpm     string `json:"source_rpm,omitempty"`
	BuildHost     string `json:"build_host,omitempty"`
	BuildTime     uint64 `json:"build_time,omitempty"`
	FileTime      uint64 `json:"file_time,omitempty"`
	InstalledSize uint64 `json:"installed_size,omitempty"`
	ArchiveSize   uint64 `json:"archive_size,omitempty"`

	Provides  []*Entry `json:"provide,omitempty"`
	Requires  []*Entry `json:"require,omitempty"`
	Conflicts []*Entry `json:"conflict,omitempty"`
	Obsoletes []*Entry `json:"obsolete,omitempty"`

	Files []*File `json:"files,omitempty"`

	Changelogs []*Changelog `json:"changelogs,omitempty"`
}

// Entry is one dependency / provides / conflicts row. XML tags match
// the primary.xml shape dnf expects.
type Entry struct {
	Name    string `json:"name" xml:"name,attr"`
	Flags   string `json:"flags,omitempty" xml:"flags,attr,omitempty"`
	Version string `json:"version,omitempty" xml:"ver,attr,omitempty"`
	Epoch   string `json:"epoch,omitempty" xml:"epoch,attr,omitempty"`
	Release string `json:"release,omitempty" xml:"rel,attr,omitempty"`
}

// File is one packaged file inside the .rpm payload.
type File struct {
	Path         string `json:"path" xml:",chardata"`
	Type         string `json:"type,omitempty" xml:"type,attr,omitempty"`
	IsExecutable bool   `json:"is_executable" xml:"-"`
}

// Changelog is one changelog entry from the RPM's CHANGELOG header.
type Changelog struct {
	Author string `json:"author,omitempty" xml:"author,attr"`
	Date   int64  `json:"date,omitempty" xml:"date,attr"`
	Text   string `json:"text,omitempty" xml:",chardata"`
}

// ParsePackage decodes the RPM lead/signature/header/payload format
// via go-rpmutils, extracts everything we need for the registry
// metadata, and returns it in our normalized shape. The lead and
// signature header are validated by go-rpmutils; we don't need to
// touch them. The main header contains every tag we care about.
//
// Ported from forgejo/modules/packages/rpm/metadata.go's
// ParsePackage(..., "rpm").
func ParsePackage(r io.Reader) (*Package, error) {
	rpm, err := rpmutils.ReadRpm(r)
	if err != nil {
		return nil, err
	}

	nevra, err := rpm.Header.GetNEVRA()
	if err != nil {
		return nil, err
	}

	// Combine version + release into the "version" we expose to the
	// rest of pkgmirror (which doesn't model release separately).
	// The original Epoch / Version / Release are still available on
	// FileMetadata for index emission.
	version := fmt.Sprintf("%s-%s", nevra.Version, nevra.Release)
	if nevra.Epoch != "" && nevra.Epoch != "0" {
		version = fmt.Sprintf("%s-%s", nevra.Epoch, version)
	}

	p := &Package{
		Name:    nevra.Name,
		Version: version,
		VersionMetadata: &VersionMetadata{
			Summary:     getString(rpm.Header, rpmutils.SUMMARY),
			Description: getString(rpm.Header, rpmutils.DESCRIPTION),
			License:     getString(rpm.Header, rpmutils.LICENSE),
			ProjectURL:  getString(rpm.Header, rpmutils.URL),
		},
		FileMetadata: &FileMetadata{
			Architecture:  nevra.Arch,
			Epoch:         nevra.Epoch,
			Version:       nevra.Version,
			Release:       nevra.Release,
			Vendor:        getString(rpm.Header, rpmutils.VENDOR),
			Group:         getString(rpm.Header, rpmutils.GROUP),
			Packager:      getString(rpm.Header, rpmutils.PACKAGER),
			SourceRpm:     getString(rpm.Header, rpmutils.SOURCERPM),
			BuildHost:     getString(rpm.Header, rpmutils.BUILDHOST),
			BuildTime:     getUInt64(rpm.Header, rpmutils.BUILDTIME),
			FileTime:      getUInt64(rpm.Header, rpmutils.FILEMTIMES),
			InstalledSize: getUInt64(rpm.Header, rpmutils.SIZE),
			ArchiveSize:   getUInt64(rpm.Header, rpmutils.SIG_PAYLOADSIZE),

			Provides:   getEntries(rpm.Header, rpmutils.PROVIDENAME, rpmutils.PROVIDEVERSION, rpmutils.PROVIDEFLAGS),
			Requires:   getEntries(rpm.Header, rpmutils.REQUIRENAME, rpmutils.REQUIREVERSION, rpmutils.REQUIREFLAGS),
			Conflicts:  getEntries(rpm.Header, rpmutils.CONFLICTNAME, rpmutils.CONFLICTVERSION, rpmutils.CONFLICTFLAGS),
			Obsoletes:  getEntries(rpm.Header, rpmutils.OBSOLETENAME, rpmutils.OBSOLETEVERSION, rpmutils.OBSOLETEFLAGS),
			Files:      getFiles(rpm.Header),
			Changelogs: getChangelogs(rpm.Header),
		},
	}

	if !isValidProjectURL(p.VersionMetadata.ProjectURL) {
		p.VersionMetadata.ProjectURL = ""
	}

	return p, nil
}

func getString(h *rpmutils.RpmHeader, tag int) string {
	values, err := h.GetStrings(tag)
	if err != nil || len(values) < 1 {
		return ""
	}
	return values[0]
}

func getUInt64(h *rpmutils.RpmHeader, tag int) uint64 {
	values, err := h.GetUint64s(tag)
	if err != nil || len(values) < 1 {
		return 0
	}
	return values[0]
}

// getEntries decodes a triplet of (names, versions, flags) tags into
// the normalized Entry slice. Flags are translated from the RPM
// bitfield into the GT/LT/EQ/GE/LE strings that dnf's primary.xml
// parser recognizes. Version strings can carry an `epoch:version-release`
// shape inline (rather than in separate tags) — we split that out so
// the XML emission has all three components.
func getEntries(h *rpmutils.RpmHeader, namesTag, versionsTag, flagsTag int) []*Entry {
	names, err := h.GetStrings(namesTag)
	if err != nil || len(names) == 0 {
		return nil
	}
	flags, err := h.GetUint64s(flagsTag)
	if err != nil || len(flags) == 0 {
		return nil
	}
	versions, err := h.GetStrings(versionsTag)
	if err != nil || len(versions) == 0 {
		return nil
	}
	if len(names) != len(flags) || len(names) != len(versions) {
		return nil
	}

	entries := make([]*Entry, 0, len(names))
	for i := range names {
		e := &Entry{Name: names[i]}

		// Compare-op flags. The four RPMSENSE bits are mutually
		// exclusive-ish; the GE / LE combinations are common enough
		// that dnf relies on the compact two-letter encoding.
		f := flags[i]
		switch {
		case f&rpmutils.RPMSENSE_GREATER != 0 && f&rpmutils.RPMSENSE_EQUAL != 0:
			e.Flags = "GE"
		case f&rpmutils.RPMSENSE_LESS != 0 && f&rpmutils.RPMSENSE_EQUAL != 0:
			e.Flags = "LE"
		case f&rpmutils.RPMSENSE_GREATER != 0:
			e.Flags = "GT"
		case f&rpmutils.RPMSENSE_LESS != 0:
			e.Flags = "LT"
		case f&rpmutils.RPMSENSE_EQUAL != 0:
			e.Flags = "EQ"
		}

		if v := versions[i]; v != "" {
			parts := strings.Split(v, "-")
			verParts := strings.Split(parts[0], ":")
			if len(verParts) == 2 {
				e.Epoch = verParts[0]
				e.Version = verParts[1]
			} else {
				e.Epoch = "0"
				e.Version = verParts[0]
			}
			if len(parts) > 1 {
				e.Release = parts[1]
			}
		}
		entries = append(entries, e)
	}
	return entries
}

// getFiles assembles File entries from the parallel BASENAMES /
// DIRNAMES / DIRINDEXES tag arrays (the RPM format stores file
// names indirectly to deduplicate directories). Tags FILEFLAGS
// and FILEMODES classify each entry as regular / directory / ghost
// and capture the executable bit.
func getFiles(h *rpmutils.RpmHeader) []*File {
	baseNames, _ := h.GetStrings(rpmutils.BASENAMES)
	dirNames, _ := h.GetStrings(rpmutils.DIRNAMES)
	dirIndexes, _ := h.GetUint32s(rpmutils.DIRINDEXES)
	fileFlags, _ := h.GetUint32s(rpmutils.FILEFLAGS)
	fileModes, _ := h.GetUint32s(rpmutils.FILEMODES)

	files := make([]*File, 0, len(baseNames))
	for i := range baseNames {
		if len(dirIndexes) <= i {
			continue
		}
		dirIndex := dirIndexes[i]
		if len(dirNames) <= int(dirIndex) {
			continue
		}

		var fileType string
		var isExecutable bool
		switch {
		case i < len(fileFlags) && fileFlags[i]&rpmutils.RPMFILE_GHOST != 0:
			fileType = "ghost"
		case i < len(fileModes):
			if fileModes[i]&sIFMT == sIFDIR {
				fileType = "dir"
			} else {
				mode := fileModes[i] & ^uint32(sIFMT)
				isExecutable = mode&sIXUSR != 0 || mode&sIXGRP != 0 || mode&sIXOTH != 0
			}
		}

		files = append(files, &File{
			Path:         dirNames[dirIndex] + baseNames[i],
			Type:         fileType,
			IsExecutable: isExecutable,
		})
	}
	return files
}

// getChangelogs decodes the parallel CHANGELOG{TEXT,NAME,TIME}
// arrays. dnf doesn't depend on these for resolution but they're
// emitted into other.xml.gz so `dnf changelog <pkg>` works.
func getChangelogs(h *rpmutils.RpmHeader) []*Changelog {
	texts, err := h.GetStrings(rpmutils.CHANGELOGTEXT)
	if err != nil || len(texts) == 0 {
		return nil
	}
	authors, err := h.GetStrings(rpmutils.CHANGELOGNAME)
	if err != nil || len(authors) == 0 {
		return nil
	}
	times, err := h.GetUint32s(rpmutils.CHANGELOGTIME)
	if err != nil || len(times) == 0 {
		return nil
	}
	if len(texts) != len(authors) || len(texts) != len(times) {
		return nil
	}
	out := make([]*Changelog, 0, len(texts))
	for i := range texts {
		out = append(out, &Changelog{
			Author: authors[i],
			Date:   int64(times[i]),
			Text:   texts[i],
		})
	}
	return out
}

func isValidProjectURL(s string) bool {
	if s == "" {
		return false
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return u.Host != ""
}
