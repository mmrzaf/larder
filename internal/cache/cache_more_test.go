package cache_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/store"
	"github.com/mmrzaf/larder/testutil"
	"github.com/mmrzaf/larder/testutil/env"
)

func TestMatrix_C13_MutableFresh(t *testing.T) {
	e := env.New(t, env.Options{Repo: testRepo()})
	e.Upstream.Set("/mutable/foo", testutil.Route{Body: []byte("v1")})
	mustCode(t, e.GET("/mutable/foo"), 200)
	rec := e.GET("/mutable/foo")
	mustCode(t, rec, 200)
	mustHeader(t, rec, "X-Larder-Cache", "HIT")
	if count := e.Upstream.Count("/mutable/foo"); count != 1 {
		t.Fatalf("upstream count = %d", count)
	}
}

func TestMatrix_C14_MutableRevalidated304(t *testing.T) {
	e := env.New(t, env.Options{Repo: testRepo()})
	e.Upstream.Set("/mutable/foo", testutil.Route{Body: []byte("v1"), ETag: `"e1"`, Conditional: true})
	first := e.GET("/mutable/foo")
	mustCode(t, first, 200)
	object, err := e.Index.CurrentObject(context.Background(), "test", "/mutable/foo")
	if err != nil {
		t.Fatal(err)
	}
	before := object.ValidatedAt
	e.Clock.Advance(61 * time.Second)

	rec := e.GET("/mutable/foo")
	mustCode(t, rec, 200)
	mustHeader(t, rec, "X-Larder-Cache", "REVALIDATED")
	if rec.Body.String() != "v1" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	after, err := e.Index.CurrentObject(context.Background(), "test", "/mutable/foo")
	if err != nil {
		t.Fatal(err)
	}
	if !after.ValidatedAt.After(before) {
		t.Fatal("validated_at was not updated")
	}
	generations, err := e.Index.Generations(context.Background(), "test", "/mutable/foo")
	if err != nil {
		t.Fatal(err)
	}
	if len(generations) != 1 {
		t.Fatalf("generations = %d, want 1", len(generations))
	}
}

func TestMatrix_C15_MutableNewGeneration(t *testing.T) {
	e := env.New(t, env.Options{Repo: testRepo()})
	e.Upstream.Set("/mutable/foo", testutil.Route{Body: []byte("v1"), ETag: `"e1"`, Conditional: true})
	mustCode(t, e.GET("/mutable/foo"), 200)
	e.Clock.Advance(61 * time.Second)
	e.Upstream.Set("/mutable/foo", testutil.Route{Body: []byte("v2"), ETag: `"e2"`, Conditional: true})

	rec := e.GET("/mutable/foo")
	mustCode(t, rec, 200)
	mustHeader(t, rec, "X-Larder-Cache", "REVALIDATED")
	if rec.Body.String() != "v2" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	generations, err := e.Index.Generations(context.Background(), "test", "/mutable/foo")
	if err != nil {
		t.Fatal(err)
	}
	if len(generations) != 2 || !generations[0].Current || generations[1].Current {
		t.Fatalf("generations = %#v", generations)
	}
}

