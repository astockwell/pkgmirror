// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2023 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// Index and signing-key builders for the RPM (yum/dnf) repository
// format. Modeled on forgejo/services/packages/rpm/repository.go
// (MIT). The four-file metadata bundle (repomd.xml +
// primary.xml.gz + filelists.xml.gz + other.xml.gz) and the
// detached repomd.xml.asc signature are preserved byte-shape
// compatible with what real dnf and yum expect to find in
// `<group>/repodata/`.
//
// Differences from upstream:
//
//   - We build indices on demand per request rather than persisting
//     them as file rows. See docs/adding-a-format.md "On-demand vs
//     cached index generation".
//   - The per-data timestamp in repomd.xml uses
//     `max(file.created_unix)` across the group, not `time.Now()`,
//     so the bytes are deterministic across separate GET requests
//     for repomd.xml vs repomd.xml.asc. See
//     docs/known-deviations-from-spec.md for the equivalent Debian
//     deviation that motivated this pattern.
//   - Per-tenant OpenPGP keypair is stored as properties on a
//     synthetic `_rpm` package row (matches Alpine + Debian) rather
//     than a `user_settings` table.

package rpm

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/astockwell/pkgmirror/internal/models"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// --- repomd.xml types ------------------------------------------------------

// RepoChecksum is one `<checksum>` or `<open-checksum>` element on a
// `<data>` entry. `type` is the hash algorithm name ("sha256"); the
// element text is the hex digest.
type RepoChecksum struct {
	Value string `xml:",chardata"`
	Type  string `xml:"type,attr"`
}

// RepoLocation is `<location href="repodata/<file>"/>` — apt
// (sorry, dnf) follows this to fetch the per-data file.
type RepoLocation struct {
	Href string `xml:"href,attr"`
}

// RepoData is one `<data type="primary|filelists|other|...">`
// entry inside repomd.xml. Order: checksum, open-checksum, location,
// timestamp, size, open-size. This element order is what real
// metadata generators emit and what dnf's parser is most lenient
// about.
type RepoData struct {
	Type         string       `xml:"type,attr"`
	Checksum     RepoChecksum `xml:"checksum"`
	OpenChecksum RepoChecksum `xml:"open-checksum"`
	Location     RepoLocation `xml:"location"`
	Timestamp    int64        `xml:"timestamp"`
	Size         int64        `xml:"size"`
	OpenSize     int64        `xml:"open-size"`
}

// Repomd is the root `<repomd>` document.
type Repomd struct {
	XMLName  xml.Name    `xml:"repomd"`
	Xmlns    string      `xml:"xmlns,attr"`
	XmlnsRpm string      `xml:"xmlns:rpm,attr"`
	Data     []*RepoData `xml:"data"`
}

// IndexEntry is one .rpm row materialized for metadata emission.
// The handler loads these from the DB and hands them to BuildAll.
type IndexEntry struct {
	Pkg     *models.Package
	Ver     *models.Version
	Blob    *models.Blob
	File    *models.File
	VerMeta VersionMetadata
	FileMd  FileMetadata
}

// MetadataBundle is the complete on-disk set a dnf client expects:
// the three per-arch xml.gz files plus the index that references
// them with hashes, plus a detached signature over the index.
type MetadataBundle struct {
	Primary   []byte // primary.xml.gz
	Filelists []byte // filelists.xml.gz
	Other     []byte // other.xml.gz
	Repomd    []byte // repomd.xml
	RepomdAsc []byte // detached PGP signature over Repomd
}

