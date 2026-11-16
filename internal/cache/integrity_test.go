package cache_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mmrzaf/larder/testutil"
	"github.com/mmrzaf/larder/testutil/env"
)

func blobPath(e *env.Environment, sha string) string {
	return filepath.Join(e.Store.DataDir(), "blobs", sha[:2], sha[2:4], sha)
}

func TestMatrix_F06_TruncatedBlobIsDiscardedAndRefetched(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/file$`), "deny")})
	body := bytes.Repeat([]byte("checked bytes "), 128)
	e.Upstream.Set("/file", testutil.Route{Body: body})
	mustCode(t, e.GET("/file"), 200)
	object, err := e.Index.CurrentObject(context.Background(), "generic", "/file")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(blobPath(e, object.SHA256), int64(len(body)-7)); err != nil {
		t.Fatal(err)
	}

	healed := e.GET("/file")
	mustCode(t, healed, 200)
	if !bytes.Equal(healed.Body.Bytes(), body) {
		t.Fatalf("healed response differs from the upstream body (%d bytes)", healed.Body.Len())
	}
	mustHeader(t, healed, "X-Larder-Cache", "MISS")
	if got := e.Upstream.Count("/file"); got != 2 {
		t.Fatalf("upstream requests = %d, want 2", got)
	}
	again := e.GET("/file")
	mustHeader(t, again, "X-Larder-Cache", "HIT")
	if !bytes.Equal(again.Body.Bytes(), body) {
		t.Fatal("repaired entry does not serve the full body")
	}
}

func TestMatrix_F07_MissingBlobFileIsRefetched(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/file$`), "deny")})
	body := bytes.Repeat([]byte("payload "), 64)
	e.Upstream.Set("/file", testutil.Route{Body: body})
	mustCode(t, e.GET("/file"), 200)
	object, err := e.Index.CurrentObject(context.Background(), "generic", "/file")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(blobPath(e, object.SHA256)); err != nil {
		t.Fatal(err)
	}
	healed := e.GET("/file")
	mustCode(t, healed, 200)
	if !bytes.Equal(healed.Body.Bytes(), body) {
		t.Fatal("refetched body differs")
	}
}

func TestMatrix_F08_BrokenEntryWithUpstreamDownIsAnError(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/file$`), "deny")})
	body := bytes.Repeat([]byte("payload "), 64)
	e.Upstream.Set("/file", testutil.Route{Body: body})
	mustCode(t, e.GET("/file"), 200)
	object, err := e.Index.CurrentObject(context.Background(), "generic", "/file")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(blobPath(e, object.SHA256), 3); err != nil {
		t.Fatal(err)
	}
	e.Engine.SetOffline(true)
	rec := e.GET("/file")
	if rec.Code == 200 {
		t.Fatalf("damaged blob served as success with %d bytes", rec.Body.Len())
	}
}
