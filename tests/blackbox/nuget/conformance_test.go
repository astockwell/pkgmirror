//go:build blackbox

// NuGet V3 wire-compatibility test. Drives the real `dotnet` CLI
// from mcr.microsoft.com/dotnet/sdk:8.0 against our registry:
// configure a NuGet source, restore a project that depends on the
// fixture package, and verify the .nupkg lands in the local nuget
// cache.
//
// The load-bearing assertion is `dotnet add package` (which does
// service-index → registration index → packageBaseAddress download)
// succeeding against our V3 endpoints. If our @id rewriting were
// wrong, or our case-folding for lowercase URL components per the
// V3 spec were wrong, dotnet would fail with HTTP 404 on the
// download URL it constructs.

package nuget_blackbox_test

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/astockwell/pkgmirror/tests/blackbox/harness"
)

// fixtureNupkg builds a minimal valid .nupkg for the given id +
// version. Same shape as the grey-box fixture, intentionally
// duplicated since blackbox tests can't import internal packages.
func fixtureNupkg(t *testing.T, id, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	nuspec := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://schemas.microsoft.com/packaging/2013/05/nuspec.xsd">
  <metadata>
    <id>%s</id>
    <version>%s</version>
    <authors>pkgmirror blackbox</authors>
    <description>fixture package for nuget blackbox</description>
    <dependencies>
      <group targetFramework="net8.0" />
    </dependencies>
  </metadata>
</package>`, id, version)
	w, err := zw.Create(strings.ToLower(id) + ".nuspec")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	if _, err := io.WriteString(w, nuspec); err != nil {
		t.Fatalf("write nuspec: %v", err)
	}
	// dotnet requires at least one lib/<tfm>/*.dll inside the
	// package for `dotnet add package` to succeed without an
	// assets-file warning. The DLL contents are never verified —
	// dotnet just needs a non-empty file at the framework path.
	w, err = zw.Create("lib/net8.0/" + strings.ToLower(id) + ".dll")
	if err != nil {
		t.Fatalf("zip create lib: %v", err)
	}
	_, _ = w.Write([]byte("fake-dll-bytes"))
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func TestNuGetConformance_DotnetAddPackage(t *testing.T) {
	ctx := context.Background()
	stack := harness.Start(ctx, t)

	const (
		pkgID  = "PkgmirrorTest.Sample"
		pkgVer = "1.2.3"
	)

	// 1) Publish the fixture .nupkg. Our handler accepts raw
	// octet-stream PUTs alongside the multipart form-data shape
	// dotnet uses; the raw path is what the harness's UploadBytes
	// drives, and the bytes that land on disk are identical to a
	// multipart upload.
	uploadURL := fmt.Sprintf("/api/packages/%s/nuget", harness.DefaultTenant)
	harness.UploadBytes(t, stack, uploadURL, "application/octet-stream", fixtureNupkg(t, pkgID, pkgVer))

	// 2) Start a dotnet sdk container. The image is large (~700 MB)
	// but it's the only reliable way to test the real V3 client.
	client := stack.NewClient(ctx, t, harness.ClientSpec{
		Image:   "mcr.microsoft.com/dotnet/sdk:8.0",
		WorkDir: "/work",
	})

	// 3) Tell dotnet about our registry. Drop the default
	// nuget.org source first so the test is independent of the
	// public registry's reachability from CI. `dotnet nuget add
	// source -u/-p --store-password-in-clear-text` writes the
	// credentials to the user-level NuGet.Config keyed on the
	// source URL; subsequent `dotnet add package` calls without
	// `--source` consult all configured sources, which after the
	// remove+add is just pkgmirror.
	sourceURL := fmt.Sprintf("%s/api/packages/%s/nuget/index.json", stack.InternalBaseURL, harness.DefaultTenant)
	// `dotnet nuget remove source nuget.org` exits non-zero if
	// nuget.org wasn't configured (e.g. a future image variant
	// drops the default). Use `|| true` to tolerate that.
	client.MustExec(t, "sh", "-c", "dotnet nuget remove source nuget.org || true")
	client.MustExec(t, "dotnet", "nuget", "add", "source", sourceURL,
		"--name", "pkgmirror",
		"--username", "x",
		"--password", stack.AdminToken,
		"--store-password-in-clear-text")

	// 4) Create a tiny project that will pull the package in.
	// `dotnet new console -o app` writes Program.cs + app.csproj
	// to /work/app. The blackbox harness mounts /work but each
	// container has its own writable layer, so this is safe.
	client.MustExec(t, "dotnet", "new", "console", "-o", "/work/app", "--no-restore")

	// 5) Add the package. This is the load-bearing step: dotnet
	// hits our service index, looks up RegistrationsBaseUrl and
	// PackageBaseAddress, follows the chain to /registration/
	// pkgmirrortest.sample/index.json, finds the version, then
	// downloads /package/pkgmirrortest.sample/1.2.3/pkgmirrortest.
	// sample.1.2.3.nupkg. Any case-folding or @id-rewriting bug
	// surfaces here as HTTP 404 or "Unable to find package".
	out := client.MustExec(t, "sh", "-c",
		fmt.Sprintf("cd /work/app && dotnet add package %s --version %s", pkgID, pkgVer))
	if strings.Contains(strings.ToLower(out), "error") {
		t.Fatalf("dotnet add package surfaced an error:\n%s", out)
	}
	if !strings.Contains(out, "PackageReference") {
		t.Fatalf("dotnet add package output did not include a PackageReference confirmation:\n%s", out)
	}

	// 6) Sanity-check the assets file. After `dotnet add package`
	// dotnet automatically runs `dotnet restore`, which writes
	// obj/project.assets.json with all resolved package coordinates.
	// Grepping for our package id confirms the restore actually
	// pulled the bytes from pkgmirror (rather than silently using
	// a cached version from a previous run — which can't happen on
	// a fresh container layer, but the check is cheap).
	out = client.MustExec(t, "sh", "-c",
		fmt.Sprintf(`grep -F '"%s/%s"' /work/app/obj/project.assets.json`, pkgID, pkgVer))
	if !strings.Contains(out, pkgID) {
		t.Fatalf("project.assets.json missing the resolved package coordinate:\n%s", out)
	}
}
