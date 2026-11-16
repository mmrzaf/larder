package cache_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/testutil"
	"github.com/mmrzaf/larder/testutil/env"
)

func alpineRepo() config.Repo {
	return config.Repo{Name: "alpine", Kind: "alpine", Mount: "/alpine", ServeGone: true, FreshTTL: config.Duration(300 * time.Second)}
}

func TestMatrix_K02_CacheLifecycle(t *testing.T) {
	e := env.New(t, env.Options{Repo: alpineRepo()})
	const index = "/v3.20/main/x86_64/APKINDEX.tar.gz"
	const pkg = "/v3.20/main/x86_64/busybox-1.36.1-r29.apk"
	indexBody := bytes.Repeat([]byte("index "), 512)
	pkgBody := bytes.Repeat([]byte("apk "), 4096)
	e.Upstream.Set(index, testutil.Route{Body: indexBody, ETag: `"i1"`, Conditional: true})
	e.Upstream.Set(pkg, testutil.Route{Body: pkgBody})

	first := e.GET(pkg)
	mustCode(t, first, 200)
	mustHeader(t, first, "X-Larder-Cache", "MISS")
	if !bytes.Equal(first.Body.Bytes(), pkgBody) {
		t.Fatal("package differs from the upstream")
	}
	mustCode(t, e.GET(index), 200)

	// With the upstream gone, cached packages are hits and the index is served stale.
	e.Upstream.Set(pkg, testutil.Route{Status: 500})
	e.Upstream.Set(index, testutil.Route{Status: 500})
	e.Clock.Advance(time.Hour)
	hit := e.GET(pkg)
	mustCode(t, hit, 200)
	mustHeader(t, hit, "X-Larder-Cache", "HIT")
	stale := e.GET(index)
	mustCode(t, stale, 200)
	mustHeader(t, stale, "X-Larder-Stale", "1")
	if !bytes.Equal(stale.Body.Bytes(), indexBody) {
		t.Fatal("stale index differs from the last good copy")
	}
	mustCode(t, e.GET("/v3.20/main/x86_64/missing-1.apk"), 404)
	mustCode(t, e.GET("/v3.20/releases/x86_64/alpine.iso"), 404)
}

func TestMatrix_K03_HTMLErrorPageIsNotCached(t *testing.T) {
	e := env.New(t, env.Options{Repo: alpineRepo()})
	e.Upstream.Set("/v3.20/main/x86_64/a-1.apk", testutil.Route{HTML: true})
	mustCode(t, e.GET("/v3.20/main/x86_64/a-1.apk"), 502)
	e.Upstream.Set("/v3.20/main/x86_64/APKINDEX.tar.gz", testutil.Route{HTML: true})
	mustCode(t, e.GET("/v3.20/main/x86_64/APKINDEX.tar.gz"), 502)
}
