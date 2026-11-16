package cache_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mmrzaf/larder/internal/cache"
	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/kinds"
	"github.com/mmrzaf/larder/testutil"
	"github.com/mmrzaf/larder/testutil/env"
)

func genericRepo(rules []config.GenericRule, defaultClass string) config.Repo {
	return config.Repo{
		Name: "generic", Kind: "generic", Mount: "/generic",
		ServeGone: true, FreshTTL: config.Duration(300 * time.Second),
		Generic: &config.GenericOptions{
			DefaultClass: defaultClass,
			RejectHTML:   true,
			Rules:        rules,
		},
	}
}

func immutableRule(pattern string) []config.GenericRule {
	return []config.GenericRule{{Match: pattern, Class: "immutable"}}
}

func testRepo() config.Repo {
	return config.Repo{
		Name: "test", Kind: "test", Mount: "/test",
		ServeGone: true, FreshTTL: config.Duration(60 * time.Second),
	}
}

func mustCode(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d; %s", rec.Code, want, env.Summary(rec))
	}
}

func mustHeader(t *testing.T, rec *httptest.ResponseRecorder, name, want string) {
	t.Helper()
	if got := rec.Header().Get(name); got != want {
		t.Fatalf("%s = %q, want %q; %s", name, got, want, env.Summary(rec))
	}
}

func TestMatrix_C01_ImmutableMissThenHit(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/file$`), "deny")})
	e.Upstream.Set("/file", testutil.Route{Body: []byte("artifact")})

	first := e.GET("/file")
	mustCode(t, first, 200)
	if first.Body.String() != "artifact" {
		t.Fatalf("body = %q", first.Body.String())
	}
	mustHeader(t, first, "X-Larder-Cache", "MISS")
	mustHeader(t, first, "X-Larder-Key", "generic:/file")
	mustHeader(t, first, "Cache-Control", "public, max-age=31536000, immutable")

	second := e.GET("/file")
	mustCode(t, second, 200)
	if second.Body.String() != "artifact" {
		t.Fatalf("hit body = %q", second.Body.String())
	}
	mustHeader(t, second, "X-Larder-Cache", "HIT")
	if count := e.Upstream.Count("/file"); count != 1 {
		t.Fatalf("upstream count = %d, want 1", count)
	}
}

func TestMatrix_C02_ImmutableHitWithUpstreamStopped(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/file$`), "deny")})
	e.Upstream.Set("/file", testutil.Route{Body: []byte("artifact")})
	mustCode(t, e.GET("/file"), 200)
	e.Upstream.ResetCounters()
	e.Upstream.Set("/file", testutil.Route{Status: http.StatusInternalServerError})

	rec := e.GET("/file")
	mustCode(t, rec, 200)
	mustHeader(t, rec, "X-Larder-Cache", "HIT")
	if count := e.Upstream.Count("/file"); count != 0 {
		t.Fatalf("hit contacted upstream %d times", count)
	}
}

func TestMatrix_C03_Range(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/file$`), "deny")})
	e.Upstream.Set("/file", testutil.Route{Body: []byte("0123456789")})
	mustCode(t, e.GET("/file"), 200)

	rec := e.Serve(http.MethodGet, "/file", env.Header("Range", "bytes=2-5"))
	mustCode(t, rec, 206)
	if rec.Body.String() != "2345" {
		t.Fatalf("range body = %q", rec.Body.String())
	}
	mustHeader(t, rec, "Content-Range", "bytes 2-5/10")
	mustHeader(t, rec, "Content-Length", "4")
}

func TestMatrix_C04_IfNoneMatch(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/file$`), "deny")})
	e.Upstream.Set("/file", testutil.Route{Body: []byte("artifact"), ETag: `"v1"`})
	mustCode(t, e.GET("/file"), 200)

	rec := e.Serve(http.MethodGet, "/file", env.Header("If-None-Match", `"v1"`))
	mustCode(t, rec, 304)
	if rec.Body.Len() != 0 {
		t.Fatalf("304 body = %q", rec.Body.String())
	}
}