// BuildAll constructs the full metadata bundle. `releaseTimestamp`
// is the deterministic `<timestamp>` value used for every `<data>`
// entry in repomd.xml; callers derive it from
// `max(file.created_unix)` across the group so /repomd.xml and
// /repomd.xml.asc see byte-identical Repomd bytes across separate
// requests. See docs/known-deviations-from-spec.md.
func BuildAll(entries []*IndexEntry, group string, releaseTimestamp int64, privPEM string) (*MetadataBundle, error) {
	primary, primaryData, err := buildPrimaryGz(entries, group, releaseTimestamp)
	if err != nil {
		return nil, fmt.Errorf("build primary: %w", err)
	}
	filelists, filelistsData, err := buildFilelistsGz(entries, releaseTimestamp)
	if err != nil {
		return nil, fmt.Errorf("build filelists: %w", err)
	}
	other, otherData, err := buildOtherGz(entries, releaseTimestamp)
	if err != nil {
		return nil, fmt.Errorf("build other: %w", err)
	}

	repomd, err := xmlMarshalWithHeader(&Repomd{
		Xmlns:    "http://linux.duke.edu/metadata/repo",
		XmlnsRpm: "http://linux.duke.edu/metadata/rpm",
		Data:     []*RepoData{primaryData, filelistsData, otherData},
	})
	if err != nil {
		return nil, err
	}

	entity, err := parseArmoredEntity(privPEM)
	if err != nil {
		return nil, err
	}
	var sig bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&sig, entity, bytes.NewReader(repomd), nil); err != nil {
		return nil, fmt.Errorf("sign repomd.xml: %w", err)
	}

	return &MetadataBundle{
		Primary:   primary,
		Filelists: filelists,
		Other:     other,
		Repomd:    repomd,
		RepomdAsc: sig.Bytes(),
	}, nil
}

// --- per-file XML builders -------------------------------------------------

