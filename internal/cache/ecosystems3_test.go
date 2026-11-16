package cache_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/testutil"
	"github.com/mmrzaf/larder/testutil/env"
)

func cratesRepo() config.Repo {
	return config.Repo{Name: "crates", Kind: "crates", Mount: "/crates", ServeGone: true}
}

func rustupRepo() config.Repo {
	return config.Repo{Name: "rustup", Kind: "rustup", Mount: "/rustup", ServeGone: true}
}

func aptRepo() config.Repo {
	return config.Repo{
		Name: "apt", Kind: "apt", Mount: "/apt", ServeGone: true,
		APT: &config.APTOptions{SuiteConsistency: true},
	}
}

func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestMatrix_R02_ConfigJSON(t *testing.T) {
	e := env.New(t, env.Options{Repo: cratesRepo()})
	rec := e.GET("/index/config.json")
	mustCode(t, rec, 200)
	if rec.Body.String() != `{"dl":"http://larder.test/crates/dl"}` {
		t.Fatalf("config body = %q", rec.Body.String())
	}
	mustHeader(t, rec, "X-Larder-Cache", "BYPASS")
	if e.Upstream.Total() != 0 {
		t.Fatal("config.json contacted upstream")
	}
}

func TestMatrix_R03_IndexRevalidation(t *testing.T) {
	e := env.New(t, env.Options{Repo: cratesRepo()})
	path := "/index/se/rd/serde"
	e.Upstream.Set("/se/rd/serde", testutil.Route{Body: testutil.Fixture(t, "crates", "se", "rd", "serde")})
	mustHeader(t, e.GET(path), "X-Larder-Cache", "MISS")
	mustHeader(t, e.GET(path), "X-Larder-Cache", "HIT")
	e.Clock.Advance(61 * time.Second)
	e.Upstream.Set("/se/rd/serde", testutil.Route{Body: []byte(`{"name":"serde","vers":"1.0.1","cksum":"` + strings.Repeat("a", 64) + `"}` + "\n")})
	rec := e.GET(path)
	mustHeader(t, rec, "X-Larder-Cache", "REVALIDATED")
	if !strings.Contains(rec.Body.String(), "1.0.1") {
		t.Fatalf("revalidated index = %q", rec.Body.String())
	}
}

func TestMatrix_R04_DownloadTemplates(t *testing.T) {
	e := env.New(t, env.Options{
		Repo: cratesRepo(),
		CrateDLTemplates: []string{
			"/missing/{crate}/{crate}-{version}.crate",
			"/files/{crate}/{crate}-{version}.crate",
		},
	})
	path := "/dl/serde/1.0.0/download"
	e.Upstream.Set("/files/serde/serde-1.0.0.crate", testutil.Route{Body: testutil.Fixture(t, "crates", "serde-1.0.0.crate")})
	rec := e.GET(path)
	mustCode(t, rec, 200)
	if !bytes.Equal(rec.Body.Bytes(), testutil.Fixture(t, "crates", "serde-1.0.0.crate")) {
		t.Fatal("crate body differs")
	}
	if e.Upstream.Count("/missing/serde/serde-1.0.0.crate") != 1 {
		t.Fatal("first download template was not tried")
	}
}

func TestMatrix_R05_CksumVerification(t *testing.T) {
	e := env.New(t, env.Options{Repo: cratesRepo()})
	indexPath := "/index/se/rd/serde"
	e.Upstream.Set("/se/rd/serde", testutil.Route{Body: testutil.Fixture(t, "crates", "se", "rd", "serde")})
	mustCode(t, e.GET(indexPath), 200)
	download := "/dl/serde/1.0.0/download"
	e.Upstream.Set("/files/serde/serde-1.0.0.crate", testutil.Route{Body: testutil.Fixture(t, "crates", "serde-1.0.0.crate")})
	// The default template is used unless a template is configured; route the
	// official path through the fake host by using a configured template.
	e2 := env.New(t, env.Options{Repo: cratesRepo(), CrateDLTemplates: []string{"/files/{crate}/{crate}-{version}.crate"}})
	e2.Upstream.Set("/se/rd/serde", testutil.Route{Body: testutil.Fixture(t, "crates", "se", "rd", "serde")})
	mustCode(t, e2.GET(indexPath), 200)
	e2.Upstream.Set("/files/serde/serde-1.0.0.crate", testutil.Route{Body: testutil.Fixture(t, "crates", "serde-1.0.0.crate")})
	rec := e2.GET(download)
	mustCode(t, rec, 200)
	mustHeader(t, rec, "X-Larder-Verified", "1")

	bad := env.New(t, env.Options{Repo: cratesRepo(), CrateDLTemplates: []string{"/files/{crate}/{crate}-{version}.crate"}})
	bad.Upstream.Set("/se/rd/serde", testutil.Route{Body: testutil.Fixture(t, "crates", "se", "rd", "serde")})
	mustCode(t, bad.GET(indexPath), 200)
	bad.Upstream.Set("/files/serde/serde-1.0.0.crate", testutil.Route{Body: []byte("substituted")})
	mustCode(t, bad.GET(download), 502)
	_ = download
}