func TestMatrix_C16_MutableStaleOnFailure(t *testing.T) {
	e := env.New(t, env.Options{Repo: testRepo()})
	e.Upstream.Set("/mutable/foo", testutil.Route{Body: []byte("v1"), ETag: `"e1"`, Conditional: true})
	mustCode(t, e.GET("/mutable/foo"), 200)
	e.Clock.Advance(61 * time.Second)
	e.Upstream.Set("/mutable/foo", testutil.Route{Status: http.StatusInternalServerError})

	rec := e.GET("/mutable/foo")
	mustCode(t, rec, 200)
	mustHeader(t, rec, "X-Larder-Cache", "STALE")
	mustHeader(t, rec, "X-Larder-Stale", "1")
	if rec.Body.String() != "v1" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestMatrix_C17_GoneObject(t *testing.T) {
	e := env.New(t, env.Options{Repo: testRepo()})
	e.Upstream.Set("/mutable/foo", testutil.Route{Body: []byte("v1"), ETag: `"e1"`, Conditional: true})
	mustCode(t, e.GET("/mutable/foo"), 200)
	e.Clock.Advance(61 * time.Second)
	e.Upstream.Set("/mutable/foo", testutil.Route{Status: http.StatusNotFound})
	rec := e.GET("/mutable/foo")
	mustCode(t, rec, 200)
	mustHeader(t, rec, "X-Larder-Cache", "GONE")
	mustHeader(t, rec, "X-Larder-Upstream-Status", "404")
	if rec.Body.String() != "v1" {
		t.Fatalf("body = %q", rec.Body.String())
	}

	e410 := env.New(t, env.Options{Repo: testRepo()})
	e410.Upstream.Set("/mutable/foo", testutil.Route{Body: []byte("v1"), ETag: `"e1"`, Conditional: true})
	mustCode(t, e410.GET("/mutable/foo"), 200)
	e410.Clock.Advance(61 * time.Second)
	e410.Upstream.Set("/mutable/foo", testutil.Route{Status: http.StatusGone})
	gone := e410.GET("/mutable/foo")
	mustHeader(t, gone, "X-Larder-Upstream-Status", "410")

	repo := testRepo()
	repo.ServeGone = false
	e2 := env.New(t, env.Options{Repo: repo})
	e2.Upstream.Set("/mutable/foo", testutil.Route{Body: []byte("v1"), ETag: `"e1"`, Conditional: true})
	mustCode(t, e2.GET("/mutable/foo"), 200)
	e2.Clock.Advance(61 * time.Second)
	e2.Upstream.Set("/mutable/foo", testutil.Route{Status: http.StatusNotFound})
	mustCode(t, e2.GET("/mutable/foo"), 404)
}

func TestMatrix_C18_OfflineMiss(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/file$`), "deny"), Offline: true})
	rec := e.GET("/file")
	mustCode(t, rec, 504)
	mustHeader(t, rec, "X-Larder-Cache", "OFFLINE-MISS")
	if e.Upstream.Total() != 0 {
		t.Fatal("offline miss contacted upstream")
	}
}

func TestMatrix_C19_OfflineHit(t *testing.T) {
	e := env.New(t, env.Options{Repo: testRepo()})
	e.Upstream.Set("/mutable/foo", testutil.Route{Body: []byte("v1")})
	mustCode(t, e.GET("/mutable/foo"), 200)
	e.Clock.Advance(61 * time.Second)
	e.Engine.SetOffline(true)
	rec := e.GET("/mutable/foo")
	mustCode(t, rec, 200)
	mustHeader(t, rec, "X-Larder-Cache", "OFFLINE-HIT")
	mustHeader(t, rec, "X-Larder-Stale", "1")
	if e.Upstream.Count("/mutable/foo") != 1 {
		t.Fatal("offline hit contacted upstream")
	}
}

func TestMatrix_C20_HTMLForBinary(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/file$`), "deny")})
	e.Upstream.Set("/file", testutil.Route{HTML: true})
	rec := e.GET("/file")
	mustCode(t, rec, 502)
	if _, err := e.Index.CurrentObject(context.Background(), "generic", "/file"); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("html was stored: %v", err)
	}
}

func TestMatrix_C21_MaxObjectSize(t *testing.T) {
	repo := genericRepo(immutableRule(`^/file$`), "deny")
	repo.MaxObjectSize = 10
	e := env.New(t, env.Options{Repo: repo})
	e.Upstream.Set("/file", testutil.Route{Body: []byte("0123456789abcdef")})
	rec := e.GET("/file")
	mustCode(t, rec, 200)
	mustHeader(t, rec, "X-Larder-Cache", "BYPASS")
	if rec.Body.String() != "0123456789abcdef" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if _, err := e.Index.CurrentObject(context.Background(), "generic", "/file"); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("oversized object was stored: %v", err)
	}
}

type toggleBypass struct{ on atomic.Bool }

func (b *toggleBypass) Bypass(string) bool { return b.on.Load() }

