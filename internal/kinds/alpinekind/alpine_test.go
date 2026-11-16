package alpinekind

import (
	"testing"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/kinds"
	"github.com/mmrzaf/larder/internal/kinds/kindtest"
)

func plan(path string, class kinds.Class, binary bool, fresh time.Duration) kinds.Plan {
	return kinds.Plan{Class: class, Key: path, UpstreamPath: path, Binary: binary, FreshTTL: fresh}
}

func TestMatrix_K01_Classification(t *testing.T) {
	kind, err := New(config.Repo{Name: "alpine", Kind: "alpine", Mount: "/alpine"})
	if err != nil {
		t.Fatal(err)
	}
	const index = "/v3.20/main/x86_64/APKINDEX.tar.gz"
	const pkg = "/v3.20/community/aarch64/busybox-1.36.1-r29.apk"
	kindtest.Table(t, kind, []kindtest.Case{
		{Name: "index", Path: index, Want: plan(index, kinds.Mutable, true, 300*time.Second)},
		{Name: "edge index", Path: "/edge/testing/x86_64/APKINDEX.tar.gz", Want: plan("/edge/testing/x86_64/APKINDEX.tar.gz", kinds.Mutable, true, 300*time.Second)},
		{Name: "latest stable", Path: "/latest-stable/main/x86_64/APKINDEX.tar.gz", Want: plan("/latest-stable/main/x86_64/APKINDEX.tar.gz", kinds.Mutable, true, 300*time.Second)},
		{Name: "package", Path: pkg, Want: plan(pkg, kinds.Immutable, true, 0)},
		{Name: "plus and tilde", Path: "/v3.20/main/x86_64/g++-13.2.1+git~1-r0.apk", Want: plan("/v3.20/main/x86_64/g++-13.2.1+git~1-r0.apk", kinds.Immutable, true, 0)},
		{Name: "release image", Path: "/v3.20/releases/x86_64/alpine-minirootfs-3.20.0-x86_64.tar.gz", Want: plan("/v3.20/releases/x86_64/alpine-minirootfs-3.20.0-x86_64.tar.gz", kinds.Deny, false, 0)},
		{Name: "bad branch", Path: "/stable/main/x86_64/APKINDEX.tar.gz", Want: plan("/stable/main/x86_64/APKINDEX.tar.gz", kinds.Deny, false, 0)},
		{Name: "branch with trailing dot", Path: "/v3./main/x86_64/APKINDEX.tar.gz", Want: plan("/v3./main/x86_64/APKINDEX.tar.gz", kinds.Deny, false, 0)},
		{Name: "traversal", Path: "/v3.20/../x86_64/a.apk", Want: plan("/v3.20/../x86_64/a.apk", kinds.Deny, false, 0)},
		{Name: "too shallow", Path: "/v3.20/main/APKINDEX.tar.gz", Want: plan("/v3.20/main/APKINDEX.tar.gz", kinds.Deny, false, 0)},
		{Name: "too deep", Path: "/v3.20/main/x86_64/sub/a.apk", Want: plan("/v3.20/main/x86_64/sub/a.apk", kinds.Deny, false, 0)},
		{Name: "other file", Path: "/v3.20/main/x86_64/README", Want: plan("/v3.20/main/x86_64/README", kinds.Deny, false, 0)},
		{Name: "hidden apk", Path: "/v3.20/main/x86_64/.a.apk", Want: plan("/v3.20/main/x86_64/.a.apk", kinds.Deny, false, 0)},
		{Name: "root", Path: "/", Want: plan("/", kinds.Deny, false, 0)},
	})
}

func FuzzClassify(f *testing.F) {
	for _, seed := range []string{"/v3.20/main/x86_64/APKINDEX.tar.gz", "/edge/main/x86_64/a.apk", "", "/", "//", "/v/./../"} {
		f.Add(seed)
	}
	kind := &Kind{}
	f.Fuzz(func(t *testing.T, path string) {
		plan, err := kind.Classify(kinds.Request{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Class != kinds.Deny && (plan.UpstreamPath != path || plan.Key != path) {
			t.Fatalf("served path changed: %q -> %q", path, plan.UpstreamPath)
		}
	})
}