func TestMatrix_R07_ManifestExpectations(t *testing.T) {
	e := env.New(t, env.Options{Repo: rustupRepo()})
	tar := []byte("rustc tar fixture")
	xz := []byte("rustc xz fixture")
	manifest := strings.ReplaceAll(string(testutil.Fixture(t, "rustup", "channel-rust-stable.toml")), "HASH_TAR", hashHex(tar))
	manifest = strings.ReplaceAll(manifest, "HASH_XZ", hashHex(xz))
	e.Upstream.Set("/dist/channel-rust-stable.toml", testutil.Route{Body: []byte(manifest)})
	mustCode(t, e.GET("/dist/channel-rust-stable.toml"), 200)
	tarPath := "/dist/2024-01-01/rustc-1.75.0-x86_64-unknown-linux-gnu.tar.gz"
	xzPath := "/dist/2024-01-01/rustc-1.75.0-x86_64-unknown-linux-gnu.tar.xz"
	e.Upstream.Set(tarPath, testutil.Route{Body: tar})
	e.Upstream.Set(xzPath, testutil.Route{Body: xz})
	mustHeader(t, e.GET(tarPath), "X-Larder-Verified", "1")
	mustHeader(t, e.GET(xzPath), "X-Larder-Verified", "1")
	e.Upstream.Set(tarPath+".sha256", testutil.Route{Body: []byte(strings.Repeat("f", 64))})
	checksum := e.GET(tarPath + ".sha256")
	mustCode(t, checksum, 200)
	if e.Metrics.VerifyFailures.With("rustup").Value() == 0 {
		t.Fatal("rustup checksum conflict was not counted")
	}

	bad := env.New(t, env.Options{Repo: rustupRepo()})
	bad.Upstream.Set("/dist/channel-rust-stable.toml", testutil.Route{Body: []byte(manifest)})
	mustCode(t, bad.GET("/dist/channel-rust-stable.toml"), 200)
	bad.Upstream.Set(tarPath, testutil.Route{Body: []byte("wrong")})
	mustCode(t, bad.GET(tarPath), 502)
}

func TestMatrix_A02_ReleaseIngest(t *testing.T) {
	e := env.New(t, env.Options{Repo: aptRepo()})
	release := testutil.Fixture(t, "apt", "Release")
	e.Upstream.Set("/dists/bookworm/Release", testutil.Route{Body: release})
	mustCode(t, e.GET("/dists/bookworm/Release"), 200)
	packages := testutil.Fixture(t, "apt", "Packages.gz")
	expectation, err := e.Index.GetExpectation(context.Background(), "apt", "/dists/bookworm/main/binary-amd64/Packages.gz", "sha256")
	if err != nil {
		t.Fatal(err)
	}
	if expectation.Digest != hashHex(packages) {
		t.Fatalf("Packages.gz expectation = %q, want %q", expectation.Digest, hashHex(packages))
	}

	inrelease := testutil.Fixture(t, "apt", "InRelease")
	e.Upstream.Set("/dists/bookworm/InRelease", testutil.Route{Body: inrelease})
	rec := e.GET("/dists/bookworm/InRelease")
	mustCode(t, rec, 200)
	if !bytes.Equal(rec.Body.Bytes(), inrelease) {
		t.Fatal("signed InRelease was rewritten")
	}
}