func TestMatrix_C22_EmergencyBypass(t *testing.T) {
	bypass := &toggleBypass{}
	bypass.on.Store(true)
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/file$`), "deny"), Bypass: bypass})
	e.Upstream.Set("/file", testutil.Route{Body: []byte("artifact")})
	rec := e.GET("/file")
	mustCode(t, rec, 200)
	mustHeader(t, rec, "X-Larder-Cache", "BYPASS")
	if _, err := e.Index.CurrentObject(context.Background(), "generic", "/file"); !errors.Is(err, index.ErrNotFound) {
		t.Fatal("bypass stored the object")
	}
	bypass.on.Store(false)
	mustHeader(t, e.GET("/file"), "X-Larder-Cache", "MISS")
	bypass.on.Store(true)
	mustHeader(t, e.GET("/file"), "X-Larder-Cache", "HIT")
}

func TestMatrix_C33_ProxyUpstreams(t *testing.T) {
	t.Run("http", func(t *testing.T) {
		proxy := testutil.NewHTTPProxy(t)
		e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/file$`), "deny"), Proxy: config.Proxy(proxy.URL())})
		e.Upstream.Set("/file", testutil.Route{Body: []byte("through-http")})
		rec := e.GET("/file")
		mustCode(t, rec, 200)
		if proxy.Count() == 0 {
			t.Fatal("HTTP proxy was not used")
		}
	})
	t.Run("socks5", func(t *testing.T) {
		proxy := testutil.NewSOCKS5Proxy(t)
		e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/file$`), "deny"), Proxy: config.Proxy("socks5://" + proxy.Addr)})
		e.Upstream.Set("/file", testutil.Route{Body: []byte("through-socks")})
		rec := e.GET("/file")
		mustCode(t, rec, 200)
		if proxy.Count() == 0 {
			t.Fatal("SOCKS proxy was not used")
		}
	})
	t.Run("direct", func(t *testing.T) {
		proxy := testutil.NewHTTPProxy(t)
		e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/file$`), "deny")})
		e.Upstream.Set("/file", testutil.Route{Body: []byte("direct")})
		mustCode(t, e.GET("/file"), 200)
		if proxy.Count() != 0 {
			t.Fatal("direct fetch used a proxy")
		}
	})
}

func TestMatrix_C34_PerUpstreamHeaders(t *testing.T) {
	e := env.New(t, env.Options{
		Repo:    genericRepo(immutableRule(`^/file$`), "deny"),
		Headers: map[string]string{"Authorization": "Bearer token"},
	})
	e.Upstream.Set("/file", testutil.Route{Body: []byte("ok"), RequireHeader: map[string]string{"Authorization": "Bearer token"}})
	rec := e.GET("/file")
	mustCode(t, rec, 200)
	if rec.Body.String() != "ok" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestMatrix_C35_RedirectFollowing(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/old$`), "deny")})
	e.Upstream.Set("/old", testutil.Route{Status: http.StatusFound, Header: http.Header{"Location": []string{"/target"}}})
	e.Upstream.Set("/target", testutil.Route{Body: []byte("redirected")})
	rec := e.GET("/old")
	mustCode(t, rec, 200)
	if rec.Body.String() != "redirected" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	object, err := e.Index.CurrentObject(context.Background(), "generic", "/old")
	if err != nil {
		t.Fatalf("redirect target was not cached under the original key: %v", err)
	}
	if object.SHA256 == "" {
		t.Fatal("empty cached object")
	}
}

func TestMatrix_C35_PrivateRedirectRefused(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/old$`), "deny")})
	parsed, err := url.Parse(e.Upstream.URL())
	if err != nil {
		t.Fatal(err)
	}
	location := "http://localhost:" + parsed.Port() + "/target"
	e.Upstream.Set("/old", testutil.Route{Status: http.StatusFound, Header: http.Header{"Location": []string{location}}})
	e.Upstream.Set("/target", testutil.Route{Body: []byte("private")})
	mustCode(t, e.GET("/old"), 502)
}