func TestMatrix_C05_ConcurrentMisses(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/big$`), "deny")})
	body := strings.Repeat("x", 4096)
	e.Upstream.Set("/big", testutil.Route{Body: []byte(body)})

	var wg sync.WaitGroup
	results := make([]*httptest.ResponseRecorder, 20)
	for i := 0; i < len(results); i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = e.GET("/big")
		}(i)
	}
	wg.Wait()
	for i, rec := range results {
		if rec.Code != 200 || rec.Body.String() != body {
			t.Fatalf("request %d = %s", i, env.Summary(rec))
		}
	}
	if count := e.Upstream.Count("/big"); count != 1 {
		t.Fatalf("upstream count = %d, want 1", count)
	}
}

type failingResponseWriter struct {
	header    http.Header
	status    int
	written   int
	failAfter int
}

func (w *failingResponseWriter) Header() http.Header { return w.header }
func (w *failingResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *failingResponseWriter) Write(p []byte) (int, error) {
	if w.written >= w.failAfter {
		return 0, errors.New("client disconnected")
	}
	n := len(p)
	if w.written+n > w.failAfter {
		n = w.failAfter - w.written
	}
	w.written += n
	return n, nil
}

func TestMatrix_C06_ClientDisconnectSmallObject(t *testing.T) {
	e := env.New(t, env.Options{
		Repo:                  genericRepo(immutableRule(`^/file$`), "deny"),
		BackgroundCompleteMax: config.Size(2 << 20),
	})
	e.Upstream.Set("/file", testutil.Route{Body: []byte(strings.Repeat("s", 1<<20))})
	writer := &failingResponseWriter{header: make(http.Header), failAfter: 10}
	req := httptest.NewRequest(http.MethodGet, "http://larder.test/file", nil)
	e.Engine.Handle(context.Background(), &e.Repo, writer, req)

	object, err := e.Index.CurrentObject(context.Background(), "generic", "/file")
	if err != nil {
		t.Fatalf("small object was not committed: %v", err)
	}
	if object.SHA256 == "" {
		t.Fatal("empty committed object")
	}
}

func TestMatrix_C07_ClientDisconnectLargeObject(t *testing.T) {
	e := env.New(t, env.Options{
		Repo:                  genericRepo(immutableRule(`^/file$`), "deny"),
		BackgroundCompleteMax: config.Size(100),
	})
	e.Upstream.Set("/file", testutil.Route{Body: []byte(strings.Repeat("l", 4096))})
	writer := &failingResponseWriter{header: make(http.Header), failAfter: 10}
	req := httptest.NewRequest(http.MethodGet, "http://larder.test/file", nil)
	e.Engine.Handle(context.Background(), &e.Repo, writer, req)

	if _, err := e.Index.CurrentObject(context.Background(), "generic", "/file"); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("large object was committed: %v", err)
	}
}

func TestMatrix_C08_TruncatedBody(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/file$`), "deny"), UpstreamCount: 2})
	e.Fakes[0].Set("/file", testutil.Route{Body: []byte("partial"), Truncate: true})
	e.Fakes[1].Set("/file", testutil.Route{Body: []byte("complete")})
	rec := e.GET("/file")
	mustCode(t, rec, 200)
	if rec.Body.String() != "complete" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if count := e.Fakes[0].Count("/file"); count != 1 {
		t.Fatalf("first upstream count = %d", count)
	}
}

