package integrity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/store"
)

func storeWriterOptions(compress bool) store.WriterOptions {
	return store.WriterOptions{Compress: compress}
}

func TestMatrix_C30_ReindexOrphans(t *testing.T) {
	f := newScrubFixture(t)
	missing := f.add(t, "repo", "/missing", []byte("missing content"), false)
	if err := os.Remove(mustPath(t, f, missing.SHA256)); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	orphanSHA := testutilSHA("orphan file content")
	orphanPath := filepath.Join(f.dir, "blobs", orphanSHA[:2], orphanSHA[2:4], orphanSHA)
	if err := os.MkdirAll(filepath.Dir(orphanPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphanPath, []byte("orphan file content"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(orphanPath, old, old); err != nil {
		t.Fatal(err)
	}
	zeroSHA := testutilSHA("zero ref row")
	zeroPath := filepath.Join(f.dir, "blobs", zeroSHA[:2], zeroSHA[2:4], zeroSHA)
	if err := os.MkdirAll(filepath.Dir(zeroPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zeroPath, []byte("zero ref row"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(zeroPath, old, old); err != nil {
		t.Fatal(err)
	}
	if err := f.index.PutBlob(context.Background(), index.Blob{
		SHA256: zeroSHA, RawSize: 14, StoredSize: 14, StoredEnc: "identity", CreatedAt: old,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.index.UpsertExpectations(context.Background(), []index.Expectation{{
		Repo: "repo", Key: "/gone", Algo: "sha256", Digest: testutilSHA("gone"),
		Source: "/index", CreatedAt: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}

	report, err := Reindex(context.Background(), f.store, f.index)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.ObjectsMissingBlobs) != 1 || report.ObjectsMissingBlobs[0].Key != "/missing" {
		t.Fatalf("missing objects = %+v", report.ObjectsMissingBlobs)
	}
	if len(report.OrphanFiles) != 1 || report.OrphanFiles[0].SHA256 != orphanSHA {
		t.Fatalf("orphan files = %+v", report.OrphanFiles)
	}
	if len(report.BlobsWithoutObjects) != 1 || report.BlobsWithoutObjects[0].SHA256 != zeroSHA {
		t.Fatalf("zero-ref rows = %+v", report.BlobsWithoutObjects)
	}
	if report.ExpectationOrphans != 1 {
		t.Fatalf("expectation orphans = %d", report.ExpectationOrphans)
	}
	if report.Problems() == 0 {
		t.Fatal("problems not reported")
	}

	result, err := Fix(context.Background(), f.store, f.index, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Problems() != 0 {
		t.Fatalf("fix left problems: %+v", result)
	}
	if result.Repair.ObjectsRemoved != 1 || result.Repair.OrphanFilesRemoved != 2 || result.Repair.ExpectationsRemoved != 1 {
		t.Fatalf("fix summary = %+v", result.Repair)
	}
	for _, path := range []string{orphanPath, zeroPath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived fix", path)
		}
	}
	if _, err := f.index.CurrentObject(context.Background(), "repo", "/missing"); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("missing object survived: %v", err)
	}
}

func TestReindexKeepsRecentOrphans(t *testing.T) {
	f := newScrubFixture(t)
	sha := testutilSHA("recent orphan")
	path := filepath.Join(f.dir, "blobs", sha[:2], sha[2:4], sha)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("recent orphan"), 0o640); err != nil {
		t.Fatal(err)
	}
	report, err := Fix(context.Background(), f.store, f.index, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.OrphanFiles) != 1 {
		t.Fatalf("recent orphan removed: %+v", report.OrphanFiles)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("recent orphan file removed")
	}
}

func TestMatrix_S19_SnapshotRestoreAndFix(t *testing.T) {
	f := newScrubFixture(t)
	first := f.add(t, "repo", "/first", []byte("first content"), false)
	second := f.add(t, "repo", "/second", []byte("second content"), false)
	snapshot := filepath.Join(t.TempDir(), "index-snapshot.db")
	if err := SnapshotTo(context.Background(), f.index, snapshot); err != nil {
		t.Fatal(err)
	}
	third := f.add(t, "repo", "/third", []byte("third content"), false)
	if err := f.index.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Restore(context.Background(), f.dir, snapshot); err != nil {
		t.Fatal(err)
	}
	restored, err := index.Open(filepath.Join(f.dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	f.index = restored
	if _, err := restored.CurrentObject(context.Background(), "repo", "/third"); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("object outside the snapshot survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "index.db.bak")); err != nil {
		t.Fatalf("backup index missing: %v", err)
	}

	if err := os.Remove(mustPath(t, f, first.SHA256)); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	thirdPath := mustPath(t, f, third.SHA256)
	if err := os.Chtimes(thirdPath, old, old); err != nil {
		t.Fatal(err)
	}
	report, err := Reindex(context.Background(), f.store, restored)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.ObjectsMissingBlobs) == 0 {
		t.Fatalf("missing blob not reported: %+v", report)
	}
	orphan := testutilSHA("restored orphan")
	orphanPath := filepath.Join(f.dir, "blobs", orphan[:2], orphan[2:4], orphan)
	if err := os.MkdirAll(filepath.Dir(orphanPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphanPath, []byte("restored orphan"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(orphanPath, old, old); err != nil {
		t.Fatal(err)
	}
	result, err := Fix(context.Background(), f.store, restored, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Problems() != 0 {
		t.Fatalf("restored store still inconsistent: %+v", result)
	}
	if _, err := restored.CurrentObject(context.Background(), "repo", "/first"); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("row without blob survived: %v", err)
	}
	if _, err := restored.CurrentObject(context.Background(), "repo", "/second"); err != nil {
		t.Fatalf("healthy restored object missing: %v", err)
	}
	if _, err := os.Stat(orphanPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("restored orphan survived fix")
	}
	_ = second
}

func TestRestoreWithoutLiveIndex(t *testing.T) {
	f := newScrubFixture(t)
	f.add(t, "repo", "/first", []byte("first content"), false)
	snapshot := filepath.Join(t.TempDir(), "index.db")
	if err := SnapshotTo(context.Background(), f.index, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := f.index.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.dir, "index.db")); err != nil {
		t.Fatal(err)
	}
	if err := Restore(context.Background(), f.dir, snapshot); err != nil {
		t.Fatal(err)
	}
	restored, err := index.Open(filepath.Join(f.dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	f.index = restored
	if _, err := restored.CurrentObject(context.Background(), "repo", "/first"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "index.db.bak")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("backup created without a previous index")
	}
}

func TestSnapshotRetentionAndErrors(t *testing.T) {
	f := newScrubFixture(t)
	f.add(t, "repo", "/first", []byte("first content"), false)
	dir := filepath.Join(t.TempDir(), "snapshots")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := 0; i < 3; i++ {
		path := filepath.Join(dir, SnapshotName(now.Add(time.Duration(i)*time.Hour)))
		if err := SnapshotTo(context.Background(), f.index, path); err != nil {
			t.Fatal(err)
		}
		at := now.Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := PruneSnapshots(dir, 2)
	if err != nil || len(removed) != 1 {
		t.Fatalf("removed = %+v, %v", removed, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries = %v, %v", entries, err)
	}
	if err := SnapshotTo(context.Background(), f.index, filepath.Join(dir, entries[0].Name())); err == nil {
		t.Fatal("existing snapshot destination accepted")
	}
	if _, err := PruneSnapshots(dir, -1); err == nil {
		t.Fatal("negative keep accepted")
	}
	snapshot := filepath.Join(t.TempDir(), "bad.db")
	if err := os.WriteFile(snapshot, []byte("not a database"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := Restore(context.Background(), f.dir, snapshot); err == nil {
		t.Fatal("invalid snapshot restored")
	}
	if err := Restore(context.Background(), f.dir, ""); err == nil {
		t.Fatal("empty restore accepted")
	}
}

func TestRebuildBlobsOnly(t *testing.T) {
	f := newScrubFixture(t)
	writer, err := f.store.NewWriter(storeWriterOptions(false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("identity rebuild")); err != nil {
		t.Fatal(err)
	}
	identity, _, err := writer.Commit()
	if err != nil {
		t.Fatal(err)
	}
	writer, err = f.store.NewWriter(storeWriterOptions(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("compressed rebuild content")); err != nil {
		t.Fatal(err)
	}
	gzipBlob, _, err := writer.Commit()
	if err != nil {
		t.Fatal(err)
	}
	result, err := RebuildBlobsOnly(context.Background(), f.store, f.index)
	if err != nil {
		t.Fatal(err)
	}
	if result.Blobs != 2 {
		t.Fatalf("rebuilt %d blobs", result.Blobs)
	}
	row, err := f.index.GetBlob(context.Background(), identity.SHA256)
	if err != nil || row.StoredEnc != "identity" {
		t.Fatalf("identity row = %+v, %v", row, err)
	}
	row, err = f.index.GetBlob(context.Background(), gzipBlob.SHA256)
	if err != nil || row.StoredEnc != "gzip" || row.RawSize != gzipBlob.RawSize {
		t.Fatalf("gzip row = %+v, %v", row, err)
	}
	if _, err := RebuildBlobsOnly(context.Background(), f.store, f.index); err == nil {
		t.Fatal("rebuild accepted a populated index")
	}
	if Warning("index.db") == "" {
		t.Fatal("warning is empty")
	}
}

func mustPath(t *testing.T, f *scrubFixture, sha string) string {
	t.Helper()
	path, err := f.store.BlobPath(sha)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReconcileStartup(t *testing.T) {
	f := newScrubFixture(t)
	missing := f.add(t, "repo", "/missing", []byte("missing content"), false)
	if err := os.Remove(mustPath(t, f, missing.SHA256)); err != nil {
		t.Fatal(err)
	}
	oldSHA := testutilSHA("old orphan startup")
	oldPath := filepath.Join(f.dir, "blobs", oldSHA[:2], oldSHA[2:4], oldSHA)
	if err := os.MkdirAll(filepath.Dir(oldPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath, []byte("old orphan startup"), 0o640); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldPath, old, old); err != nil {
		t.Fatal(err)
	}
	recentSHA := testutilSHA("recent orphan startup")
	recentPath := filepath.Join(f.dir, "blobs", recentSHA[:2], recentSHA[2:4], recentSHA)
	if err := os.MkdirAll(filepath.Dir(recentPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recentPath, []byte("recent orphan startup"), 0o640); err != nil {
		t.Fatal(err)
	}
	result, err := Reconcile(context.Background(), f.store, f.index, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ObjectsRemoved != 1 || result.BlobRowsRemoved != 1 || result.OrphanFilesRemoved != 1 {
		t.Fatalf("reconcile = %+v", result)
	}
	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old orphan survived startup")
	}
	if _, err := os.Stat(recentPath); err != nil {
		t.Fatal("recent orphan removed at startup")
	}
}
