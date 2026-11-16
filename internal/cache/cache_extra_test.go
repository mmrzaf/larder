package cache_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/testutil"
	"github.com/mmrzaf/larder/testutil/env"
)

func TestLocalRedirectDenyAndPassthrough(t *testing.T) {
	e := env.New(t, env.Options{Repo: testRepo()})
	local := e.GET("/local")
	mustCode(t, local, 200)
	if local.Body.String() != "local" {
		t.Fatalf("local body = %q", local.Body.String())
	}
	mustHeader(t, local, "X-Larder-Cache", "BYPASS")

	redirect := e.GET("/redirect")
	mustCode(t, redirect, 301)
	mustHeader(t, redirect, "Location", "/local")

	deny := e.GET("/deny")
	mustCode(t, deny, 404)

	e.Upstream.Set("/passthrough", testutil.Route{Body: []byte("streamed")})
	pass := e.GET("/passthrough")
	mustCode(t, pass, 200)
	if pass.Body.String() != "streamed" {
		t.Fatalf("passthrough body = %q", pass.Body.String())
	}
	mustHeader(t, pass, "X-Larder-Cache", "BYPASS")
	mustHeader(t, pass, "Cache-Control", "no-store")

	e.Engine.SetOffline(true)
	mustCode(t, e.GET("/passthrough"), 504)
	e.Engine.SetOffline(false)
	if e.Engine.Offline() {
		t.Fatal("Offline() did not reflect the switch")
	}
	if e.Metrics.Offline.With().Value() != 0 {
		t.Fatal("offline metric was not cleared")
	}
}

func TestHeadMissDoesNotStore(t *testing.T) {
	e := env.New(t, env.Options{Repo: testRepo()})
	e.Upstream.Set("/plain/foo", testutil.Route{Body: []byte("artifact")})
	rec := e.Serve(http.MethodHead, "/plain/foo", nil)
	mustCode(t, rec, 200)
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD body = %q", rec.Body.String())
	}
	mustHeader(t, rec, "Content-Length", "8")
	if _, err := e.Index.CurrentObject(context.Background(), "test", "/plain/foo"); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("HEAD stored an object: %v", err)
	}
	if count := e.Upstream.Count("/plain/foo"); count != 1 {
		t.Fatalf("HEAD miss count = %d", count)
	}
}

func TestRewriteAndDerivedETag(t *testing.T) {
	e := env.New(t, env.Options{Repo: testRepo()})
	e.Upstream.Set("/rewrite", testutil.Route{Body: []byte("raw")})
	rec := e.GET("/rewrite")
	mustCode(t, rec, 200)
	if rec.Body.String() != "http://larder.test|raw" {
		t.Fatalf("rewritten body = %q", rec.Body.String())
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("rewritten response has no ETag")
	}
	notModified := e.Serve(http.MethodGet, "/rewrite", env.Header("If-None-Match", etag))
	mustCode(t, notModified, 304)
}