// buildPrimaryGz emits primary.xml.gz and returns the bytes + the
// RepoData entry to register it in repomd.xml. The packageHref is
// computed from the on-disk basename apt-equivalent download URL
// (`package/<name>/<version>/<arch>/<basename>.rpm`).
func buildPrimaryGz(entries []*IndexEntry, group string, ts int64) ([]byte, *RepoData, error) {
	// Local types match the XML shape forgejo emits. xml namespace
	// prefixing on `rpm:*` requires struct tag prefixes — Go's
	// encoding/xml doesn't fold namespaces, so we name attrs
	// directly.
	type version struct {
		Epoch   string `xml:"epoch,attr"`
		Version string `xml:"ver,attr"`
		Release string `xml:"rel,attr"`
	}
	type checksum struct {
		Checksum string `xml:",chardata"`
		Type     string `xml:"type,attr"`
		Pkgid    string `xml:"pkgid,attr"`
	}
	type times struct {
		File  uint64 `xml:"file,attr"`
		Build uint64 `xml:"build,attr"`
	}
	type sizes struct {
		Package   int64  `xml:"package,attr"`
		Installed uint64 `xml:"installed,attr"`
		Archive   uint64 `xml:"archive,attr"`
	}
	type location struct {
		Href string `xml:"href,attr"`
	}
	type entryList struct {
		Entries []*Entry `xml:"rpm:entry"`
	}
	type format struct {
		License   string    `xml:"rpm:license"`
		Vendor    string    `xml:"rpm:vendor"`
		Group     string    `xml:"rpm:group"`
		Buildhost string    `xml:"rpm:buildhost"`
		Sourcerpm string    `xml:"rpm:sourcerpm"`
		Provides  entryList `xml:"rpm:provides"`
		Requires  entryList `xml:"rpm:requires"`
		Conflicts entryList `xml:"rpm:conflicts"`
		Obsoletes entryList `xml:"rpm:obsoletes"`
		Files     []*File   `xml:"file"`
	}
	type pkg struct {
		XMLName      xml.Name `xml:"package"`
		Type         string   `xml:"type,attr"`
		Name         string   `xml:"name"`
		Architecture string   `xml:"arch"`
		Version      version  `xml:"version"`
		Checksum     checksum `xml:"checksum"`
		Summary      string   `xml:"summary"`
		Description  string   `xml:"description"`
		Packager     string   `xml:"packager"`
		URL          string   `xml:"url"`
		Time         times    `xml:"time"`
		Size         sizes    `xml:"size"`
		Location     location `xml:"location"`
		Format       format   `xml:"format"`
	}
	type metadata struct {
		XMLName      xml.Name `xml:"metadata"`
		Xmlns        string   `xml:"xmlns,attr"`
		XmlnsRpm     string   `xml:"xmlns:rpm,attr"`
		PackageCount int      `xml:"packages,attr"`
		Packages     []*pkg   `xml:"package"`
	}

	packages := make([]*pkg, 0, len(entries))
	for _, e := range entries {
		// primary.xml only carries executable files (the full list
		// lives in filelists.xml). Matches forgejo and the upstream
		// `createrepo_c` behavior.
		var execFiles []*File
		for _, f := range e.FileMd.Files {
			if f.IsExecutable {
				execFiles = append(execFiles, f)
			}
		}
		packages = append(packages, &pkg{
			Type:         "rpm",
			Name:         e.Pkg.Name,
			Architecture: e.FileMd.Architecture,
			Version: version{
				Epoch:   e.FileMd.Epoch,
				Version: e.FileMd.Version,
				Release: e.FileMd.Release,
			},
			Checksum: checksum{
				Type:     "sha256",
				Checksum: e.Blob.HashSHA256,
				Pkgid:    "YES",
			},
			Summary:     e.VerMeta.Summary,
			Description: e.VerMeta.Description,
			Packager:    e.FileMd.Packager,
			URL:         e.VerMeta.ProjectURL,
			Time: times{
				File:  e.FileMd.FileTime,
				Build: e.FileMd.BuildTime,
			},
			Size: sizes{
				Package:   e.Blob.Size,
				Installed: e.FileMd.InstalledSize,
				Archive:   e.FileMd.ArchiveSize,
			},
			Location: location{
				// Maps to the handler's
				// /pool/.../package/:name/:version/:architecture/:filename route.
				Href: fmt.Sprintf("package/%s/%s/%s/%s-%s.%s.rpm",
					e.Pkg.Name, e.Ver.Version, e.FileMd.Architecture,
					e.Pkg.Name, e.Ver.Version, e.FileMd.Architecture),
			},
			Format: format{
				License:   e.VerMeta.License,
				Vendor:    e.FileMd.Vendor,
				Group:     e.FileMd.Group,
				Buildhost: e.FileMd.BuildHost,
				Sourcerpm: e.FileMd.SourceRpm,
				Provides:  entryList{Entries: e.FileMd.Provides},
				Requires:  entryList{Entries: e.FileMd.Requires},
				Conflicts: entryList{Entries: e.FileMd.Conflicts},
				Obsoletes: entryList{Entries: e.FileMd.Obsoletes},
				Files:     execFiles,
			},
		})
	}

	return gzipXML("primary", &metadata{
		Xmlns:        "http://linux.duke.edu/metadata/common",
		XmlnsRpm:     "http://linux.duke.edu/metadata/rpm",
		PackageCount: len(entries),
		Packages:     packages,
	}, ts)
}

func buildFilelistsGz(entries []*IndexEntry, ts int64) ([]byte, *RepoData, error) {
	type version struct {
		Epoch   string `xml:"epoch,attr"`
		Version string `xml:"ver,attr"`
		Release string `xml:"rel,attr"`
	}
	type pkg struct {
		Pkgid        string  `xml:"pkgid,attr"`
		Name         string  `xml:"name,attr"`
		Architecture string  `xml:"arch,attr"`
		Version      version `xml:"version"`
		Files        []*File `xml:"file"`
	}
	type filelists struct {
		XMLName      xml.Name `xml:"filelists"`
		Xmlns        string   `xml:"xmlns,attr"`
		PackageCount int      `xml:"packages,attr"`
		Packages     []*pkg   `xml:"package"`
	}

	packages := make([]*pkg, 0, len(entries))
	for _, e := range entries {
		packages = append(packages, &pkg{
			Pkgid:        e.Blob.HashSHA256,
			Name:         e.Pkg.Name,
			Architecture: e.FileMd.Architecture,
			Version: version{
				Epoch: e.FileMd.Epoch, Version: e.FileMd.Version, Release: e.FileMd.Release,
			},
			Files: e.FileMd.Files,
		})
	}
	return gzipXML("filelists", &filelists{
		Xmlns:        "http://linux.duke.edu/metadata/filelists",
		PackageCount: len(entries),
		Packages:     packages,
	}, ts)
}