func TestMatrix_C35_AuthorizationDroppedAcrossHosts(t *testing.T) {
	e := env.New(t, env.Options{
		Repo:          genericRepo(immutableRule(`^/old$`), "deny"),
		UpstreamCount: 2,
		Headers:       map[string]string{"Authorization": "Bearer token"},
	})
	e.Fakes[0].Set("/old", testutil.Route{Status: http.StatusFound, Header: http.Header{"Location": []string{e.Fakes[1].URL() + "/target"}}})
	e.Fakes[1].Set("/target", testutil.Route{Body: []byte("ok"), ForbiddenHeader: "Authorization"})
	rec := e.GET("/old")
	mustCode(t, rec, 200)
	if rec.Body.String() != "ok" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestMatrix_C39_ExpectationMismatch(t *testing.T) {
	digest := sha256String("good")
	path := "/expect/" + digest
	e := env.New(t, env.Options{Repo: testRepo()})
	e.Upstream.Set(path, testutil.Route{Body: []byte("bad")})
	mustCode(t, e.GET(path), 502)
	if e.Metrics.VerifyFailures.With("test").Value() == 0 {
		t.Fatal("verify failure metric was not incremented")
	}
	if _, err := e.Index.CurrentObject(context.Background(), "test", path); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("mismatched object was stored: %v", err)
	}

	e2 := env.New(t, env.Options{Repo: testRepo(), UpstreamCount: 2})
	e2.Fakes[0].Set(path, testutil.Route{Body: []byte("bad")})
	e2.Fakes[1].Set(path, testutil.Route{Body: []byte("good")})
	rec := e2.GET(path)
	mustCode(t, rec, 200)
	if rec.Body.String() != "good" {
		t.Fatalf("failover body = %q", rec.Body.String())
	}
}

func ingestDoc(expectKey, digest string) []byte {
	doc := map[string]any{
		"expect": []map[string]string{{
			"key": expectKey, "algo": "sha256", "digest": digest,
		}},
	}
	data, _ := json.Marshal(doc)
	return data
}

func TestMatrix_C40_LateExpectation(t *testing.T) {
	e := env.New(t, env.Options{Repo: testRepo()})
	e.Upstream.Set("/plain/foo", testutil.Route{Body: []byte("artifact")})
	first := e.GET("/plain/foo")
	mustCode(t, first, 200)
	mustHeader(t, first, "X-Larder-Verified", "0")
	e.Upstream.Set("/ingest", testutil.Route{Body: ingestDoc("/plain/foo", sha256String("artifact"))})
	mustCode(t, e.GET("/ingest"), 200)
	object, err := e.Index.CurrentObject(context.Background(), "test", "/plain/foo")
	if err != nil {
		t.Fatal(err)
	}
	if !object.Verified {
		t.Fatal("late matching expectation did not verify the object")
	}

	e2 := env.New(t, env.Options{Repo: testRepo()})
	e2.Upstream.Set("/plain/foo", testutil.Route{Body: []byte("artifact")})
	mustCode(t, e2.GET("/plain/foo"), 200)
	stored, err := e2.Index.CurrentObject(context.Background(), "test", "/plain/foo")
	if err != nil {
		t.Fatal(err)
	}
	e2.Upstream.Set("/ingest", testutil.Route{Body: ingestDoc("/plain/foo", sha256String("other"))})
	mustCode(t, e2.GET("/ingest"), 200)
	if _, err := e2.Index.CurrentObject(context.Background(), "test", "/plain/foo"); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("mismatched object still present: %v", err)
	}
	if _, err := e2.Store.Open(stored.SHA256, store.EncodingIdentity); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("mismatched blob was not quarantined: %v", err)
	}
}

func TestMatrix_C41_DigestConflict(t *testing.T) {
	digest := sha256String("artifact")
	path := "/expect/" + digest
	e := env.New(t, env.Options{Repo: testRepo()})
	e.Upstream.Set(path, testutil.Route{Body: []byte("artifact")})
	first := e.GET(path)
	mustCode(t, first, 200)
	mustHeader(t, first, "X-Larder-Verified", "1")
	e.Upstream.Set("/ingest", testutil.Route{Body: ingestDoc(path, sha256String("other"))})
	mustCode(t, e.GET("/ingest"), 200)
	object, err := e.Index.CurrentObject(context.Background(), "test", path)
	if err != nil {
		t.Fatal("verified object was removed on conflict")
	}
	if !object.Verified {
		t.Fatal("verified object lost its state")
	}
	expectation, err := e.Index.GetExpectation(context.Background(), "test", path, "sha256")
	if err != nil {
		t.Fatal(err)
	}
	if expectation.Digest != digest {
		t.Fatalf("expectation = %q, want original %q", expectation.Digest, digest)
	}
	if e.Metrics.VerifyFailures.With("test").Value() == 0 {
		t.Fatal("digest conflict did not increment verify failures")
	}
}