func TestMatrix_A04_PackagesAndDebVerification(t *testing.T) {
	e := env.New(t, env.Options{Repo: aptRepo()})
	release := testutil.Fixture(t, "apt", "Release")
	packages := testutil.Fixture(t, "apt", "Packages.gz")
	e.Upstream.Set("/dists/bookworm/Release", testutil.Route{Body: release})
	mustCode(t, e.GET("/dists/bookworm/Release"), 200)
	e.Upstream.Set("/dists/bookworm/main/binary-amd64/Packages.gz", testutil.Route{Body: packages})
	mustCode(t, e.GET("/dists/bookworm/main/binary-amd64/Packages.gz"), 200)
	expectation, err := e.Index.GetExpectation(context.Background(), "apt", "/pool/main/d/demo/demo_1.0_amd64.deb", "sha256")
	if err != nil {
		t.Fatal(err)
	}
	deb := []byte("fixture deb bytes")
	if expectation.Digest != hashHex(deb) {
		t.Fatalf("deb expectation = %q", expectation.Digest)
	}
	e.Upstream.Set("/pool/main/d/demo/demo_1.0_amd64.deb", testutil.Route{Body: deb})
	rec := e.GET("/pool/main/d/demo/demo_1.0_amd64.deb")
	mustCode(t, rec, 200)
	mustHeader(t, rec, "X-Larder-Verified", "1")

	bad := env.New(t, env.Options{Repo: aptRepo()})
	bad.Upstream.Set("/dists/bookworm/Release", testutil.Route{Body: release})
	mustCode(t, bad.GET("/dists/bookworm/Release"), 200)
	bad.Upstream.Set("/dists/bookworm/main/binary-amd64/Packages.gz", testutil.Route{Body: packages})
	mustCode(t, bad.GET("/dists/bookworm/main/binary-amd64/Packages.gz"), 200)
	bad.Upstream.Set("/pool/main/d/demo/demo_1.0_amd64.deb", testutil.Route{Body: []byte("wrong")})
	mustCode(t, bad.GET("/pool/main/d/demo/demo_1.0_amd64.deb"), 502)
}

func TestMatrix_A05_ByHash(t *testing.T) {
	e := env.New(t, env.Options{Repo: aptRepo()})
	packages := testutil.Fixture(t, "apt", "Packages.gz")
	digest := hashHex(packages)
	path := "/dists/bookworm/main/binary-amd64/by-hash/SHA256/" + digest
	e.Upstream.Set(path, testutil.Route{Body: packages})
	rec := e.GET(path)
	mustCode(t, rec, 200)
	mustHeader(t, rec, "X-Larder-Verified", "1")
	if !bytes.Equal(rec.Body.Bytes(), packages) {
		t.Fatal("by-hash body differs")
	}
}

func TestMatrix_A06_SuiteConsistency(t *testing.T) {
	e := env.New(t, env.Options{Repo: aptRepo()})
	release := testutil.Fixture(t, "apt", "Release")
	packages := testutil.Fixture(t, "apt", "Packages.gz")
	e.Upstream.Set("/dists/bookworm/Release", testutil.Route{Body: release})
	e.Upstream.Set("/dists/bookworm/main/binary-amd64/Packages.gz", testutil.Route{Body: packages})
	mustCode(t, e.GET("/dists/bookworm/Release"), 200)
	mustCode(t, e.GET("/dists/bookworm/main/binary-amd64/Packages.gz"), 200)
	before, err := e.Index.CurrentObject(context.Background(), "apt", "/dists/bookworm/main/binary-amd64/Packages.gz")
	if err != nil {
		t.Fatal(err)
	}
	e.Clock.Advance(301 * time.Second)
	newRelease := []byte(strings.Replace(string(release), "Origin: Larder Fixture", "Origin: Larder Fixture Updated", 1))
	e.Upstream.Set("/dists/bookworm/Release", testutil.Route{Body: newRelease})
	mustHeader(t, e.GET("/dists/bookworm/Release"), "X-Larder-Cache", "REVALIDATED")
	after, err := e.Index.CurrentObject(context.Background(), "apt", "/dists/bookworm/main/binary-amd64/Packages.gz")
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != before.ID {
		t.Fatal("Packages generation changed unexpectedly")
	}
	if !after.ValidatedAt.IsZero() {
		t.Fatal("Packages was not marked stale by the latest Release")
	}
}

