package cache_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/testutil"
	"github.com/mmrzaf/larder/testutil/env"
)

func assertAborted(t *testing.T, rec interface{ Header() http.Header }) {
	t.Helper()
	if rec.Header().Get(env.AbortedHeader) != "1" {
		t.Fatal("a response cut short was not aborted, so a client could mistake it for a complete one")
	}
}

func TestMatrix_F01_StreamedMissInterruptedUpstreamAbortsAndStoresNothing(t *testing.T) {
	e := env.New(t, env.Options{Repo: testRepo()})
	body := bytes.Repeat([]byte("0123456789abcdef"), 4096)
	e.Upstream.Set("/plain/a", testutil.Route{Body: body, BreakAfter: 20000})

	rec := e.GET("/plain/a")
	assertAborted(t, rec)
	if _, err := e.Index.CurrentObject(context.Background(), "test", "/plain/a"); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("interrupted download left an object behind: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(e.Store.DataDir(), "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("%d temporary files remain after an interrupted download", len(entries))
	}

	e.Upstream.Set("/plain/a", testutil.Route{Body: body})
	healthy := e.GET("/plain/a")
	mustCode(t, healthy, 200)
	if !bytes.Equal(healthy.Body.Bytes(), body) {
		t.Fatal("body after recovery differs from the upstream")
	}
	if _, err := e.Index.CurrentObject(context.Background(), "test", "/plain/a"); err != nil {
		t.Fatalf("complete download was not stored: %v", err)
	}
}

func TestMatrix_F02_PassthroughInterruptedUpstreamAborts(t *testing.T) {
	e := env.New(t, env.Options{Repo: testRepo()})
	body := bytes.Repeat([]byte("x"), 100000)
	e.Upstream.Set("/passthrough", testutil.Route{Body: body, BreakAfter: 1000})
	assertAborted(t, e.GET("/passthrough"))
}

func TestMatrix_F03_VerifiedMissInterruptedUpstreamIsA502(t *testing.T) {
	e := env.New(t, env.Options{Repo: testRepo()})
	body := bytes.Repeat([]byte("verified "), 20000)
	sum := sha256.Sum256(body)
	path := "/expect/" + hex.EncodeToString(sum[:])
	e.Upstream.Set(path, testutil.Route{Body: body, BreakAfter: 5000})

	rec := e.GET(path)
	mustCode(t, rec, 502)
	if rec.Header().Get(env.AbortedHeader) != "" {
		t.Fatal("nothing had been sent, so the connection must not be aborted")
	}
	if _, err := e.Index.CurrentObject(context.Background(), "test", path); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("interrupted download was stored: %v", err)
	}
}

func TestMatrix_F04_LocalStorageFailureIsNotChargedToTheUpstream(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not bind root")
	}
	e := env.New(t, env.Options{Repo: testRepo()})
	body := bytes.Repeat([]byte("payload "), 512)
	for i := 0; i < 8; i++ {
		e.Upstream.Set(fmt.Sprintf("/plain/%d", i), testutil.Route{Body: body})
	}
	tmp := filepath.Join(e.Store.DataDir(), "tmp")
	if err := os.Chmod(tmp, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tmp, 0o750) })

	for i := 0; i < 8; i++ {
		rec := e.GET(fmt.Sprintf("/plain/%d", i))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("request %d: status = %d, want 500 for a local failure; %s", i, rec.Code, env.Summary(rec))
		}
	}
	for _, state := range e.Engine.UpstreamStates() {
		if state.Open {
			t.Fatalf("upstream %s circuit opened because of a local storage failure", state.Upstream)
		}
	}

	if err := os.Chmod(tmp, 0o750); err != nil {
		t.Fatal(err)
	}
	rec := e.GET("/plain/0")
	mustCode(t, rec, 200)
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatal("body after the storage problem was fixed differs")
	}
}

type fullDisk struct{}

func (fullDisk) Bypass(string) bool                      { return false }
func (fullDisk) Reserve(int64) (release func(), ok bool) { return nil, false }