func TestMatrix_C09_ServerErrorFailover(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/file$`), "deny"), UpstreamCount: 2})
	e.Fakes[0].Set("/file", testutil.Route{Status: http.StatusBadGateway})
	e.Fakes[1].Set("/file", testutil.Route{Body: []byte("second")})
	rec := e.GET("/file")
	mustCode(t, rec, 200)
	if rec.Body.String() != "second" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestMatrix_C10_NotFoundThenSuccess(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/file$`), "deny"), UpstreamCount: 2})
	e.Fakes[0].Set("/file", testutil.Route{Status: http.StatusNotFound})
	e.Fakes[1].Set("/file", testutil.Route{Body: []byte("found")})
	rec := e.GET("/file")
	mustCode(t, rec, 200)
	if rec.Body.String() != "found" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestMatrix_C11_NegativeCache(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/file$`), "deny"), UpstreamCount: 2})
	e.Fakes[0].Set("/file", testutil.Route{Status: http.StatusNotFound})
	e.Fakes[1].Set("/file", testutil.Route{Status: http.StatusNotFound})
	mustCode(t, e.GET("/file"), 404)
	first := e.Fakes[0].Count("/file") + e.Fakes[1].Count("/file")
	if first != 2 {
		t.Fatalf("first request upstream count = %d", first)
	}
	e.Fakes[0].ResetCounters()
	e.Fakes[1].ResetCounters()
	mustCode(t, e.GET("/file"), 404)
	if e.Fakes[0].Count("/file")+e.Fakes[1].Count("/file") != 0 {
		t.Fatal("negative cache did not suppress upstream contact")
	}
}

func TestMatrix_C12_Breaker(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/file$`), "deny")})
	e.Upstream.Set("/file", testutil.Route{Status: http.StatusInternalServerError})
	for i := 0; i < 3; i++ {
		if rec := e.GET("/file"); rec.Code != http.StatusBadGateway {
			t.Fatalf("failure %d status = %d", i, rec.Code)
		}
	}
	before := e.Upstream.Count("/file")
	if rec := e.GET("/file"); rec.Code != http.StatusBadGateway {
		t.Fatalf("open breaker status = %d", rec.Code)
	}
	if e.Upstream.Count("/file") != before {
		t.Fatal("open breaker contacted upstream")
	}
	e.Upstream.Set("/file", testutil.Route{Body: []byte("recovered")})
	e.Clock.Advance(30 * time.Second)
	rec := e.GET("/file")
	mustCode(t, rec, 200)
	if rec.Body.String() != "recovered" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestMatrix_C28_Head(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/file$`), "deny")})
	e.Upstream.Set("/file", testutil.Route{Body: []byte("artifact")})
	get := e.GET("/file")
	mustCode(t, get, 200)

	head := e.Serve(http.MethodHead, "/file", nil)
	mustCode(t, head, 200)
	if head.Body.Len() != 0 {
		t.Fatalf("HEAD body = %q", head.Body.String())
	}
	mustHeader(t, head, "Content-Length", get.Header().Get("Content-Length"))
	mustHeader(t, head, "X-Larder-Cache", "HIT")

	miss := e.Serve(http.MethodHead, "/missing.bin", nil)
	mustCode(t, miss, 404)
	if miss.Body.Len() != 0 {
		t.Fatalf("HEAD miss body = %q", miss.Body.String())
	}
}

func TestMatrix_C31_ResponseHeaders(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo([]config.GenericRule{
		{Match: `^/immutable`, Class: "immutable"},
		{Match: `^/mutable`, Class: "mutable"},
	}, "deny")})
	e.Upstream.Set("/immutable", testutil.Route{Body: []byte("i")})
	e.Upstream.Set("/mutable", testutil.Route{Body: []byte("m")})

	immutable := e.GET("/immutable")
	mustHeader(t, immutable, "X-Larder-Cache", "MISS")
	mustHeader(t, immutable, "X-Larder-Key", "generic:/immutable")
	mustHeader(t, immutable, "Cache-Control", "public, max-age=31536000, immutable")
	mutable := e.GET("/mutable")
	mustHeader(t, mutable, "X-Larder-Cache", "MISS")
	mustHeader(t, mutable, "Cache-Control", "public, max-age=0, must-revalidate")
	hit := e.GET("/mutable")
	mustHeader(t, hit, "X-Larder-Cache", "HIT")
}

func TestMatrix_X01_DefaultDeny(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo(nil, "deny")})
	rec := e.GET("/anything")
	mustCode(t, rec, 404)
	if e.Upstream.Total() != 0 {
		t.Fatal("deny path contacted upstream")
	}
}

func TestMatrix_X02_RulesFirstMatch(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo([]config.GenericRule{
		{Match: `^/x`, Class: "immutable"},
		{Match: `^/x`, Class: "mutable"},
	}, "deny")})
	e.Upstream.Set("/x", testutil.Route{Body: []byte("x")})
	rec := e.GET("/x")
	mustHeader(t, rec, "Cache-Control", "public, max-age=31536000, immutable")
}

func TestMatrix_X06_KeepQuery(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo([]config.GenericRule{
		{Match: `^/x`, Class: "immutable"},
	}, "deny")})
	e.Upstream.Set("/x", testutil.Route{Body: []byte("x")})
	rec := e.GET("/x?a=1")
	mustHeader(t, rec, "X-Larder-Key", "generic:/x")

	keep := genericRepo([]config.GenericRule{{Match: `^/x`, Class: "immutable"}}, "deny")
	keep.Generic.KeepQuery = true
	e2 := env.New(t, env.Options{Repo: keep})
	e2.Upstream.Set("/x", testutil.Route{Body: []byte("x")})
	rec2 := e2.GET("/x?a=1")
	mustHeader(t, rec2, "X-Larder-Key", "generic:/x?a=1")
}

func sha256String(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func readAll(t *testing.T, reader io.ReadCloser) []byte {
	t.Helper()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	return data
}

var _ cache.Bypass
var _ kinds.Kind