func TestResolveHook(t *testing.T) {
	e := env.New(t, env.Options{Repo: testRepo()})
	e.Upstream.Set("/resolved", testutil.Route{Body: []byte("resolved-body")})
	rec := e.GET("/resolve")
	mustCode(t, rec, 200)
	if rec.Body.String() != "resolved-body" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestIngestMalformedAndMarkStale(t *testing.T) {
	var logs bytes.Buffer
	e := env.New(t, env.Options{Repo: testRepo(), Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	e.Upstream.Set("/mutable/foo", testutil.Route{Body: []byte("v1")})
	mustCode(t, e.GET("/mutable/foo"), 200)

	e.Upstream.Set("/ingest", testutil.Route{Body: []byte("not json")})
	mustCode(t, e.GET("/ingest"), 200)
	e.Clock.Advance(61 * time.Second)

	e.Upstream.Set("/ingest", testutil.Route{Body: []byte(`{"stale":["/mutable/*"]}`)})
	rec := e.GET("/ingest")
	t.Logf("ingest status=%d cache=%s body=%q", rec.Code, rec.Header().Get("X-Larder-Cache"), rec.Body.String())
	mustCode(t, rec, 200)
	object, err := e.Index.CurrentObject(context.Background(), "test", "/mutable/foo")
	if err != nil {
		t.Fatal(err)
	}
	if !object.ValidatedAt.IsZero() {
		t.Logf("logs:\n%s", logs.String())
		t.Fatalf("object was not marked stale: %v", object.ValidatedAt)
	}
}

func TestRangeVariants(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/file$`), "deny")})
	e.Upstream.Set("/file", testutil.Route{Body: []byte("0123456789")})
	mustCode(t, e.GET("/file"), 200)

	suffix := e.Serve(http.MethodGet, "/file", env.Header("Range", "bytes=-3"))
	mustCode(t, suffix, 206)
	if suffix.Body.String() != "789" {
		t.Fatalf("suffix body = %q", suffix.Body.String())
	}
	open := e.Serve(http.MethodGet, "/file", env.Header("Range", "bytes=5-"))
	mustCode(t, open, 206)
	if open.Body.String() != "56789" {
		t.Fatalf("open body = %q", open.Body.String())
	}
	unsat := e.Serve(http.MethodGet, "/file", env.Header("Range", "bytes=99-"))
	mustCode(t, unsat, 416)
	mustHeader(t, unsat, "Content-Range", "bytes */10")
	multi := e.Serve(http.MethodGet, "/file", env.Header("Range", "bytes=0-1,3-4"))
	mustCode(t, multi, 200)
	if multi.Body.String() != "0123456789" {
		t.Fatalf("multi range body = %q", multi.Body.String())
	}
	head := e.Serve(http.MethodHead, "/file", env.Header("Range", "bytes=2-4"))
	mustCode(t, head, 206)
	if head.Body.Len() != 0 {
		t.Fatalf("HEAD range body = %q", head.Body.String())
	}
}

func TestGzipServing(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/file$`), "deny")})
	body := `{"name":"larder","value":"` + strings.Repeat("x", 500) + `"}`
	e.Upstream.Set("/file", testutil.Route{Body: []byte(body), Header: http.Header{"Content-Type": []string{"application/json"}}})
	mustCode(t, e.GET("/file"), 200)

	identity := e.GET("/file")
	mustCode(t, identity, 200)
	if identity.Body.String() != body {
		t.Fatal("identity body differs")
	}
	if identity.Header().Get("Content-Encoding") != "" {
		t.Fatal("identity response has Content-Encoding")
	}

	compressed := e.Serve(http.MethodGet, "/file", env.Header("Accept-Encoding", "gzip"))
	mustCode(t, compressed, 200)
	mustHeader(t, compressed, "Content-Encoding", "gzip")
	gz, err := gzip.NewReader(bytes.NewReader(compressed.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if string(decoded) != body {
		t.Fatal("gzip body differs")
	}

	head := e.Serve(http.MethodHead, "/file", env.Header("Accept-Encoding", "gzip"))
	mustCode(t, head, 200)
	if head.Body.Len() != 0 {
		t.Fatal("gzip HEAD body is not empty")
	}
}

func TestIfModifiedSince(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/file$`), "deny")})
	modified := time.Unix(1_700_000_000, 0).UTC()
	e.Upstream.Set("/file", testutil.Route{Body: []byte("artifact"), LastModified: modified})
	mustCode(t, e.GET("/file"), 200)
	rec := e.Serve(http.MethodGet, "/file", env.Header("If-Modified-Since", modified.Format(http.TimeFormat)))
	mustCode(t, rec, 304)
}

func TestMutableMaxObjectSizeBypass(t *testing.T) {
	repo := testRepo()
	repo.MaxObjectSize = 10
	e := env.New(t, env.Options{Repo: repo})
	e.Upstream.Set("/mutable/foo", testutil.Route{Body: []byte(strings.Repeat("x", 20))})
	rec := e.GET("/mutable/foo")
	mustCode(t, rec, 200)
	mustHeader(t, rec, "X-Larder-Cache", "BYPASS")
	if _, err := e.Index.CurrentObject(context.Background(), "test", "/mutable/foo"); !errors.Is(err, index.ErrNotFound) {
		t.Fatal("mutable oversized object was stored")
	}
}

func TestCompanionMismatch(t *testing.T) {
	repo := genericRepo(immutableRule(`^/file$`), "deny")
	repo.Generic.Companion = []config.GenericCompanion{{Suffix: ".sha256", Algo: "sha256"}}
	e := env.New(t, env.Options{Repo: repo})
	e.Upstream.Set("/file", testutil.Route{Body: []byte("artifact")})
	e.Upstream.Set("/file.sha256", testutil.Route{Body: []byte(sha256String("other"))})
	mustCode(t, e.GET("/file"), 502)
	if e.Metrics.VerifyFailures.With("generic").Value() == 0 {
		t.Fatal("companion mismatch did not increment verify failures")
	}
}

func TestHistoryPruning(t *testing.T) {
	e := env.New(t, env.Options{Repo: testRepo(), MetadataHistory: 1})
	e.Upstream.Set("/mutable/foo", testutil.Route{Body: []byte("v1")})
	mustCode(t, e.GET("/mutable/foo"), 200)
	for _, body := range []string{"v2", "v3"} {
		e.Clock.Advance(61 * time.Second)
		e.Upstream.Set("/mutable/foo", testutil.Route{Body: []byte(body)})
		mustCode(t, e.GET("/mutable/foo"), 200)
	}
	generations, err := e.Index.Generations(context.Background(), "test", "/mutable/foo")
	if err != nil {
		t.Fatal(err)
	}
	if len(generations) != 2 {
		t.Fatalf("generations = %d, want current plus one history", len(generations))
	}
}

func TestPublicBaseURL(t *testing.T) {
	repo := testRepo()
	repo.PublicURL = "https://repo.example"
	e := env.New(t, env.Options{Repo: repo})
	e.Upstream.Set("/rewrite", testutil.Route{Body: []byte("raw")})
	rec := e.GET("/rewrite")
	mustCode(t, rec, 200)
	if rec.Body.String() != "https://repo.example|raw" {
		t.Fatalf("body = %q", rec.Body.String())
	}

	e2 := env.New(t, env.Options{Repo: testRepo(), PublicBaseURL: "https://global.example"})
	e2.Upstream.Set("/rewrite", testutil.Route{Body: []byte("raw")})
	rec2 := e2.GET("/rewrite")
	if rec2.Body.String() != "https://global.example|raw" {
		t.Fatalf("global base body = %q", rec2.Body.String())
	}
}

func TestMethodAndUnknownRepo(t *testing.T) {
	e := env.New(t, env.Options{Repo: testRepo()})
	mustCode(t, e.Serve(http.MethodPost, "/local", nil), 405)
	rec := httptest.NewRecorder()
	e.Engine.Handle(context.Background(), &config.Repo{Name: "missing"}, rec, httptest.NewRequest(http.MethodGet, "http://larder.test/x", nil))
	if rec.Code != 404 {
		t.Fatalf("unknown repo status = %d", rec.Code)
	}
}

func TestValidateHook(t *testing.T) {
	e := env.New(t, env.Options{Repo: testRepo()})
	e.Upstream.Set("/validate", testutil.Route{Body: []byte("valid artifact")})
	mustCode(t, e.GET("/validate"), 200)
	e2 := env.New(t, env.Options{Repo: testRepo()})
	e2.Upstream.Set("/validate", testutil.Route{Body: []byte("bad artifact")})
	mustCode(t, e2.GET("/validate"), 502)
	if _, err := e2.Index.CurrentObject(context.Background(), "test", "/validate"); !errors.Is(err, index.ErrNotFound) {
		t.Fatal("invalid artifact was stored")
	}
}

func TestMaxStaleLimit(t *testing.T) {
	e := env.New(t, env.Options{Repo: testRepo()})
	e.Upstream.Set("/mutable/foo", testutil.Route{Body: []byte("v1")})
	mustCode(t, e.GET("/mutable/foo"), 200)
	e.Config.Cache.MaxStale = config.Duration(30 * time.Second)
	e.Clock.Advance(2 * time.Minute)
	e.Upstream.Set("/mutable/foo", testutil.Route{Status: http.StatusInternalServerError})
	mustCode(t, e.GET("/mutable/foo"), 502)
}

func TestGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/file$`), "deny")})
	e.Upstream.Set("/file", testutil.Route{Body: []byte("artifact")})
	mustCode(t, e.GET("/file"), 200)
	e.Engine.Close()
	for _, fake := range e.Fakes {
		fake.Server.Close()
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("goroutines leaked: before %d, after %d", before, runtime.NumGoroutine())
}

