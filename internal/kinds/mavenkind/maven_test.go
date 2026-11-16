package mavenkind

import (
	"testing"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/kinds"
	"github.com/mmrzaf/larder/internal/kinds/kindtest"
)

func newKind(t *testing.T, opts config.MavenOptions) kinds.Kind {
	t.Helper()
	kind, err := New(config.Repo{Name: "maven", Kind: "maven", Maven: &opts})
	if err != nil {
		t.Fatal(err)
	}
	return kind
}

func TestMatrix_M01_Classification(t *testing.T) {
	kind := newKind(t, config.MavenOptions{CompanionFetch: true})
	kindtest.Table(t, kind, []kindtest.Case{
		{Name: "metadata", Path: "/org/example/artifact/maven-metadata.xml", Want: kinds.Plan{Class: kinds.Mutable, Key: "/org/example/artifact/maven-metadata.xml", UpstreamPath: "/org/example/artifact/maven-metadata.xml", FreshTTL: 300e9}},
		{Name: "metadata sha1", Path: "/org/example/artifact/maven-metadata.xml.sha1", Want: kinds.Plan{Class: kinds.Mutable, Key: "/org/example/artifact/maven-metadata.xml.sha1", UpstreamPath: "/org/example/artifact/maven-metadata.xml.sha1", FreshTTL: 300e9}},
		{Name: "archetype catalog", Path: "/archetype-catalog.xml", Want: kinds.Plan{Class: kinds.Mutable, Key: "/archetype-catalog.xml", UpstreamPath: "/archetype-catalog.xml"}},
		{Name: "archetype catalog signature", Path: "/archetype-catalog.xml.asc", Want: kinds.Plan{Class: kinds.Mutable, Key: "/archetype-catalog.xml.asc", UpstreamPath: "/archetype-catalog.xml.asc"}},
		{Name: "snapshot", Path: "/org/example/artifact/1.0-SNAPSHOT/artifact-1.0-SNAPSHOT.jar", Want: kinds.Plan{Class: kinds.Mutable, Key: "/org/example/artifact/1.0-SNAPSHOT/artifact-1.0-SNAPSHOT.jar", UpstreamPath: "/org/example/artifact/1.0-SNAPSHOT/artifact-1.0-SNAPSHOT.jar"}},
		{Name: "timestamped snapshot", Path: "/org/example/artifact/1.0-SNAPSHOT/artifact-1.0-20240101.120000-1.jar", Want: kinds.Plan{Class: kinds.Immutable, Key: "/org/example/artifact/1.0-SNAPSHOT/artifact-1.0-20240101.120000-1.jar", UpstreamPath: "/org/example/artifact/1.0-SNAPSHOT/artifact-1.0-20240101.120000-1.jar", Binary: true}},
		{Name: "release jar", Path: "/org/example/artifact/1.2.3/artifact-1.2.3.jar", Want: kinds.Plan{Class: kinds.Immutable, Key: "/org/example/artifact/1.2.3/artifact-1.2.3.jar", UpstreamPath: "/org/example/artifact/1.2.3/artifact-1.2.3.jar", Binary: true}},
		{Name: "pom", Path: "/org/example/artifact/1.2.3/artifact-1.2.3.pom", Want: kinds.Plan{Class: kinds.Immutable, Key: "/org/example/artifact/1.2.3/artifact-1.2.3.pom", UpstreamPath: "/org/example/artifact/1.2.3/artifact-1.2.3.pom", Binary: true}},
		{Name: "module metadata", Path: "/org/example/artifact/1.2.3/artifact-1.2.3.module", Want: kinds.Plan{Class: kinds.Immutable, Key: "/org/example/artifact/1.2.3/artifact-1.2.3.module", UpstreamPath: "/org/example/artifact/1.2.3/artifact-1.2.3.module", Binary: true}},
		{Name: "classifier", Path: "/org/example/artifact/1.2.3/artifact-1.2.3-sources.jar", Want: kinds.Plan{Class: kinds.Immutable, Key: "/org/example/artifact/1.2.3/artifact-1.2.3-sources.jar", UpstreamPath: "/org/example/artifact/1.2.3/artifact-1.2.3-sources.jar", Binary: true}},
		{Name: "checksum", Path: "/org/example/artifact/1.2.3/artifact-1.2.3.jar.sha1", Want: kinds.Plan{Class: kinds.Immutable, Key: "/org/example/artifact/1.2.3/artifact-1.2.3.jar.sha1", UpstreamPath: "/org/example/artifact/1.2.3/artifact-1.2.3.jar.sha1", Binary: true}},
		{Name: "signature", Path: "/org/example/artifact/1.2.3/artifact-1.2.3.jar.asc", Want: kinds.Plan{Class: kinds.Immutable, Key: "/org/example/artifact/1.2.3/artifact-1.2.3.jar.asc", UpstreamPath: "/org/example/artifact/1.2.3/artifact-1.2.3.jar.asc", Binary: true}},
		{Name: "directory", Path: "/org/example/artifact/1.2.3/", Want: kinds.Plan{Class: kinds.Deny, Key: "/org/example/artifact/1.2.3/", UpstreamPath: "/org/example/artifact/1.2.3/"}},
	})
}

func TestMavenCompanionHooks(t *testing.T) {
	kind := newKind(t, config.MavenOptions{CompanionFetch: true})
	plan, err := kind.Classify(kinds.Request{Path: "/org/example/artifact/1.2.3/artifact-1.2.3.jar"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Hooks.Companions == nil {
		t.Fatal("companions hook is missing")
	}
	companions := plan.Hooks.Companions(plan.Key)
	if len(companions) != 4 {
		t.Fatalf("companions = %#v", companions)
	}
	for i, want := range []string{".sha512", ".sha256", ".sha1", ".md5"} {
		if companions[i].Suffix != want {
			t.Fatalf("companion %d = %#v, want %s", i, companions[i], want)
		}
	}
	if plan.Hooks.ChecksumFor == nil {
		t.Fatal("checksum hook is missing")
	}
	artifact, algo, ok := plan.Hooks.ChecksumFor(plan.Key + ".sha1")
	if !ok || artifact != plan.Key || algo != "sha1" {
		t.Fatalf("checksum hook = %q %q %v", artifact, algo, ok)
	}

	checksumPlan, err := kind.Classify(kinds.Request{Path: plan.Key + ".sha1"})
	if err != nil {
		t.Fatal(err)
	}
	if checksumPlan.Hooks.Companions != nil {
		t.Fatal("checksum file has companion hooks")
	}

	disabled := newKind(t, config.MavenOptions{CompanionFetch: false})
	plan, err = disabled.Classify(kinds.Request{Path: "/org/example/artifact/1.2.3/artifact-1.2.3.jar"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Hooks.Companions != nil {
		t.Fatal("companion hooks present with companion_fetch disabled")
	}
}

func TestMavenConstructorError(t *testing.T) {
	if _, err := New(config.Repo{Name: "maven", Kind: "maven"}); err == nil {
		t.Fatal("missing maven options accepted")
	}
}
