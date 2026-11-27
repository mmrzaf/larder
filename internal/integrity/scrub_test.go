package integrity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/metrics"
	"github.com/mmrzaf/larder/internal/store"
	"github.com/mmrzaf/larder/testutil"
)

type scrubFixture struct {
	store   *store.Store
	index   *index.DB
	metrics *metrics.Metrics
	clock   *testutil.FakeClock
	dir     string
}

func newScrubFixture(t *testing.T) *scrubFixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := index.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	metricSet, err := metrics.New("test")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &scrubFixture{
		store: st, index: idx, metrics: metricSet, dir: dir,
		clock: testutil.NewFakeClock(time.Unix(1_700_000_000, 0)),
	}
	t.Cleanup(func() {
		_ = idx.Close()
		_ = st.Close()
	})
	return fixture
}

func (f *scrubFixture) add(t *testing.T, repo, key string, data []byte, compress bool) index.Blob {
	t.Helper()
	writer, err := f.store.NewWriter(store.WriterOptions{Compress: compress})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	blob, _, err := writer.Commit()
	if err != nil {
		t.Fatal(err)
	}
	row := index.Blob{
		SHA256: blob.SHA256, RawSize: blob.RawSize, StoredSize: blob.StoredSize,
		StoredEnc: string(blob.StoredEnc), CreatedAt: f.clock.Now(),
	}
	if err := f.index.PutBlob(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if _, err := f.index.PutObject(context.Background(), index.PutObjectParams{
		Repo: repo, Key: key, Class: "immutable", SHA256: blob.SHA256,
		FetchedAt: f.clock.Now(), ValidatedAt: f.clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	return row
}

func (f *scrubFixture) scrubber(t *testing.T) *Scrubber {
	t.Helper()
	scrubber, err := NewScrubber(f.store, f.index, f.metrics, nil, f.clock)
	if err != nil {
		t.Fatal(err)
	}
	return scrubber
}

func TestMatrix_L15_DryRunReportsObjectCount(t *testing.T) {
	f := newScrubFixture(t)
	bad := f.add(t, "repo", "/bad", []byte("corrupt me"), false)
	path, err := f.store.BlobPath(bad.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	result, err := f.scrubber(t).Scan(context.Background(), ScrubOptions{Sample: 100, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Corrupt != 1 || len(result.CorruptBlobs) != 1 {
		t.Fatalf("dry run result = %+v", result)
	}
	if got := result.CorruptBlobs[0].Objects; got != 1 {
		t.Fatalf("dry-run object count = %d, want 1", got)
	}
	if _, err := f.index.CurrentObject(context.Background(), "repo", "/bad"); err != nil {
		t.Fatalf("dry run removed the object: %v", err)
	}
}

func TestMatrix_S16_ScrubCorruption(t *testing.T) {
	f := newScrubFixture(t)
	bad := f.add(t, "repo", "/bad", []byte("corrupt me"), false)
	good := f.add(t, "repo", "/good", []byte("good bytes here"), false)
	gz := f.add(t, "repo", "/gzip", []byte("compressed metadata document"), true)

	path, err := f.store.BlobPath(bad.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := bytes.Repeat([]byte("x"), int(bad.RawSize))
	if err := os.WriteFile(path, corrupt, 0o640); err != nil {
		t.Fatal(err)
	}

	result, err := f.scrubber(t).Scan(context.Background(), ScrubOptions{Sample: 100})
	if err != nil {
		t.Fatal(err)
	}
	if result.Checked != 3 || result.Corrupt != 1 || result.Quarantined != 1 || result.RemovedObjects != 1 {
		t.Fatalf("scrub result = %+v", result)
	}
	if len(result.CorruptBlobs) != 1 || result.CorruptBlobs[0].SHA256 != bad.SHA256 || result.CorruptBlobs[0].Reason != "digest" {
		t.Fatalf("corrupt blobs = %+v", result.CorruptBlobs)
	}
	if value := f.metrics.ScrubCorrupt.With().Value(); value != 1 {
		t.Fatalf("corrupt metric = %v", value)
	}
	if _, err := f.index.GetBlob(context.Background(), bad.SHA256); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("corrupt blob row survived: %v", err)
	}
	if _, err := f.index.CurrentObject(context.Background(), "repo", "/bad"); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("corrupt object survived: %v", err)
	}
	quarantined := filepath.Join(f.dir, "quarantine", bad.SHA256)
	if _, err := os.Stat(quarantined); err != nil {
		t.Fatalf("quarantined file missing: %v", err)
	}
	for _, blob := range []index.Blob{good, gz} {
		if _, err := f.index.GetBlob(context.Background(), blob.SHA256); err != nil {
			t.Fatalf("healthy blob removed: %v", err)
		}
	}
	result, err = f.scrubber(t).Scan(context.Background(), ScrubOptions{Sample: 100})
	if err != nil || result.Corrupt != 0 {
		t.Fatalf("second scrub = %+v, %v", result, err)
	}
}

func TestScrubTruncatedAndMissing(t *testing.T) {
	f := newScrubFixture(t)
	truncated := f.add(t, "repo", "/truncated", []byte("truncate this content"), false)
	missing := f.add(t, "repo", "/missing", []byte("missing content"), false)
	path, _ := f.store.BlobPath(truncated.SHA256)
	if err := os.Truncate(path, 4); err != nil {
		t.Fatal(err)
	}
	missingPath, _ := f.store.BlobPath(missing.SHA256)
	if err := os.Remove(missingPath); err != nil {
		t.Fatal(err)
	}
	result, err := f.scrubber(t).Scan(context.Background(), ScrubOptions{Sample: 100})
	if err != nil {
		t.Fatal(err)
	}
	if result.Corrupt != 2 || result.Missing != 1 {
		t.Fatalf("result = %+v", result)
	}
	reasons := map[string]string{}
	for _, blob := range result.CorruptBlobs {
		reasons[blob.SHA256] = blob.Reason
	}
	if reasons[truncated.SHA256] == "" || reasons[missing.SHA256] != "missing" {
		t.Fatalf("reasons = %+v", reasons)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "quarantine", missing.SHA256)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing blob produced a quarantine entry")
	}
}

func TestScrubRollingCursor(t *testing.T) {
	f := newScrubFixture(t)
	var shas []string
	for i := 0; i < 5; i++ {
		blob := f.add(t, "repo", "/cursor-"+string(rune('a'+i)), []byte("cursor content "+string(rune('a'+i))), false)
		shas = append(shas, blob.SHA256)
	}
	sort.Strings(shas)
	scrubber := f.scrubber(t)
	checked := int64(0)
	cursor := ""
	for page := 0; page < 4; page++ {
		result, err := scrubber.Scan(context.Background(), ScrubOptions{Sample: 100, Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		checked += result.Checked
		cursor = result.NextCursor
		if cursor == "" {
			break
		}
	}
	if cursor != "" {
		t.Fatalf("cursor did not finish: %q", cursor)
	}
	if checked != 5 {
		t.Fatalf("rolling scan checked %d blobs", checked)
	}

	all, err := scrubber.Scan(context.Background(), ScrubOptions{Sample: 100})
	if err != nil || all.Checked != 5 {
		t.Fatalf("full scan = %+v, %v", all, err)
	}
	filtered, err := scrubber.Scan(context.Background(), ScrubOptions{Sample: 100, Repo: "other"})
	if err != nil || filtered.Checked != 0 {
		t.Fatalf("repo filter = %+v, %v", filtered, err)
	}
}

func TestScrubCheckExpectations(t *testing.T) {
	f := newScrubFixture(t)
	blob := f.add(t, "repo", "/mismatch", []byte("expectation subject"), false)
	if err := f.index.UpsertExpectations(context.Background(), []index.Expectation{{
		Repo: "repo", Key: "/mismatch", Algo: "sha256", Digest: testutilSHA("different"),
		Source: "/index", CreatedAt: f.clock.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	f.add(t, "repo", "/unverified", []byte("unverified subject"), false)
	result, err := f.scrubber(t).Scan(context.Background(), ScrubOptions{Sample: 100, CheckExpectations: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Expectations) != 1 || result.Expectations[0].Key != "/mismatch" {
		t.Fatalf("expectations = %+v", result.Expectations)
	}
	if result.Unverified != 2 {
		t.Fatalf("unverified = %d", result.Unverified)
	}
	_ = blob
}

func TestScrubRateLimiter(t *testing.T) {
	clock := testutil.NewFakeClock(time.Unix(1_700_000_000, 0))
	limiter := NewRateLimiter(100, clock)
	var delays []time.Duration
	limiter.wait = func(_ context.Context, d time.Duration) error {
		delays = append(delays, d)
		clock.Advance(d)
		return nil
	}
	if err := limiter.Wait(context.Background(), 50); err != nil {
		t.Fatal(err)
	}
	if err := limiter.Wait(context.Background(), 50); err != nil {
		t.Fatal(err)
	}
	if len(delays) != 2 || delays[0] != 500*time.Millisecond || delays[1] != 500*time.Millisecond {
		t.Fatalf("delays = %+v", delays)
	}
	unlimited := NewRateLimiter(0, clock)
	if err := unlimited.Wait(context.Background(), 1<<30); err != nil {
		t.Fatal(err)
	}
}

func testutilSHA(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
