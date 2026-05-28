//go:build blackbox

// Maven wire-compatibility test. Drives real `mvn` from
// maven:3.9-eclipse-temurin-21 against our registry: `mvn deploy` to
// publish, then `mvn dependency:get` to resolve and download from a
// fresh local repository. The maven-metadata.xml generation, POM
// parsing, SHA-1 verification, and PUT-followed-by-GET round trip
// are all exercised by this single command pair.
//
// We don't go further (build a consumer project, exercise transitive
// resolution) because Maven Central's per-test caching makes that
// flaky in CI for reasons unrelated to our code.

package maven_blackbox_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/astockwell/pkgmirror/tests/blackbox/harness"
)

// registryURL is the repo root the maven client treats as a Maven
// repository. `mvn` appends `<groupId-as-path>/<artifactId>/<version>/...`
// to whatever URL we hand it.
func registryURL(s *harness.Stack) string {
	return s.InternalBaseURL + "/api/packages/" + harness.DefaultTenant + "/maven"
}

const fixturePom = `<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <modelVersion>4.0.0</modelVersion>
  <groupId>com.example.pkgmirror</groupId>
  <artifactId>fixture</artifactId>
  <version>1.0.0</version>
  <packaging>jar</packaging>
  <name>pkgmirror fixture</name>
  <description>blackbox conformance fixture</description>
  <url>https://example.com/pkgmirror</url>
  <licenses>
    <license>
      <name>MIT</name>
    </license>
  </licenses>
</project>
`

// settingsXML configures `mvn` with our registry as a deployment
// target and supplies the credentials it'll use for PUT. The
// `pkgmirror` <id> here must match the `altDeploymentRepository`
// id used at the command line.
//
// We also override Maven 3.8.1+'s built-in `maven-default-http-blocker`
// mirror — which routes every external HTTP repo through a 0.0.0.0
// sink to enforce HTTPS-by-default — by shadowing it with a mirror of
// the same id that points at no upstream. Without this override
// `dependency:get` against an http:// URL fails with "Blocked mirror
// for repositories" even when the URL is explicitly configured. In
// production, operators would expose pkgmirror over TLS; for the
// conformance test we accept plain HTTP between sibling containers.
func settingsXML(registry, token string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<settings xmlns="http://maven.apache.org/SETTINGS/1.0.0">
  <servers>
    <server>
      <id>pkgmirror</id>
      <username>x</username>
      <password>` + token + `</password>
    </server>
  </servers>
  <mirrors>
    <mirror>
      <id>maven-default-http-blocker</id>
      <mirrorOf>dummy</mirrorOf>
      <name>Disabled default HTTP blocker (test only)</name>
      <url>http://0.0.0.0/</url>
      <blocked>false</blocked>
    </mirror>
  </mirrors>
</settings>
`
}

func TestMavenConformance_DeployAndResolve(t *testing.T) {
	ctx := context.Background()
	stack := harness.Start(ctx, t)
	registry := registryURL(stack)

	client := stack.NewClient(ctx, t, harness.ClientSpec{
		Image:   "maven:3.9-eclipse-temurin-21",
		WorkDir: "/work",
		Env: map[string]string{
			// Maven's default ~/.m2 lives under $HOME; pin it under
			// /work so we don't depend on a writable $HOME in the
			// container image (some hardened maven images don't
			// give the default user a home directory at all).
			"MAVEN_OPTS": "-Dmaven.repo.local=/work/.m2/repository",
		},
		Files: map[string]string{
			"/work/pom.xml":              fixturePom,
			"/work/settings.xml":         settingsXML(registry, stack.AdminToken),
		},
	})

	// 1) Create the artifact files maven will deploy. `mvn install`
	// would package a real JAR, but for the conformance check we
	// only need to verify the PUT/GET wire shape; using
	// `deploy:deploy-file` lets us bypass packaging entirely and
	// hand-craft a tiny binary jar.
	client.MustExec(t, "sh", "-c",
		"mkdir -p /work/target && printf 'PK\\003\\004fake jar bytes' > /work/target/fixture-1.0.0.jar")

	// 2) Deploy. `deploy:deploy-file` is the mvn goal designed
	// exactly for "I have a jar + pom on disk, push them to a
	// remote repository." It does the PUT-jar + PUT-pom + PUT-checksum
	// sidecars + maven-metadata.xml + checksum dance for us.
	out := client.MustExec(t, "mvn",
		"-s", "/work/settings.xml",
		"deploy:deploy-file",
		"-DrepositoryId=pkgmirror",
		"-Durl="+registry,
		"-DgroupId=com.example.pkgmirror",
		"-DartifactId=fixture",
		"-Dversion=1.0.0",
		"-Dpackaging=jar",
		"-Dfile=/work/target/fixture-1.0.0.jar",
		"-DpomFile=/work/pom.xml",
		"-DgeneratePom=false",
		"-B", // batch mode — no ANSI / progress chrome
	)
	if !strings.Contains(out, "BUILD SUCCESS") {
		t.Fatalf("mvn deploy did not BUILD SUCCESS:\n%s", out)
	}

	// 3) Resolve from a *fresh* local repository so we exercise the
	// download path, not Maven's cache. The
	// `dependency:get` goal pulls a specific GAV from the named
	// remote repository — it'll fail loudly if our generated
	// maven-metadata.xml is malformed or the checksum doesn't
	// validate against what we served.
	out = client.MustExec(t, "sh", "-c",
		"rm -rf /work/.m2/repository && "+
			"mvn -s /work/settings.xml dependency:get "+
			"-DremoteRepositories=pkgmirror::default::"+registry+" "+
			"-Dartifact=com.example.pkgmirror:fixture:1.0.0 "+
			"-Dtransitive=false -B")
	if !strings.Contains(out, "BUILD SUCCESS") {
		t.Fatalf("mvn dependency:get did not BUILD SUCCESS:\n%s", out)
	}

	// 4) Sanity check: the maven-metadata.xml we generated is
	// reachable directly and lists our version.
	out = client.MustExec(t, "sh", "-c",
		fmt.Sprintf("curl -fsS -u x:%s %s/com/example/pkgmirror/fixture/maven-metadata.xml",
			stack.AdminToken, registry))
	if !strings.Contains(out, "<version>1.0.0</version>") {
		t.Fatalf("maven-metadata.xml missing version 1.0.0:\n%s", out)
	}
}
