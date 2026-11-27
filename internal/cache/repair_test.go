package cache_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/mmrzaf/larder/internal/integrity"
	"github.com/mmrzaf/larder/testutil"
	"github.com/mmrzaf/larder/testutil/env"
)

// TestMatrix_F17_InconsistentBlobRowIsRefetched starts from the state a failed
// write used to leave behind: a non-empty raw size, a zero stored size and an
// empty file. The request must refetch instead of serving 200 with no body.
func TestMatrix_F17_InconsistentBlobRowIsRefetched(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/file$`), "deny")})
	body := bytes.Repeat([]byte("checked bytes "), 128)
	e.Upstream.Set("/file", testutil.Route{Body: body})
	mustCode(t, e.GET("/file"), 200)

	ctx := context.Background()
	object, err := e.Index.CurrentObject(ctx, "generic", "/file")
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlTruncate(e.Config.DataDir, object.SHA256); err != nil {
		t.Fatal(err)
	}

	healed := e.GET("/file")
	mustCode(t, healed, 200)
	if healed.Header().Get(env.AbortedHeader) != "" {
		t.Fatal("response was aborted")
	}
	if !bytes.Equal(healed.Body.Bytes(), body) {
		t.Fatalf("healed body has %d bytes, want %d", healed.Body.Len(), len(body))
	}
	mustHeader(t, healed, "X-Larder-Cache", "MISS")
	if got := e.Upstream.Count("/file"); got != 2 {
		t.Fatalf("upstream requests = %d, want 2", got)
	}

	repaired, err := e.Index.GetBlob(ctx, object.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if repaired.RawSize != int64(len(body)) || repaired.StoredSize <= 0 {
		t.Fatalf("repaired blob sizes = raw %d stored %d, want raw %d and stored > 0",
			repaired.RawSize, repaired.StoredSize, len(body))
	}
	if repaired.StoredEnc == "identity" && repaired.StoredSize != repaired.RawSize {
		t.Fatalf("identity blob sizes = raw %d stored %d", repaired.RawSize, repaired.StoredSize)
	}
}

// TestMatrix_F18_ExistingEmptyBlobFileIsReplaced starts from a zero-length
// file already sitting at the digest path and checks that the fetch replaces it
// instead of recording it as the object's bytes.
func TestMatrix_F18_ExistingEmptyBlobFileIsReplaced(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/file$`), "deny")})
	body := bytes.Repeat([]byte("tarball "), 64)
	sum := sha256.Sum256(body)
	path := blobPath(e, hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o640); err != nil {
		t.Fatal(err)
	}

	e.Upstream.Set("/file", testutil.Route{Body: body})
	rec := e.GET("/file")
	mustCode(t, rec, 200)
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("body has %d bytes, want %d", rec.Body.Len(), len(body))
	}
	object, err := e.Index.CurrentObject(context.Background(), "generic", "/file")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := e.Index.GetBlob(context.Background(), object.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if blob.RawSize != int64(len(body)) || blob.StoredSize <= 0 {
		t.Fatalf("blob sizes = raw %d stored %d, want raw %d and stored > 0",
			blob.RawSize, blob.StoredSize, len(body))
	}
}

func sqlTruncate(dataDir, sha string) error {
	path := filepath.Join(dataDir, "blobs", sha[:2], sha[2:4], sha)
	if err := os.Truncate(path, 0); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "index.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`UPDATE blobs SET stored_size = 0 WHERE sha256 = ?`, sha)
	return err
}

// TestMatrix_C45_ParallelFetchesStayConsistent downloads many distinct
// artifacts at once and requires every stored blob to verify clean.
func TestMatrix_C45_ParallelFetchesStayConsistent(t *testing.T) {
	e := env.New(t, env.Options{Repo: genericRepo(immutableRule(`^/pkg/.*\.tgz$`), "deny")})
	const count = 30
	bodies := make([][]byte, count)
	paths := make([]string, count)
	for i := range bodies {
		paths[i] = fmt.Sprintf("/pkg/%02d.tgz", i)
		bodies[i] = bytes.Repeat([]byte(fmt.Sprintf("package-%02d ", i)), 40+i)
		e.Upstream.Set(paths[i], testutil.Route{Body: bodies[i]})
	}

	var wg sync.WaitGroup
	failures := make(chan string, count)
	for i := range bodies {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := e.GET(paths[i])
			switch {
			case rec.Code != 200:
				failures <- fmt.Sprintf("%s: status %d", paths[i], rec.Code)
			case !bytes.Equal(rec.Body.Bytes(), bodies[i]):
				failures <- fmt.Sprintf("%s: body has %d bytes, want %d", paths[i], rec.Body.Len(), len(bodies[i]))
			}
		}(i)
	}
	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	if t.Failed() {
		return
	}

	ctx := context.Background()
	objects, err := e.Index.AllObjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != count {
		t.Fatalf("objects = %d, want %d", len(objects), count)
	}
	for _, object := range objects {
		blob, err := e.Index.GetBlob(ctx, object.SHA256)
		if err != nil {
			t.Fatalf("%s: %v", object.Key, err)
		}
		want := int64(len(bodies[0]))
		for i, path := range paths {
			if path == object.Key {
				want = int64(len(bodies[i]))
				break
			}
		}
		if blob.RawSize != want || blob.StoredSize <= 0 {
			t.Fatalf("%s: blob sizes raw=%d stored=%d, want raw=%d and stored > 0",
				object.Key, blob.RawSize, blob.StoredSize, want)
		}
		if blob.StoredEnc == "identity" && blob.StoredSize != blob.RawSize {
			t.Fatalf("%s: identity sizes raw=%d stored=%d", object.Key, blob.RawSize, blob.StoredSize)
		}
	}
	scrubber, err := integrity.NewScrubber(e.Store, e.Index, e.Metrics, nil, e.Clock)
	if err != nil {
		t.Fatal(err)
	}
	result, err := scrubber.Scan(ctx, integrity.ScrubOptions{Repo: "generic", Sample: 100})
	if err != nil {
		t.Fatal(err)
	}
	if result.Checked != count || result.Corrupt != 0 {
		t.Fatalf("scrub result = %+v", result)
	}
}