func buildOtherGz(entries []*IndexEntry, ts int64) ([]byte, *RepoData, error) {
	type version struct {
		Epoch   string `xml:"epoch,attr"`
		Version string `xml:"ver,attr"`
		Release string `xml:"rel,attr"`
	}
	type pkg struct {
		Pkgid        string       `xml:"pkgid,attr"`
		Name         string       `xml:"name,attr"`
		Architecture string       `xml:"arch,attr"`
		Version      version      `xml:"version"`
		Changelogs   []*Changelog `xml:"changelog"`
	}
	type otherdata struct {
		XMLName      xml.Name `xml:"otherdata"`
		Xmlns        string   `xml:"xmlns,attr"`
		PackageCount int      `xml:"packages,attr"`
		Packages     []*pkg   `xml:"package"`
	}

	packages := make([]*pkg, 0, len(entries))
	for _, e := range entries {
		packages = append(packages, &pkg{
			Pkgid:        e.Blob.HashSHA256,
			Name:         e.Pkg.Name,
			Architecture: e.FileMd.Architecture,
			Version: version{
				Epoch: e.FileMd.Epoch, Version: e.FileMd.Version, Release: e.FileMd.Release,
			},
			Changelogs: e.FileMd.Changelogs,
		})
	}
	return gzipXML("other", &otherdata{
		Xmlns:        "http://linux.duke.edu/metadata/other",
		PackageCount: len(entries),
		Packages:     packages,
	}, ts)
}

// gzipXML XML-encodes `obj` with the standard header and gzips it.
// Returns (gz bytes, RepoData entry for repomd.xml). The
// timestamp is the caller-supplied deterministic value, not
// time.Now(), so the resulting repomd.xml is byte-stable across
// separate GET requests.
func gzipXML(filetype string, obj any, timestamp int64) ([]byte, *RepoData, error) {
	body, err := xmlMarshalWithHeader(obj)
	if err != nil {
		return nil, nil, err
	}

	openSize := int64(len(body))
	openSum := sha256.Sum256(body)

	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	if _, err := gw.Write(body); err != nil {
		return nil, nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, nil, err
	}

	gzBytes := gz.Bytes()
	gzSum := sha256.Sum256(gzBytes)
	filename := filetype + ".xml.gz"

	return gzBytes, &RepoData{
		Type: filetype,
		Checksum: RepoChecksum{
			Type:  "sha256",
			Value: hex.EncodeToString(gzSum[:]),
		},
		OpenChecksum: RepoChecksum{
			Type:  "sha256",
			Value: hex.EncodeToString(openSum[:]),
		},
		Location:  RepoLocation{Href: "repodata/" + filename},
		Timestamp: timestamp,
		Size:      int64(len(gzBytes)),
		OpenSize:  openSize,
	}, nil
}

func xmlMarshalWithHeader(obj any) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	if err := xml.NewEncoder(&buf).Encode(obj); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// --- per-tenant signing key ------------------------------------------------

