package cache_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/store"
	"github.com/mmrzaf/larder/testutil"
	"github.com/mmrzaf/larder/testutil/env"
)

func npmRepo() config.Repo {
	return config.Repo{
		Name: "npm", Kind: "npm", Mount: "/npm", ServeGone: true,
		NPM: &config.NPMOptions{Audit: "passthrough", AuditTimeout: config.Duration(3 * time.Second)},
	}
}

func pypiRepo() config.Repo {
	return config.Repo{
		Name: "pypi", Kind: "pypi", Mount: "/pypi", ServeGone: true,
		PyPI: &config.PyPIOptions{FilesHosts: []string{"files.pythonhosted.org"}},
	}
}

func fixtureWithHost(t *testing.T, parts []string, oldHost, newHost string) []byte {
	t.Helper()
	body := testutil.Fixture(t, parts...)
	return bytes.ReplaceAll(body, []byte(oldHost), []byte(newHost))
}

func TestMatrix_N02_PackumentVariants(t *testing.T) {
	e := env.New(t, env.Options{Repo: npmRepo()})
	e.Upstream.Set("/demo", testutil.Route{Body: testutil.Fixture(t, "npm", "demo.json"), Header: http.Header{"Content-Type": []string{"application/json"}}})
	full := e.GET("/demo")
	mustCode(t, full, 200)
	mustHeader(t, full, "X-Larder-Key", "npm:/demo#full")
	mustHeader(t, full, "Vary", "Accept")
	e.Upstream.Set("/demo", testutil.Route{
		Body:          testutil.Fixture(t, "npm", "demo-abbrev.json"),
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		RequireHeader: map[string]string{"Accept": "application/vnd.npm.install-v1+json"},
	})
	abbrev := e.Serve(http.MethodGet, "/demo", env.Header("Accept", "application/vnd.npm.install-v1+json"))
	mustCode(t, abbrev, 200)
	mustHeader(t, abbrev, "X-Larder-Key", "npm:/demo#abbrev")
	if abbrev.Body.String() == full.Body.String() {
		t.Fatal("variants returned the same bytes")
	}
	if _, err := e.Index.CurrentObject(context.Background(), "npm", "/demo#full"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Index.CurrentObject(context.Background(), "npm", "/demo#abbrev"); err != nil {
		t.Fatal(err)
	}
	if count := e.Upstream.Count("/demo"); count != 2 {
		t.Fatalf("upstream count = %d, want 2", count)
	}
}

func TestMatrix_N03_TarballRewriteOfficialHost(t *testing.T) {
	e := env.New(t, env.Options{Repo: npmRepo()})
	e.Upstream.Set("/demo", testutil.Route{Body: testutil.Fixture(t, "npm", "demo.json")})
	rec := e.GET("/demo")
	mustCode(t, rec, 200)
	want := `"tarball": "http://larder.test/npm/demo/-/demo-1.0.0.tgz"`
	if !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("rewritten tarball missing %q: %s", want, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "registry.npmjs.org") {
		t.Fatalf("official host remains: %s", rec.Body.String())
	}
}

func TestMatrix_N04_MirrorBasePath(t *testing.T) {
	e := env.New(t, env.Options{Repo: npmRepo(), UpstreamBasePath: "/mirror"})
	body := fixtureWithHost(t, []string{"npm", "mirror.json"}, "https://mirror.example/npm", e.Upstream.URL()+"/mirror")
	e.Upstream.Set("/mirror/demo", testutil.Route{Body: body})
	rec := e.GET("/demo")
	mustCode(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `"tarball": "http://larder.test/npm/demo/-/demo-1.0.0.tgz"`) {
		t.Fatalf("mirror tarball was not rewritten: %s", rec.Body.String())
	}
	e.Upstream.Set("/mirror/demo/-/demo-1.0.0.tgz", testutil.Route{Body: testutil.Fixture(t, "npm", "demo-1.0.0.tgz")})
	tarball := e.GET("/demo/-/demo-1.0.0.tgz")
	mustCode(t, tarball, 200)
	if !bytes.Equal(tarball.Body.Bytes(), testutil.Fixture(t, "npm", "demo-1.0.0.tgz")) {
		t.Fatal("tarball body differs")
	}
	if e.Upstream.Count("/mirror/demo/-/demo-1.0.0.tgz") != 1 {
		t.Fatal("tarball did not use the mirror base path")
	}
}

func serveHost(e *env.Environment, host, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://"+host+path, nil)
	e.Engine.Handle(context.Background(), &e.Repo, rec, req)
	return rec
}

func TestMatrix_N05_PublicBaseAndETag(t *testing.T) {
	e := env.New(t, env.Options{Repo: npmRepo()})
	e.Upstream.Set("/demo", testutil.Route{Body: testutil.Fixture(t, "npm", "demo.json")})
	first := serveHost(e, "a.example", "/demo")
	mustCode(t, first, 200)
	if !strings.Contains(first.Body.String(), "http://a.example/npm/demo/-/demo-1.0.0.tgz") {
		t.Fatalf("first body = %s", first.Body.String())
	}
	second := serveHost(e, "b.example", "/demo")
	mustCode(t, second, 200)
	if !strings.Contains(second.Body.String(), "http://b.example/npm/demo/-/demo-1.0.0.tgz") {
		t.Fatalf("second body = %s", second.Body.String())
	}
	if first.Header().Get("ETag") == second.Header().Get("ETag") {
		t.Fatal("derived ETag did not change with the public base")
	}
	object, err := e.Index.CurrentObject(context.Background(), "npm", "/demo#full")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := e.Index.GetBlob(context.Background(), object.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := e.Store.Open(object.SHA256, store.Encoding(blob.StoredEnc))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := io.ReadAll(raw)
	_ = raw.Close()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "a.example") || !strings.Contains(string(stored), "registry.npmjs.org") {
		t.Fatal("stored packument was modified")
	}
}

func TestMatrix_N06_IntegrityFromPackument(t *testing.T) {
	e := env.New(t, env.Options{Repo: npmRepo()})
	e.Upstream.Set("/demo", testutil.Route{Body: testutil.Fixture(t, "npm", "demo.json")})
	mustCode(t, e.GET("/demo"), 200)
	tarballPath := "/demo/-/demo-1.0.0.tgz"
	e.Upstream.Set(tarballPath, testutil.Route{Body: testutil.Fixture(t, "npm", "demo-1.0.0.tgz")})
	rec := e.GET(tarballPath)
	mustCode(t, rec, 200)
	mustHeader(t, rec, "X-Larder-Verified", "1")

	bad := env.New(t, env.Options{Repo: npmRepo()})
	bad.Upstream.Set("/demo", testutil.Route{Body: testutil.Fixture(t, "npm", "demo.json")})
	mustCode(t, bad.GET("/demo"), 200)
	bad.Upstream.Set(tarballPath, testutil.Route{Body: []byte("substituted")})
	mustCode(t, bad.GET(tarballPath), 502)
	if bad.Metrics.VerifyFailures.With("npm").Value() == 0 {
		t.Fatal("mismatch did not increment verify failures")
	}
}

func TestMatrix_N07_TarballBeforePackument(t *testing.T) {
	e := env.New(t, env.Options{Repo: npmRepo()})
	tarballPath := "/demo/-/demo-1.0.0.tgz"
	e.Upstream.Set(tarballPath, testutil.Route{Body: testutil.Fixture(t, "npm", "demo-1.0.0.tgz")})
	first := e.GET(tarballPath)
	mustCode(t, first, 200)
	mustHeader(t, first, "X-Larder-Verified", "0")
	e.Upstream.Set("/demo", testutil.Route{Body: testutil.Fixture(t, "npm", "demo.json")})
	mustCode(t, e.GET("/demo"), 200)
	object, err := e.Index.CurrentObject(context.Background(), "npm", tarballPath)
	if err != nil {
		t.Fatal(err)
	}
	if !object.Verified {
		t.Fatal("tarball was not verified when the packument arrived")
	}
}

func TestMatrix_N08_AuditModes(t *testing.T) {
	e := env.New(t, env.Options{Repo: npmRepo()})
	auditPath := "/-/npm/v1/security/audits"
	e.Upstream.Set(auditPath, testutil.Route{Body: []byte(`{"auditReportVersion":2}`), RequireBody: `{"name":"demo"}`})
	rec := e.ServeBody(http.MethodPost, auditPath, env.Header("Content-Type", "application/json"), strings.NewReader(`{"name":"demo"}`))
	mustCode(t, rec, 200)
	mustHeader(t, rec, "X-Larder-Cache", "BYPASS")
	if rec.Body.String() != `{"auditReportVersion":2}` {
		t.Fatalf("audit body = %q", rec.Body.String())
	}
	e.Upstream.Set(auditPath, testutil.Route{Status: http.StatusInternalServerError})
	mustCode(t, e.ServeBody(http.MethodPost, auditPath, nil, strings.NewReader("{}")), http.StatusServiceUnavailable)
	e.Engine.SetOffline(true)
	mustCode(t, e.ServeBody(http.MethodPost, auditPath, nil, strings.NewReader("{}")), http.StatusServiceUnavailable)
	e.Engine.SetOffline(false)

	stubRepo := npmRepo()
	stubRepo.NPM.Audit = "stub"
	stub := env.New(t, env.Options{Repo: stubRepo})
	rec = stub.ServeBody(http.MethodPost, auditPath, nil, strings.NewReader("{}"))
	mustCode(t, rec, 200)
	if rec.Body.String() != "{}" || stub.Upstream.Total() != 0 {
		t.Fatalf("stub audit = %s, upstream %d", rec.Body.String(), stub.Upstream.Total())
	}

	denyRepo := npmRepo()
	denyRepo.NPM.Audit = "deny"
	deny := env.New(t, env.Options{Repo: denyRepo})
	mustCode(t, deny.ServeBody(http.MethodPost, auditPath, nil, strings.NewReader("{}")), 404)
}

func TestMatrix_N09_Ping(t *testing.T) {
	e := env.New(t, env.Options{Repo: npmRepo()})
	rec := e.GET("/-/ping")
	mustCode(t, rec, 200)
	if rec.Body.String() != "{}" {
		t.Fatalf("ping body = %q", rec.Body.String())
	}
	mustHeader(t, rec, "X-Larder-Cache", "BYPASS")
	if e.Upstream.Total() != 0 {
		t.Fatal("ping contacted upstream")
	}
}

func TestMatrix_N10_LargePackument(t *testing.T) {
	var versions strings.Builder
	versions.WriteString(`{"name":"demo","versions":{`)
	for i := 0; i < 20000; i++ {
		if i > 0 {
			versions.WriteByte(',')
		}
		fmt.Fprintf(&versions, `"1.0.%d":{"dist":{"tarball":"https://registry.npmjs.org/demo/-/demo-1.0.%d.tgz","integrity":"sha512-%s"}}`, i, i, strings.Repeat("A", 86)+"==")
	}
	versions.WriteString(`}}`)
	body := []byte(versions.String())
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	e := env.New(t, env.Options{Repo: npmRepo()})
	e.Upstream.Set("/demo", testutil.Route{Body: body})
	rec := e.GET("/demo")
	mustCode(t, rec, 200)
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if after.HeapAlloc > before.HeapAlloc+64<<20 {
		t.Fatalf("heap grew from %d to %d", before.HeapAlloc, after.HeapAlloc)
	}
}

func TestMatrix_N11_Unpublished(t *testing.T) {
	e := env.New(t, env.Options{Repo: npmRepo()})
	body := testutil.Fixture(t, "npm", "demo.json")
	e.Upstream.Set("/demo", testutil.Route{Body: body})
	first := e.GET("/demo")
	mustCode(t, first, 200)
	e.Clock.Advance(61 * time.Second)
	e.Upstream.Set("/demo", testutil.Route{Status: http.StatusNotFound, Body: []byte("unpublished\n")})
	rec := e.GET("/demo")
	mustCode(t, rec, 200)
	mustHeader(t, rec, "X-Larder-Cache", "GONE")
	if !strings.Contains(rec.Body.String(), `"name": "demo"`) {
		t.Fatalf("stored packument was not served for the unpublished version: %s", rec.Body.String())
	}
}

func TestMatrix_P02_NormalizationRedirect(t *testing.T) {
	e := env.New(t, env.Options{Repo: pypiRepo()})
	rec := e.GET("/simple/Demo_Name/")
	mustCode(t, rec, http.StatusMovedPermanently)
	mustHeader(t, rec, "Location", "/pypi/simple/demo-name/")
	if e.Upstream.Total() != 0 {
		t.Fatal("redirect contacted upstream")
	}
}

func TestMatrix_P03_HTMLRewrite(t *testing.T) {
	e := env.New(t, env.Options{Repo: pypiRepo()})
	e.Upstream.Set("/simple/demo/", testutil.Route{
		Body:   testutil.Fixture(t, "pypi", "demo.html"),
		Header: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
	})
	rec := e.GET("/simple/demo/")
	mustCode(t, rec, 200)
	body := rec.Body.String()
	if !strings.Contains(body, "http://larder.test/pypi/_files/files.pythonhosted.org/packages/demo-1.0.0-py3-none-any.whl#sha256=") {
		t.Fatalf("absolute URL was not rewritten: %s", body)
	}
	if !strings.Contains(body, `href="../../packages/demo-1.0.0.tar.gz#sha256=`) {
		t.Fatalf("relative URL was changed: %s", body)
	}
}

func TestMatrix_P04_JSONRewrite(t *testing.T) {
	e := env.New(t, env.Options{Repo: pypiRepo()})
	e.Upstream.Set("/simple/demo/", testutil.Route{
		Body:   testutil.Fixture(t, "pypi", "demo.json"),
		Header: http.Header{"Content-Type": []string{"application/vnd.pypi.simple.v1+json"}},
	})
	rec := e.Serve(http.MethodGet, "/simple/demo/", env.Header("Accept", "application/vnd.pypi.simple.v1+json"))
	mustCode(t, rec, 200)
	body := rec.Body.String()
	if !strings.Contains(body, `"url": "http://larder.test/pypi/_files/files.pythonhosted.org/packages/demo-1.0.0-py3-none-any.whl#sha256=`) {
		t.Fatalf("JSON URL was not rewritten: %s", body)
	}
	if !strings.Contains(body, `"url": "../../packages/demo-1.0.0.tar.gz#sha256=`) {
		t.Fatalf("relative JSON URL was changed: %s", body)
	}
}

func pypiHostFixture(t *testing.T, e *env.Environment, name string) []byte {
	t.Helper()
	host := "http://" + strings.TrimPrefix(e.Upstream.URL(), "http://")
	return fixtureWithHost(t, []string{"pypi", name}, "https://files.pythonhosted.org", host)
}

func TestMatrix_P05_FilesVerified(t *testing.T) {
	e := env.New(t, env.Options{Repo: pypiRepo()})
	html := pypiHostFixture(t, e, "demo.html")
	e.Upstream.Set("/simple/demo/", testutil.Route{Body: html, Header: http.Header{"Content-Type": []string{"text/html"}}})
	mustCode(t, e.GET("/simple/demo/"), 200)
	path := "/_files/" + strings.TrimPrefix(e.Upstream.URL(), "http://") + "/packages/demo-1.0.0-py3-none-any.whl"
	e.Upstream.Set("/packages/demo-1.0.0-py3-none-any.whl", testutil.Route{Body: testutil.Fixture(t, "pypi", "demo-1.0.0-py3-none-any.whl")})
	rec := e.GET(path)
	mustCode(t, rec, 200)
	mustHeader(t, rec, "X-Larder-Verified", "1")
	if !bytes.Equal(rec.Body.Bytes(), testutil.Fixture(t, "pypi", "demo-1.0.0-py3-none-any.whl")) {
		t.Fatal("wheel body differs")
	}
}

func TestMatrix_P06_DisallowedHost(t *testing.T) {
	e := env.New(t, env.Options{Repo: pypiRepo()})
	for _, host := range []string{"evil.example", "files.pythonhosted.org.evil.example"} {
		rec := e.GET("/_files/" + host + "/packages/demo.whl")
		mustCode(t, rec, http.StatusForbidden)
	}
	if e.Upstream.Total() != 0 {
		t.Fatal("disallowed host contacted upstream")
	}
}

func TestMatrix_P07_MetadataExpectation(t *testing.T) {
	e := env.New(t, env.Options{Repo: pypiRepo()})
	html := pypiHostFixture(t, e, "demo.html")
	e.Upstream.Set("/simple/demo/", testutil.Route{Body: html, Header: http.Header{"Content-Type": []string{"text/html"}}})
	mustCode(t, e.GET("/simple/demo/"), 200)
	host := strings.TrimPrefix(e.Upstream.URL(), "http://")
	path := "/_files/" + host + "/packages/demo-1.0.0-py3-none-any.whl.metadata"
	e.Upstream.Set("/packages/demo-1.0.0-py3-none-any.whl.metadata", testutil.Route{Body: testutil.Fixture(t, "pypi", "demo-1.0.0-py3-none-any.whl.metadata")})
	rec := e.GET(path)
	mustCode(t, rec, 200)
	mustHeader(t, rec, "X-Larder-Verified", "1")
}

func TestMatrix_P08_Variants(t *testing.T) {
	e := env.New(t, env.Options{Repo: pypiRepo()})
	e.Upstream.Set("/simple/demo/", testutil.Route{
		Body:   testutil.Fixture(t, "pypi", "demo.html"),
		Header: http.Header{"Content-Type": []string{"text/html"}},
	})
	html := e.GET("/simple/demo/")
	mustCode(t, html, 200)
	e.Upstream.Set("/simple/demo/", testutil.Route{
		Body:          testutil.Fixture(t, "pypi", "demo.json"),
		Header:        http.Header{"Content-Type": []string{"application/vnd.pypi.simple.v1+json"}},
		RequireHeader: map[string]string{"Accept": "application/vnd.pypi.simple.v1+json"},
	})
	jsonRec := e.Serve(http.MethodGet, "/simple/demo/", env.Header("Accept", "application/vnd.pypi.simple.v1+json"))
	mustCode(t, jsonRec, 200)
	if _, err := e.Index.CurrentObject(context.Background(), "pypi", "/simple/demo/#html"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Index.CurrentObject(context.Background(), "pypi", "/simple/demo/#json"); err != nil {
		t.Fatal(err)
	}
	if e.Upstream.Count("/simple/demo/") != 2 {
		t.Fatalf("upstream count = %d", e.Upstream.Count("/simple/demo/"))
	}
	mustHeader(t, html, "Vary", "Accept")
}

func TestMatrix_P09_RootIndexGzipAndTTL(t *testing.T) {
	e := env.New(t, env.Options{Repo: pypiRepo()})
	body := []byte("<!DOCTYPE html><html><body>" + strings.Repeat("<a href=\"/simple/demo/\">demo</a>", 4000) + "</body></html>")
	e.Upstream.Set("/simple/", testutil.Route{Body: body, Header: http.Header{"Content-Type": []string{"text/html"}}})
	rec := e.GET("/simple/")
	mustCode(t, rec, 200)
	object, err := e.Index.CurrentObject(context.Background(), "pypi", "/simple/#html")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := e.Index.GetBlob(context.Background(), object.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if blob.StoredEnc != "gzip" || blob.StoredSize >= blob.RawSize {
		t.Fatalf("root index was not compressed: %#v", blob)
	}
	e.Clock.Advance(61 * time.Second)
	hit := e.GET("/simple/")
	mustHeader(t, hit, "X-Larder-Cache", "HIT")
	if e.Upstream.Count("/simple/") != 1 {
		t.Fatal("root index was revalidated before its TTL")
	}
	identity := e.Serve(http.MethodGet, "/simple/", env.Header("Accept-Encoding", "gzip"))
	mustCode(t, identity, 200)
	if identity.Body.Len() != len(body) {
		t.Fatalf("served root index length = %d, want %d", identity.Body.Len(), len(body))
	}
}

func TestMatrix_P06_FileRedirectRestricted(t *testing.T) {
	e := env.New(t, env.Options{Repo: pypiRepo()})
	host := strings.TrimPrefix(e.Upstream.URL(), "http://")
	path := "/_files/" + host + "/packages/redirect.whl"
	e.Upstream.Set("/packages/redirect.whl", testutil.Route{
		Status: http.StatusFound,
		Header: http.Header{"Location": []string{"http://evil.invalid/packages/redirect.whl"}},
	})
	mustCode(t, e.GET(path), http.StatusBadGateway)
}

func TestMatrix_N13_AuditForwardsCompressionHeader(t *testing.T) {
	e := env.New(t, env.Options{Repo: npmRepo()})
	auditPath := "/-/npm/v1/security/advisories/bulk"
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write([]byte(`{"demo":["1.0.0"]}`)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	e.Upstream.Set(auditPath, testutil.Route{
		Body:          []byte(`{}`),
		RequireHeader: map[string]string{"Content-Encoding": "gzip"},
		RequireBody:   compressed.String(),
	})
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Content-Encoding", "gzip")
	rec := e.ServeBody(http.MethodPost, auditPath, headers, bytes.NewReader(compressed.Bytes()))
	mustCode(t, rec, 200)
}