type discardResponseWriter struct{ header http.Header }

func (w *discardResponseWriter) Header() http.Header { return w.header }
func (w *discardResponseWriter) WriteHeader(int)     {}
func (w *discardResponseWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

func TestStreamingMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("streaming memory test")
	}
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/big$`), "deny")})
	body := bytes.Repeat([]byte("a"), 200<<20)
	e.Upstream.Set("/big", testutil.Route{Body: body})
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	writer := &discardResponseWriter{header: make(http.Header)}
	req := httptest.NewRequest(http.MethodGet, "http://larder.test/big", nil)
	e.Engine.Handle(context.Background(), &e.Repo, writer, req)
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if after.HeapAlloc > before.HeapAlloc+64<<20 {
		t.Fatalf("heap grew from %d to %d", before.HeapAlloc, after.HeapAlloc)
	}
	object, err := e.Index.CurrentObject(context.Background(), "generic", "/big")
	if err != nil || object.SHA256 == "" {
		t.Fatalf("large object not committed: %v", err)
	}
}

func TestBypassAndPassthroughFailures(t *testing.T) {
	bypass := &toggleBypass{}
	bypass.on.Store(true)
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/file$`), "deny"), Bypass: bypass})
	e.Upstream.Set("/file", testutil.Route{Status: http.StatusInternalServerError})
	mustCode(t, e.GET("/file"), 502)

	p := env.New(t, env.Options{Repo: testRepo()})
	p.Upstream.Set("/passthrough", testutil.Route{Status: http.StatusInternalServerError})
	mustCode(t, p.GET("/passthrough"), 502)
}