// GenerateKeyPair builds a fresh OpenPGP keypair as armored PEM
// strings. apt and dnf both accept this format directly.
func GenerateKeyPair() (privatePEM, publicPEM string, err error) {
	e, err := openpgp.NewEntity("", "RPM Registry", "", nil)
	if err != nil {
		return "", "", err
	}

	var priv, pub strings.Builder
	pw, err := armor.Encode(&priv, openpgp.PrivateKeyType, nil)
	if err != nil {
		return "", "", err
	}
	if err := e.SerializePrivate(pw, nil); err != nil {
		return "", "", err
	}
	_ = pw.Close()

	uw, err := armor.Encode(&pub, openpgp.PublicKeyType, nil)
	if err != nil {
		return "", "", err
	}
	if err := e.Serialize(uw); err != nil {
		return "", "", err
	}
	_ = uw.Close()

	return priv.String(), pub.String(), nil
}

// GetOrCreateKeyPair returns the per-tenant OpenPGP keypair, generating
// one on first call. Keys are stored as properties on the synthetic
// `_rpm` package row scoped to the tenant — same pattern as Debian
// (`_debian`) and Alpine (`_alpine`). Concurrent first-time calls are
// serialized externally by the handler via ExclusivePool; this
// function does not lock.
func GetOrCreateKeyPair(ctx context.Context, m *models.Store, tenantID int64) (privatePEM, publicPEM string, err error) {
	pkg, err := m.GetOrCreatePackage(ctx, tenantID, models.TypeRPM, RepositoryPackage, models.CreatedViaUploaded)
	if err != nil {
		return "", "", err
	}
	priv, hasPriv, err := m.GetProperty(ctx, models.PropertyRefPackage, pkg.ID, SettingKeyPrivate)
	if err != nil {
		return "", "", err
	}
	pub, hasPub, err := m.GetProperty(ctx, models.PropertyRefPackage, pkg.ID, SettingKeyPublic)
	if err != nil {
		return "", "", err
	}
	if hasPriv && hasPub && priv != "" && pub != "" {
		return priv, pub, nil
	}

	priv, pub, err = GenerateKeyPair()
	if err != nil {
		return "", "", err
	}
	if err := m.SetProperty(ctx, models.PropertyRefPackage, pkg.ID, SettingKeyPrivate, priv); err != nil {
		return "", "", err
	}
	if err := m.SetProperty(ctx, models.PropertyRefPackage, pkg.ID, SettingKeyPublic, pub); err != nil {
		return "", "", err
	}
	return priv, pub, nil
}

func parseArmoredEntity(privPEM string) (*openpgp.Entity, error) {
	block, err := armor.Decode(strings.NewReader(privPEM))
	if err != nil {
		return nil, fmt.Errorf("decode armored private key: %w", err)
	}
	if block.Type != openpgp.PrivateKeyType {
		return nil, errors.New("armored block is not an openpgp private key")
	}
	return openpgp.ReadEntity(packet.NewReader(block.Body))
}

// --- .repo config file -----------------------------------------------------

// BuildRepoConfig renders the [pkgmirror-<tenant>-<group>] section
// dnf and yum expect to find in /etc/yum.repos.d/. baseURL is the
// fully-qualified URL prefix up to the group (e.g.
// "https://mirror.example.com/api/packages/default/rpm/el9").
func BuildRepoConfig(tenantName, group, baseURL string) []byte {
	sectionID := "pkgmirror-" + tenantName
	if group != "" {
		sectionID += "-" + strings.ReplaceAll(group, "/", "-")
	}
	displayName := "pkgmirror"
	if tenantName != "" {
		displayName += " - " + tenantName
	}
	if group != "" {
		displayName += " - " + group
	}
	body := fmt.Sprintf(`[%s]
name=%s
baseurl=%s
enabled=1
gpgcheck=1
gpgkey=%s/repository.key
`, sectionID, displayName, baseURL, baseURL)
	return []byte(body)
}

// writtenCounter is a small io.Writer that records how many bytes
// passed through it; used by tests to verify gzip output didn't lose
// data.
type writtenCounter struct{ n int64 }

func (w *writtenCounter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

func (w *writtenCounter) N() int64 { return w.n }

// silence unused-warnings on the helper above (it's used by tests
// but exported for future internal callers as well).
var _ io.Writer = (*writtenCounter)(nil)