func TestMatrix_A07_MismatchRetry(t *testing.T) {
	e := env.New(t, env.Options{Repo: aptRepo()})
	release := testutil.Fixture(t, "apt", "Release")
	packages := testutil.Fixture(t, "apt", "Packages.gz")
	e.Upstream.Set("/dists/bookworm/Release", testutil.Route{Body: release})
	e.Upstream.Set("/dists/bookworm/main/binary-amd64/Packages.gz", testutil.Route{Body: packages})
	mustCode(t, e.GET("/dists/bookworm/Release"), 200)
	mustCode(t, e.GET("/dists/bookworm/main/binary-amd64/Packages.gz"), 200)
	e.Clock.Advance(301 * time.Second)
	newPackages := []byte("Package: replacement\nFilename: pool/replacement.deb\nSHA256: " + strings.Repeat("c", 64) + "\n\n")
	newRelease := []byte(strings.Replace(string(release), hashHex(packages), hashHex(newPackages), 1))
	e.Upstream.Set("/dists/bookworm/Release", testutil.Route{Body: newRelease})
	e.Upstream.Set("/dists/bookworm/main/binary-amd64/Packages.gz", testutil.Route{Body: newPackages})
	rec := e.GET("/dists/bookworm/main/binary-amd64/Packages.gz")
	mustCode(t, rec, 200)
	if !bytes.Equal(rec.Body.Bytes(), newPackages) {
		t.Fatalf("retry did not serve the refreshed Packages: cache=%s body=%q up=%d", rec.Header().Get("X-Larder-Cache"), rec.Body.String(), e.Upstream.Count("/dists/bookworm/main/binary-amd64/Packages.gz"))
	}
	if e.Upstream.Count("/dists/bookworm/Release") != 2 {
		t.Fatalf("Release count = %d, want 2", e.Upstream.Count("/dists/bookworm/Release"))
	}
}

func TestMatrix_A07_MismatchStillStale(t *testing.T) {
	e := env.New(t, env.Options{Repo: aptRepo()})
	release := testutil.Fixture(t, "apt", "Release")
	packages := testutil.Fixture(t, "apt", "Packages.gz")
	e.Upstream.Set("/dists/bookworm/Release", testutil.Route{Body: release})
	e.Upstream.Set("/dists/bookworm/main/binary-amd64/Packages.gz", testutil.Route{Body: packages})
	mustCode(t, e.GET("/dists/bookworm/Release"), 200)
	mustCode(t, e.GET("/dists/bookworm/main/binary-amd64/Packages.gz"), 200)
	e.Clock.Advance(301 * time.Second)
	newRelease := []byte(strings.Replace(string(release), hashHex(packages), strings.Repeat("d", 64), 1))
	e.Upstream.Set("/dists/bookworm/Release", testutil.Route{Body: newRelease})
	e.Upstream.Set("/dists/bookworm/main/binary-amd64/Packages.gz", testutil.Route{Body: []byte("still mismatched")})
	rec := e.GET("/dists/bookworm/main/binary-amd64/Packages.gz")
	mustCode(t, rec, 200)
	mustHeader(t, rec, "X-Larder-Cache", "STALE")
	if !bytes.Equal(rec.Body.Bytes(), packages) {
		t.Fatal("stale Packages body differs")
	}
}

func TestMatrix_A09_OfflineIndexes(t *testing.T) {
	e := env.New(t, env.Options{Repo: aptRepo()})
	release := testutil.Fixture(t, "apt", "Release")
	packages := testutil.Fixture(t, "apt", "Packages.gz")
	e.Upstream.Set("/dists/bookworm/Release", testutil.Route{Body: release})
	e.Upstream.Set("/dists/bookworm/main/binary-amd64/Packages.gz", testutil.Route{Body: packages})
	mustCode(t, e.GET("/dists/bookworm/Release"), 200)
	mustCode(t, e.GET("/dists/bookworm/main/binary-amd64/Packages.gz"), 200)
	e.Clock.Advance(301 * time.Second)
	e.Engine.SetOffline(true)
	for _, path := range []string{"/dists/bookworm/Release", "/dists/bookworm/main/binary-amd64/Packages.gz"} {
		rec := e.GET(path)
		mustCode(t, rec, 200)
		mustHeader(t, rec, "X-Larder-Cache", "OFFLINE-HIT")
		mustHeader(t, rec, "X-Larder-Stale", "1")
	}
	if e.Upstream.Count("/dists/bookworm/Release") != 1 {
		t.Fatal("offline index contacted upstream")
	}
}

func TestMatrix_A08_SignedFilesUnchanged(t *testing.T) {
	e := env.New(t, env.Options{Repo: aptRepo()})
	for _, name := range []string{"Release", "InRelease", "Release.gpg"} {
		path := "/dists/bookworm/" + name
		body := testutil.Fixture(t, "apt", name)
		e.Upstream.Set(path, testutil.Route{Body: body})
		rec := e.GET(path)
		mustCode(t, rec, 200)
		if !bytes.Equal(rec.Body.Bytes(), body) {
			t.Fatalf("%s was modified in transit", name)
		}
	}
}