func TestMatrix_C42_RequireVerified(t *testing.T) {
	repo := testRepo()
	repo.RequireVerified = true
	e := env.New(t, env.Options{Repo: repo})
	e.Upstream.Set("/plain/foo", testutil.Route{Body: []byte("artifact")})
	mustCode(t, e.GET("/plain/foo"), 502)
	if _, err := e.Index.CurrentObject(context.Background(), "test", "/plain/foo"); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("unverified object was stored: %v", err)
	}
}

func TestMatrix_C43_StaleWhileRevalidate(t *testing.T) {
	e := env.New(t, env.Options{Repo: testRepo(), StaleWhileRevalidate: config.Duration(60 * time.Second)})
	e.Upstream.Set("/mutable/foo", testutil.Route{Body: []byte("v1")})
	mustCode(t, e.GET("/mutable/foo"), 200)
	e.Clock.Advance(70 * time.Second)
	e.Upstream.Set("/mutable/foo", testutil.Route{Body: []byte("v2")})
	rec := e.GET("/mutable/foo")
	mustCode(t, rec, 200)
	mustHeader(t, rec, "X-Larder-Cache", "STALE")
	if rec.Body.String() != "v1" {
		t.Fatalf("stale body = %q", rec.Body.String())
	}
	e.Engine.Wait()
	object, err := e.Index.CurrentObject(context.Background(), "test", "/mutable/foo")
	if err != nil {
		t.Fatal(err)
	}
	if object.SHA256 != sha256String("v2") {
		t.Fatalf("background refresh did not update object: %s", object.SHA256)
	}
	if e.Upstream.Count("/mutable/foo") != 2 {
		t.Fatalf("upstream count = %d", e.Upstream.Count("/mutable/foo"))
	}
}

func TestMatrix_X03_ConfiguredExpect(t *testing.T) {
	repo := genericRepo(immutableRule(`^/file$`), "deny")
	repo.Generic.Expect = []config.GenericExpect{{Path: "/file", SHA256: sha256String("good")}}
	e := env.New(t, env.Options{Repo: repo})
	e.Upstream.Set("/file", testutil.Route{Body: []byte("good")})
	rec := e.GET("/file")
	mustCode(t, rec, 200)
	mustHeader(t, rec, "X-Larder-Verified", "1")

	e2 := env.New(t, env.Options{Repo: repo})
	e2.Upstream.Set("/file", testutil.Route{Body: []byte("bad")})
	mustCode(t, e2.GET("/file"), 502)
}

func TestMatrix_X04_Companion(t *testing.T) {
	repo := genericRepo(immutableRule(`^/file$`), "deny")
	repo.Generic.Companion = []config.GenericCompanion{{Suffix: ".sha256", Algo: "sha256"}}
	e := env.New(t, env.Options{Repo: repo})
	e.Upstream.Set("/file", testutil.Route{Body: []byte("artifact")})
	e.Upstream.Set("/file.sha256", testutil.Route{Body: []byte(sha256String("artifact") + "  file\n")})
	rec := e.GET("/file")
	mustCode(t, rec, 200)
	mustHeader(t, rec, "X-Larder-Verified", "1")

	e2 := env.New(t, env.Options{Repo: repo})
	e2.Upstream.Set("/file", testutil.Route{Body: []byte("artifact")})
	rec2 := e2.GET("/file")
	mustCode(t, rec2, 200)
	mustHeader(t, rec2, "X-Larder-Verified", "0")
}

func TestMatrix_X05_GenericRedirect(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/old$`), "deny")})
	e.Upstream.Set("/old", testutil.Route{Status: http.StatusMovedPermanently, Header: http.Header{"Location": []string{"/target"}}})
	e.Upstream.Set("/target", testutil.Route{Body: []byte("target")})
	rec := e.GET("/old")
	mustCode(t, rec, 200)
	if rec.Body.String() != "target" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}
