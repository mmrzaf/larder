package cache_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/mod/sumdb/dirhash"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/testutil"
	"github.com/mmrzaf/larder/testutil/env"
)

func goRepo(opts *config.GoOptions) config.Repo {
	if opts == nil {
		opts = &config.GoOptions{Verify: "basic"}
	}
	return config.Repo{Name: "go", Kind: "go", Mount: "/go", Go: opts, ServeGone: true}
}

func mavenRepo(companion bool) config.Repo {
	return config.Repo{
		Name: "maven", Kind: "maven", Mount: "/maven", ServeGone: true,
		Maven: &config.MavenOptions{CompanionFetch: companion},
	}
}

func goZip(t *testing.T, escapedModule, version string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	prefix := escapedModule + "@" + version + "/"
	for name, body := range files {
		entry, err := writer.Create(prefix + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipDirHash(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "module.zip")
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatal(err)
	}
	hash, err := dirhash.HashZip(path, dirhash.Hash1)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(hash, "h1:")
}

func modDirHash(t *testing.T, mod []byte) string {
	t.Helper()
	hash, err := dirhash.Hash1([]string{"go.mod"}, func(string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(mod)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(hash, "h1:")
}

func lookupBody(t *testing.T, zipHash, modHash string) []byte {
	t.Helper()
	body := string(testutil.Fixture(t, "go", "sumdb-lookup.txt"))
	body = strings.ReplaceAll(body, "PLACEHOLDER_ZIP_HASH", zipHash)
	body = strings.ReplaceAll(body, "PLACEHOLDER_MOD_HASH", modHash)
	return []byte(body)
}

func TestMatrix_G02_ListAndLatestRevalidation(t *testing.T) {
	e := env.New(t, env.Options{Repo: goRepo(nil)})
	listPath := "/example.com/hello/@v/list"
	latestPath := "/example.com/hello/@latest"
	e.Upstream.Set(listPath, testutil.Route{Body: []byte("v1.0.0\n")})
	e.Upstream.Set(latestPath, testutil.Route{Body: []byte(`{"Version":"v1.0.0"}`)})

	mustHeader(t, e.GET(listPath), "X-Larder-Cache", "MISS")
	mustHeader(t, e.GET(latestPath), "X-Larder-Cache", "MISS")
	hit := e.GET(listPath)
	mustHeader(t, hit, "X-Larder-Cache", "HIT")
	if hit.Body.String() != "v1.0.0\n" {
		t.Fatalf("list hit = %q", hit.Body.String())
	}

	e.Clock.Advance(61 * time.Second)
	e.Upstream.Set(listPath, testutil.Route{Body: []byte("v1.0.0\nv1.1.0\n")})
	revalidated := e.GET(listPath)
	mustHeader(t, revalidated, "X-Larder-Cache", "REVALIDATED")
	if revalidated.Body.String() != "v1.0.0\nv1.1.0\n" {
		t.Fatalf("revalidated list = %q", revalidated.Body.String())
	}
	if count := e.Upstream.Count(listPath); count != 2 {
		t.Fatalf("list upstream count = %d, want 2", count)
	}
	mustHeader(t, e.GET(latestPath), "X-Larder-Cache", "REVALIDATED")
}

func TestMatrix_G03_ArtifactsMissThenHit(t *testing.T) {
	mod := testutil.Fixture(t, "go", "hello.mod")
	info := testutil.Fixture(t, "go", "hello.info")
	archive := goZip(t, "example.com/hello", "v1.0.0", map[string]string{
		"go.mod":   string(mod),
		"hello.go": "package hello\n",
	})
	e := env.New(t, env.Options{Repo: goRepo(nil)})
	paths := map[string][]byte{
		"/example.com/hello/@v/v1.0.0.info": info,
		"/example.com/hello/@v/v1.0.0.mod":  mod,
		"/example.com/hello/@v/v1.0.0.zip":  archive,
	}
	for path, body := range paths {
		e.Upstream.Set(path, testutil.Route{Body: body})
		mustHeader(t, e.GET(path), "X-Larder-Cache", "MISS")
		hit := e.GET(path)
		mustHeader(t, hit, "X-Larder-Cache", "HIT")
		if !bytes.Equal(hit.Body.Bytes(), body) {
			t.Fatalf("%s hit body differs", path)
		}
		if count := e.Upstream.Count(path); count != 1 {
			t.Fatalf("%s upstream count = %d", path, count)
		}
	}
}

func TestMatrix_G04_InvalidZipRejected(t *testing.T) {
	wrongPrefix := goZip(t, "other.com/hello", "v1.0.0", map[string]string{"go.mod": "module other.com/hello\n"})
	e := env.New(t, env.Options{Repo: goRepo(nil)})
	path := "/example.com/hello/@v/v1.0.0.zip"
	e.Upstream.Set(path, testutil.Route{Body: wrongPrefix})
	mustCode(t, e.GET(path), 502)
	if _, err := e.Index.CurrentObject(context.Background(), "go", path); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("wrong-prefix zip was stored: %v", err)
	}

	e2 := env.New(t, env.Options{Repo: goRepo(nil)})
	e2.Upstream.Set(path, testutil.Route{Body: []byte("PK\x03\x04truncated")})
	mustCode(t, e2.GET(path), 502)
	if e2.Metrics.VerifyFailures.With("go").Value() != 0 {
		t.Fatal("invalid zip was counted as a verification failure")
	}
}

func TestMatrix_G05_StatusPassthrough(t *testing.T) {
	path := "/example.com/hello/@v/v1.0.0.zip"

	notFound := env.New(t, env.Options{Repo: goRepo(nil)})
	notFound.Upstream.Set(path, testutil.Route{Status: http.StatusNotFound, Body: []byte("missing module\n")})
	first := notFound.GET(path)
	mustCode(t, first, http.StatusNotFound)
	if first.Body.String() != "missing module\n" {
		t.Fatalf("404 body = %q", first.Body.String())
	}
	before := notFound.Upstream.Count(path)
	second := notFound.GET(path)
	mustCode(t, second, http.StatusNotFound)
	if second.Body.String() != "missing module\n" {
		t.Fatalf("cached 404 body = %q", second.Body.String())
	}
	if notFound.Upstream.Count(path) != before {
		t.Fatal("negative cache contacted upstream")
	}

	gone := env.New(t, env.Options{Repo: goRepo(nil)})
	gone.Upstream.Set(path, testutil.Route{Status: http.StatusGone, Body: []byte("gone module\n")})
	first = gone.GET(path)
	mustCode(t, first, http.StatusGone)
	if first.Body.String() != "gone module\n" {
		t.Fatalf("410 body = %q", first.Body.String())
	}
	before = gone.Upstream.Count(path)
	cached := gone.GET(path)
	mustCode(t, cached, http.StatusGone)
	if gone.Upstream.Count(path) != before {
		t.Fatal("negative cache contacted upstream after 410")
	}

	mixed := env.New(t, env.Options{Repo: goRepo(nil), UpstreamCount: 2})
	mixed.Fakes[0].Set(path, testutil.Route{Status: http.StatusNotFound, Body: []byte("first\n")})
	mixed.Fakes[1].Set(path, testutil.Route{Status: http.StatusGone, Body: []byte("second\n")})
	rec := mixed.GET(path)
	mustCode(t, rec, http.StatusGone)
	if rec.Body.String() != "second\n" {
		t.Fatalf("mixed body = %q", rec.Body.String())
	}
}

func TestMatrix_G06_SumDB(t *testing.T) {
	e := env.New(t, env.Options{Repo: goRepo(nil)})
	paths := map[string][]byte{
		"/sumdb/sum.golang.org/supported":                       []byte("supported"),
		"/sumdb/sum.golang.org/latest":                          []byte("latest"),
		"/sumdb/sum.golang.org/lookup/example.com/hello@v1.0.0": testutil.Fixture(t, "go", "sumdb-lookup.txt"),
		"/sumdb/sum.golang.org/tile/8/0/000":                    testutil.Fixture(t, "go", "sumdb-tile.txt"),
	}
	for path, body := range paths {
		e.Upstream.Set(path, testutil.Route{Body: body})
		mustHeader(t, e.GET(path), "X-Larder-Cache", "MISS")
		hit := e.GET(path)
		mustHeader(t, hit, "X-Larder-Cache", "HIT")
		if !bytes.Equal(hit.Body.Bytes(), body) {
			t.Fatalf("%s body differs", path)
		}
	}
}

func TestMatrix_G07_StrictVerification(t *testing.T) {
	archive := goZip(t, "example.com/hello", "v1.0.0", map[string]string{
		"go.mod":   string(testutil.Fixture(t, "go", "hello.mod")),
		"hello.go": "package hello\n",
	})
	mod := testutil.Fixture(t, "go", "hello.mod")
	zipHash := zipDirHash(t, archive)
	modHash := modDirHash(t, mod)
	lookupPath := "/lookup/example.com/hello@v1.0.0"
	artifactPath := "/example.com/hello/@v/v1.0.0.zip"

	sumdb := testutil.NewFakeUpstream(t)
	sumdb.Set(lookupPath, testutil.Route{Body: lookupBody(t, zipHash, modHash)})
	repo := goRepo(&config.GoOptions{
		Verify:         "strict",
		SumDBUpstreams: map[string][]string{"sum.golang.org": {sumdb.URL()}},
	})
	e := env.New(t, env.Options{Repo: repo})
	e.Upstream.Set(artifactPath, testutil.Route{Body: archive})
	rec := e.GET(artifactPath)
	mustCode(t, rec, http.StatusOK)
	mustHeader(t, rec, "X-Larder-Verified", "1")
	expectation, err := e.Index.GetExpectation(context.Background(), "go", artifactPath, "h1")
	if err != nil {
		t.Fatalf("h1 expectation was not stored: %v", err)
	}
	if expectation.Digest != zipHash {
		t.Fatalf("h1 expectation = %q, want %q", expectation.Digest, zipHash)
	}

	modPath := "/example.com/hello/@v/v1.0.0.mod"
	e.Upstream.Set(modPath, testutil.Route{Body: mod})
	modRec := e.GET(modPath)
	mustCode(t, modRec, http.StatusOK)
	mustHeader(t, modRec, "X-Larder-Verified", "1")
	modExpectation, err := e.Index.GetExpectation(context.Background(), "go", modPath, "h1")
	if err != nil || modExpectation.Digest != modHash {
		t.Fatalf("go.mod h1 expectation = %#v, %v; want %q", modExpectation, err, modHash)
	}

	wrongSum := sha256.Sum256([]byte("other content"))
	wrongBody := lookupBody(t, base64.StdEncoding.EncodeToString(wrongSum[:]), modHash)
	badSumdb := testutil.NewFakeUpstream(t)
	badSumdb.Set(lookupPath, testutil.Route{Body: wrongBody})
	badRepo := goRepo(&config.GoOptions{
		Verify:         "strict",
		SumDBUpstreams: map[string][]string{"sum.golang.org": {badSumdb.URL()}},
	})
	bad := env.New(t, env.Options{Repo: badRepo})
	bad.Upstream.Set(artifactPath, testutil.Route{Body: archive})
	mustCode(t, bad.GET(artifactPath), 502)
	if _, err := bad.Index.CurrentObject(context.Background(), "go", artifactPath); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("mismatched object was stored: %v", err)
	}
	if bad.Metrics.VerifyFailures.With("go").Value() == 0 {
		t.Fatal("mismatch did not increment verify failures")
	}

	lateSumdb := testutil.NewFakeUpstream(t)
	lateSumdb.Set(lookupPath, testutil.Route{Status: http.StatusNotFound})
	lateRepo := goRepo(&config.GoOptions{
		Verify:         "strict",
		SumDBUpstreams: map[string][]string{"sum.golang.org": {lateSumdb.URL()}},
	})
	late := env.New(t, env.Options{Repo: lateRepo})
	late.Upstream.Set(artifactPath, testutil.Route{Body: archive})
	first := late.GET(artifactPath)
	mustCode(t, first, http.StatusOK)
	mustHeader(t, first, "X-Larder-Verified", "0")
	lateSumdb.Set(lookupPath, testutil.Route{Body: lookupBody(t, zipHash, modHash)})
	if err := late.Engine.RetryUnverified(context.Background()); err != nil {
		t.Fatal(err)
	}
	object, err := late.Index.CurrentObject(context.Background(), "go", artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if !object.Verified {
		t.Fatal("late lookup did not verify the object")
	}
}

func TestMatrix_G08_CaseEncodedPath(t *testing.T) {
	e := env.New(t, env.Options{Repo: goRepo(nil)})
	path := "/example.com/!hello/@v/v1.0.0.info"
	e.Upstream.Set(path, testutil.Route{Body: testutil.Fixture(t, "go", "hello.info")})
	rec := e.GET(path)
	mustCode(t, rec, http.StatusOK)
	mustHeader(t, rec, "X-Larder-Key", "go:"+path)
	object, err := e.Index.CurrentObject(context.Background(), "go", path)
	if err != nil {
		t.Fatal(err)
	}
	if object.Key != path {
		t.Fatalf("stored key = %q", object.Key)
	}
}

func TestMatrix_M02_CompanionVerification(t *testing.T) {
	jarPath := "/org/example/artifact/1.2.3/artifact-1.2.3.jar"
	jar := testutil.Fixture(t, "maven", "artifact-1.2.3.jar")
	sha512 := testutil.Fixture(t, "maven", "artifact-1.2.3.jar.sha512")
	sha256Body := testutil.Fixture(t, "maven", "artifact-1.2.3.jar.sha256")

	match := env.New(t, env.Options{Repo: mavenRepo(true)})
	match.Upstream.Set(jarPath, testutil.Route{Body: jar})
	match.Upstream.Set(jarPath+".sha512", testutil.Route{Body: sha512})
	rec := match.GET(jarPath)
	mustCode(t, rec, http.StatusOK)
	mustHeader(t, rec, "X-Larder-Verified", "1")
	if match.Upstream.Count(jarPath+".sha512") != 1 || match.Upstream.Count(jarPath+".sha256") != 0 {
		t.Fatalf("companion order counts: sha512=%d sha256=%d", match.Upstream.Count(jarPath+".sha512"), match.Upstream.Count(jarPath+".sha256"))
	}
	companion := match.GET(jarPath + ".sha512")
	mustHeader(t, companion, "X-Larder-Cache", "HIT")
	if !bytes.Equal(companion.Body.Bytes(), sha512) {
		t.Fatalf("companion body = %q", companion.Body.String())
	}
	if match.Upstream.Count(jarPath+".sha512") != 1 {
		t.Fatal("companion request contacted upstream after it was stored")
	}

	fallback := env.New(t, env.Options{Repo: mavenRepo(true)})
	fallback.Upstream.Set(jarPath, testutil.Route{Body: jar})
	fallback.Upstream.Set(jarPath+".sha256", testutil.Route{Body: sha256Body})
	mustHeader(t, fallback.GET(jarPath), "X-Larder-Verified", "1")
	if fallback.Upstream.Count(jarPath+".sha512") != 1 || fallback.Upstream.Count(jarPath+".sha256") != 1 {
		t.Fatal("companion fallback did not try sha512 then sha256")
	}

	mismatch := env.New(t, env.Options{Repo: mavenRepo(true)})
	mismatch.Upstream.Set(jarPath, testutil.Route{Body: jar})
	mismatch.Upstream.Set(jarPath+".sha512", testutil.Route{Body: []byte(strings.Repeat("00", 64))})
	mustCode(t, mismatch.GET(jarPath), 502)
	if mismatch.Upstream.Count(jarPath+".sha256") != 0 {
		t.Fatal("mismatch continued to the next companion instead of failing the candidate")
	}
	if mismatch.Metrics.VerifyFailures.With("maven").Value() == 0 {
		t.Fatal("mismatch did not increment verify failures")
	}

	absent := env.New(t, env.Options{Repo: mavenRepo(true)})
	absent.Upstream.Set(jarPath, testutil.Route{Body: jar})
	rec = absent.GET(jarPath)
	mustCode(t, rec, http.StatusOK)
	mustHeader(t, rec, "X-Larder-Verified", "0")
}

func TestMatrix_M03_CompanionNetworkError(t *testing.T) {
	jarPath := "/org/example/artifact/1.2.3/artifact-1.2.3.jar"
	e := env.New(t, env.Options{Repo: mavenRepo(true)})
	e.Upstream.Set(jarPath, testutil.Route{Body: testutil.Fixture(t, "maven", "artifact-1.2.3.jar")})
	e.Upstream.Set(jarPath+".sha512", testutil.Route{Close: true})
	rec := e.GET(jarPath)
	mustCode(t, rec, http.StatusOK)
	mustHeader(t, rec, "X-Larder-Verified", "0")
	if _, err := e.Index.CurrentObject(context.Background(), "maven", jarPath); err != nil {
		t.Fatalf("artifact was not stored: %v", err)
	}
}

func TestMatrix_M04_ChecksumConflict(t *testing.T) {
	jarPath := "/org/example/artifact/1.2.3/artifact-1.2.3.jar"
	checksumPath := jarPath + ".sha1"
	logs := &bytes.Buffer{}
	e := env.New(t, env.Options{Repo: mavenRepo(false), Logger: slog.New(slog.NewTextHandler(logs, nil))})
	e.Upstream.Set(jarPath, testutil.Route{Body: testutil.Fixture(t, "maven", "artifact-1.2.3.jar")})
	mustHeader(t, e.GET(jarPath), "X-Larder-Verified", "0")
	e.Upstream.Set(checksumPath, testutil.Route{Body: []byte(strings.Repeat("ab", 20) + "  artifact-1.2.3.jar\n")})
	rec := e.GET(checksumPath)
	mustCode(t, rec, http.StatusOK)
	if e.Metrics.VerifyFailures.With("maven").Value() == 0 {
		t.Fatal("checksum conflict was not counted")
	}
	if !strings.Contains(logs.String(), "digest_conflict") {
		t.Fatalf("digest_conflict was not logged:\n%s", logs.String())
	}
	before := e.Upstream.Count(checksumPath)
	cached := e.GET(checksumPath)
	mustHeader(t, cached, "X-Larder-Cache", "HIT")
	if e.Upstream.Count(checksumPath) != before {
		t.Fatal("stored checksum was fetched again")
	}
}

func TestMatrix_M05_MetadataRevalidation(t *testing.T) {
	path := "/org/example/artifact/maven-metadata.xml"
	e := env.New(t, env.Options{Repo: mavenRepo(true)})
	body := testutil.Fixture(t, "maven", "maven-metadata.xml")
	e.Upstream.Set(path, testutil.Route{Body: body, ETag: `"meta-1"`, Conditional: true})
	mustHeader(t, e.GET(path), "X-Larder-Cache", "MISS")
	mustHeader(t, e.GET(path), "X-Larder-Cache", "HIT")
	e.Clock.Advance(301 * time.Second)
	mustHeader(t, e.GET(path), "X-Larder-Cache", "REVALIDATED")
}

func TestMatrix_M06_HTML200Rejected(t *testing.T) {
	path := "/org/example/artifact/1.2.3/artifact-1.2.3.jar"
	e := env.New(t, env.Options{Repo: mavenRepo(true)})
	e.Upstream.Set(path, testutil.Route{HTML: true})
	mustCode(t, e.GET(path), 502)
	if _, err := e.Index.CurrentObject(context.Background(), "maven", path); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("HTML page was stored: %v", err)
	}
}

func TestMatrix_M07_GradleDistributionRedirect(t *testing.T) {
	repo := config.Repo{
		Name: "gradle-dist", Kind: "generic", Mount: "/gradle-dist", ServeGone: true,
		Generic: &config.GenericOptions{
			DefaultClass: "deny",
			RejectHTML:   true,
			Rules: []config.GenericRule{{
				Match: `^/distributions/gradle-[0-9][^/]*-(bin|all)\.zip(\.sha256)?$`,
				Class: "immutable",
			}},
		},
	}
	e := env.New(t, env.Options{Repo: repo})
	path := "/distributions/gradle-8.10-bin.zip"
	e.Upstream.Set(path, testutil.Route{Status: http.StatusFound, Header: http.Header{"Location": []string{"/downloads/gradle-8.10-bin.zip"}}})
	e.Upstream.Set("/downloads/gradle-8.10-bin.zip", testutil.Route{Body: []byte("gradle-distribution")})
	mustCode(t, e.GET(path), http.StatusOK)
	hit := e.GET(path)
	mustHeader(t, hit, "X-Larder-Cache", "HIT")
	if hit.Body.String() != "gradle-distribution" {
		t.Fatalf("hit body = %q", hit.Body.String())
	}
	if _, err := e.Index.CurrentObject(context.Background(), "gradle-dist", path); err != nil {
		t.Fatalf("redirect target was not cached under the original path: %v", err)
	}
}