func TestMatrix_F05_RefusedReservationStreamsWithoutStoring(t *testing.T) {
	e := env.New(t, env.Options{Repo: testRepo(), Bypass: fullDisk{}})
	body := bytes.Repeat([]byte("big "), 8192)
	e.Upstream.Set("/plain/big", testutil.Route{Body: body})
	rec := e.GET("/plain/big")
	mustCode(t, rec, 200)
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatal("streamed body differs from the upstream")
	}
	mustHeader(t, rec, "X-Larder-Cache", "BYPASS")
	if _, err := e.Index.CurrentObject(context.Background(), "test", "/plain/big"); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("object stored although the reservation was refused: %v", err)
	}
}

// Many keys share one blob while a sweeper removes unreferenced blobs. A
// response must never be built on a blob the sweeper removed, and no
// reference may outlive its file.
func TestMatrix_F09_PublicationRacingBlobSweepStaysConsistent(t *testing.T) {
	e := env.New(t, env.Options{Repo: testRepo()})
	body := bytes.Repeat([]byte("shared content "), 256)
	const keys = 24
	for i := 0; i < keys; i++ {
		e.Upstream.Set(fmt.Sprintf("/plain/%d", i), testutil.Route{Body: body})
	}

	ctx := context.Background()
	stop := make(chan struct{})
	var sweeper sync.WaitGroup
	sweeper.Add(1)
	go func() {
		defer sweeper.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = e.Store.Reclaim(func() error {
				blobs, err := e.Index.ZeroRefBlobs(ctx, 0)
				if err != nil {
					return err
				}
				shas := make([]string, 0, len(blobs))
				for _, blob := range blobs {
					shas = append(shas, blob.SHA256)
				}
				freed, err := e.Index.DeleteBlobRows(ctx, shas)
				if err != nil {
					return err
				}
				for _, blob := range freed {
					_ = e.Store.Delete(blob.SHA256)
				}
				return nil
			})
		}
	}()

	var workers sync.WaitGroup
	for w := 0; w < 8; w++ {
		workers.Add(1)
		go func(w int) {
			defer workers.Done()
			for round := 0; round < 6; round++ {
				for i := 0; i < keys; i++ {
					key := (i + w*3) % keys
					rec := e.GET(fmt.Sprintf("/plain/%d", key))
					if rec.Code == 200 && !bytes.Equal(rec.Body.Bytes(), body) {
						t.Errorf("key %d: wrong body served", key)
						return
					}
					if rec.Code != 200 && rec.Code != 500 && rec.Code != 502 {
						t.Errorf("key %d: unexpected status %d", key, rec.Code)
						return
					}
				}
			}
		}(w)
	}
	workers.Wait()
	close(stop)
	sweeper.Wait()

	// Whatever was stored must be complete and readable.
	for i := 0; i < keys; i++ {
		object, err := e.Index.CurrentObject(ctx, "test", fmt.Sprintf("/plain/%d", i))
		if errors.Is(err, index.ErrNotFound) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(e.Store.DataDir(), "blobs", object.SHA256[:2], object.SHA256[2:4], object.SHA256)); err != nil {
			t.Fatalf("object /plain/%d references a blob that is gone: %v", i, err)
		}
	}
}

func TestMatrix_F15_RefusedReservationStreamsMetadataWithoutStoring(t *testing.T) {
	e := env.New(t, env.Options{Repo: testRepo(), Bypass: fullDisk{}})
	body := bytes.Repeat([]byte("index "), 4096)
	e.Upstream.Set("/doc", testutil.Route{Body: body})
	rec := e.GET("/doc")
	mustCode(t, rec, 200)
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatal("streamed document differs from the upstream")
	}
	mustHeader(t, rec, "X-Larder-Cache", "BYPASS")
	if _, err := e.Index.CurrentObject(context.Background(), "test", "/doc"); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("document stored although the reservation was refused: %v", err)
	}
}