func TestHeadMissFailure(t *testing.T) {
	e := env.New(t, env.Options{Repo: testRepo()})
	e.Upstream.Set("/plain/foo", testutil.Route{Status: http.StatusInternalServerError})
	mustCode(t, e.Serve(http.MethodHead, "/plain/foo", nil), 502)
}

func TestCompanionMalformed(t *testing.T) {
	repo := genericRepo(immutableRule(`^/file$`), "deny")
	repo.Generic.Companion = []config.GenericCompanion{{Suffix: ".sha256", Algo: "sha256"}}
	e := env.New(t, env.Options{Repo: repo})
	e.Upstream.Set("/file", testutil.Route{Body: []byte("artifact")})
	e.Upstream.Set("/file.sha256", testutil.Route{Body: []byte("not-a-digest")})
	rec := e.GET("/file")
	mustCode(t, rec, 200)
	mustHeader(t, rec, "X-Larder-Verified", "0")
}

func TestRetryUnverifiedSkipsNonGo(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/file$`), "deny")})
	e.Upstream.Set("/file", testutil.Route{Body: []byte("artifact")})
	mustCode(t, e.GET("/file"), 200)
	if err := e.Engine.RetryUnverified(context.Background()); err != nil {
		t.Fatal(err)
	}
	object, err := e.Index.CurrentObject(context.Background(), "generic", "/file")
	if err != nil {
		t.Fatal(err)
	}
	if object.Verified {
		t.Fatal("generic object was verified by the Go retry")
	}
	if len(e.Engine.UpstreamStates()) == 0 {
		t.Fatal("upstream states are empty after a fetch")
	}
}
