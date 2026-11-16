package index

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestBlobAndObjectTotals(t *testing.T) {
	db := openIndex(t)
	ctx := context.Background()
	first := seedBlob(t, db, "first", "identity", 10)
	second := seedBlob(t, db, "second", "identity", 20)
	seedObject(t, db, "r1", "k1", "immutable", first, testNow)
	seedObject(t, db, "r1", "k2", "immutable", second, testNow)
	seedObject(t, db, "r2", "k3", "mutable", second, testNow)

	totals, err := db.BlobTotals(ctx)
	if err != nil || totals.Blobs != 2 || totals.StoredBytes != 30 {
		t.Fatalf("blob totals = %+v, %v", totals, err)
	}
	counts, err := db.ObjectCounts(ctx)
	if err != nil || len(counts) != 2 {
		t.Fatalf("object counts = %+v, %v", counts, err)
	}
	refcounts, err := db.RefCounts(ctx)
	if err != nil || refcounts[second.SHA256] != 2 {
		t.Fatalf("refcounts = %+v, %v", refcounts, err)
	}
}

func TestDeleteObjectsRemovesBlobAtZero(t *testing.T) {
	db := openIndex(t)
	ctx := context.Background()
	shared := seedBlob(t, db, "shared", "identity", 12)
	first := seedObject(t, db, "r1", "k1", "immutable", shared, testNow)
	second := seedObject(t, db, "r1", "k2", "immutable", shared, testNow)

	freed, err := db.DeleteObjects(ctx, []int64{first.ID, first.ID, 0, 99999})
	if err != nil {
		t.Fatal(err)
	}
	if len(freed) != 0 {
		t.Fatalf("freed shared blob early: %+v", freed)
	}
	refcount, err := db.BlobRefCount(ctx, shared.SHA256)
	if err != nil || refcount != 1 {
		t.Fatalf("refcount = %d, %v", refcount, err)
	}
	freed, err = db.DeleteObjects(ctx, []int64{second.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(freed) != 1 || freed[0].SHA256 != shared.SHA256 || freed[0].StoredSize != 12 {
		t.Fatalf("freed = %+v", freed)
	}
	if _, err := db.GetBlob(ctx, shared.SHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("blob row survived: %v", err)
	}
	if freed, err := db.DeleteObjects(ctx, nil); err != nil || len(freed) != 0 {
		t.Fatalf("empty delete = %+v, %v", freed, err)
	}
}

func TestZeroRefBlobCleanup(t *testing.T) {
	db := openIndex(t)
	ctx := context.Background()
	blob := seedBlob(t, db, "orphan", "identity", 9)
	object := seedObject(t, db, "r1", "k1", "immutable", blob, testNow)
	if err := db.DeleteObject(ctx, object.ID); err != nil {
		t.Fatal(err)
	}
	orphans, err := db.ZeroRefBlobs(ctx, 10)
	if err != nil || len(orphans) != 1 || orphans[0].SHA256 != blob.SHA256 {
		t.Fatalf("orphans = %+v, %v", orphans, err)
	}
	freed, err := db.DeleteBlobRows(ctx, []string{blob.SHA256, testSHA("missing")})
	if err != nil || len(freed) != 1 || freed[0].StoredSize != 9 {
		t.Fatalf("freed = %+v, %v", freed, err)
	}
	if _, err := db.GetBlob(ctx, blob.SHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("orphan survived: %v", err)
	}
}

func TestCandidatesHistoryAndRollback(t *testing.T) {
	db := openIndex(t)
	ctx := context.Background()
	blobs := make([]Blob, 0, 4)
	for i := 0; i < 4; i++ {
		blob := seedBlob(t, db, string(rune('a'+i))+" generations", "identity", int64(10+i))
		blobs = append(blobs, blob)
		seedObject(t, db, "r1", "/meta", "mutable", blob, testNow.Add(time.Duration(i)*time.Minute))
	}
	seedObject(t, db, "r1", "/other", "immutable", blobs[0], testNow)
	seedObject(t, db, "r2", "/meta", "mutable", blobs[1], testNow)

	current, err := db.CurrentObjects(ctx, "r1")
	if err != nil || len(current) != 2 {
		t.Fatalf("current = %+v, %v", current, err)
	}
	all, err := db.CurrentObjects(ctx, "")
	if err != nil || len(all) != 3 {
		t.Fatalf("all current = %+v, %v", all, err)
	}
	keys, err := db.KeyCandidates(ctx, "r1", "/meta")
	if err != nil || len(keys) != 4 {
		t.Fatalf("key candidates = %+v, %v", keys, err)
	}
	generation, err := db.ObjectGeneration(ctx, "r1", "/meta", 2)
	if err != nil || generation.Generation != 2 {
		t.Fatalf("generation = %+v, %v", generation, err)
	}
	if _, err := db.ObjectGeneration(ctx, "r1", "/meta", 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing generation error = %v", err)
	}

	history, err := db.HistoryCandidates(ctx, "r1", 2)
	if err != nil || len(history) != 2 {
		t.Fatalf("history = %+v, %v", history, err)
	}
	if history[0].Generation != 1 || history[1].Generation != 2 {
		t.Fatalf("history generations = %+v", history)
	}
	if _, err := db.HistoryCandidates(ctx, "", -1); err == nil {
		t.Fatal("negative keep accepted")
	}

	object, err := db.Rollback(ctx, "r1", "/meta", 0)
	if err != nil || object.Generation != 3 || !object.Current {
		t.Fatalf("default rollback = %+v, %v", object, err)
	}
	object, err = db.Rollback(ctx, "r1", "/meta", 1)
	if err != nil || object.Generation != 1 {
		t.Fatalf("explicit rollback = %+v, %v", object, err)
	}
	generations, err := db.Generations(ctx, "r1", "/meta")
	if err != nil || len(generations) != 4 {
		t.Fatalf("generations = %+v, %v", generations, err)
	}
	currentCount := 0
	for _, generation := range generations {
		if generation.Current {
			currentCount++
		}
	}
	if currentCount != 1 {
		t.Fatalf("current generations = %d", currentCount)
	}
	if _, err := db.Rollback(ctx, "r1", "/meta", 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing rollback error = %v", err)
	}
	if _, err := db.Rollback(ctx, "r1", "/missing", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing key error = %v", err)
	}
	if _, err := db.Rollback(ctx, "", "", 0); err == nil {
		t.Fatal("empty rollback accepted")
	}
}

func TestPinsAndExpectationHelpers(t *testing.T) {
	db := openIndex(t)
	ctx := context.Background()
	if err := db.CreatePinSet(ctx, PinSet{Name: "set", CreatedAt: testNow, Note: "note"}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddPinItem(ctx, PinItem{SetName: "set", Repo: "r1", Key: "/key"}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddPinRule(ctx, PinRule{SetName: "set", Repo: "r1", Pattern: "/glob/**"}); err != nil {
		t.Fatal(err)
	}
	items, err := db.PinItems(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("pin items = %+v, %v", items, err)
	}
	rules, err := db.PinRules(ctx)
	if err != nil || len(rules) != 1 {
		t.Fatalf("pin rules = %+v, %v", rules, err)
	}
	if !MatchGlob("/glob/**", "/glob/a") || MatchGlob("/glob/*", "/glob/a/b") {
		t.Fatal("MatchGlob behaviour differs")
	}

	if err := db.UpsertExpectations(ctx, []Expectation{{
		Repo: "r1", Key: "/key", Algo: "sha256", Digest: testSHA("key"),
		Source: "/index", CreatedAt: testNow,
	}}); err != nil {
		t.Fatal(err)
	}
	expectations, err := db.ExpectationsFor(ctx, "r1", "/key")
	if err != nil || len(expectations) != 1 || expectations[0].Algo != "sha256" {
		t.Fatalf("expectations = %+v, %v", expectations, err)
	}
}

func TestSetAccessCheckpointAndReadOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	blob := seedBlob(t, db, "readonly", "identity", 5)
	object := seedObject(t, db, "r1", "k1", "immutable", blob, testNow)
	at := testNow.Add(time.Hour)
	if err := db.SetAccess(ctx, object.ID, at, 7); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAccess(ctx, 0, at, 1); err == nil {
		t.Fatal("SetAccess accepted an empty id")
	}
	if err := db.SetAccess(ctx, object.ID, at, -1); err == nil {
		t.Fatal("SetAccess accepted negative hits")
	}
	updated, err := db.CurrentObject(ctx, "r1", "k1")
	if err != nil || updated.Hits != 7 || !updated.LastAccess.Equal(at) {
		t.Fatalf("updated object = %+v, %v", updated, err)
	}
	if err := db.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	readOnly, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	totals, err := readOnly.BlobTotals(ctx)
	if err != nil || totals.Blobs != 1 {
		t.Fatalf("read-only totals = %+v, %v", totals, err)
	}
	if err := readOnly.Close(); err != nil {
		t.Fatal(err)
	}
	if err := readOnly.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(""); err == nil {
		t.Fatal("empty read-only path accepted")
	}
	if _, err := OpenReadOnly(filepath.Join(dir, "missing.db")); err == nil {
		t.Fatal("missing database accepted")
	}
}

func TestRefcountAndDuplicateCurrentRepair(t *testing.T) {
	db := openIndex(t)
	ctx := context.Background()
	blob := seedBlob(t, db, "repair blob", "identity", 11)
	first := seedObject(t, db, "r1", "k1", "immutable", blob, testNow)
	second := seedObject(t, db, "r1", "k2", "immutable", blob, testNow)
	if _, err := db.sql.ExecContext(ctx, `UPDATE blobs SET refcount = 99 WHERE sha256 = ?`, blob.SHA256); err != nil {
		t.Fatal(err)
	}
	mismatches, err := db.RefcountMismatches(ctx)
	if err != nil || len(mismatches) != 1 || mismatches[0].Computed != 2 || mismatches[0].Stored != 99 {
		t.Fatalf("mismatches = %+v, %v", mismatches, err)
	}
	if err := db.RecomputeRefcounts(ctx); err != nil {
		t.Fatal(err)
	}
	updated, err := db.GetBlob(ctx, blob.SHA256)
	if err != nil || updated.RefCount != 2 {
		t.Fatalf("refcount = %+v, %v", updated, err)
	}

	if _, err := db.sql.ExecContext(ctx, `DROP INDEX objects_current`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.ExecContext(ctx, `UPDATE objects SET key = 'k1', current = 1 WHERE id IN (?, ?)`, first.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	duplicates, err := db.DuplicateCurrents(ctx)
	if err != nil || len(duplicates) != 1 {
		t.Fatalf("duplicates = %+v, %v", duplicates, err)
	}
	if err := db.FixDuplicateCurrents(ctx); err != nil {
		t.Fatal(err)
	}
	duplicates, err = db.DuplicateCurrents(ctx)
	if err != nil || len(duplicates) != 0 {
		t.Fatalf("duplicates after fix = %+v, %v", duplicates, err)
	}
	current, err := db.CurrentObject(ctx, "r1", "k1")
	if err != nil || !current.Current {
		t.Fatalf("newest generation is not current: %+v, %v", current, err)
	}
}

func TestIntegrityQueries(t *testing.T) {
	db := openIndex(t)
	ctx := context.Background()
	first := seedBlob(t, db, "integrity first", "identity", 15)
	second := seedBlob(t, db, "integrity second", "identity", 16)
	seedObject(t, db, "r1", "/a", "immutable", first, testNow)
	seedObject(t, db, "r1", "/b", "immutable", second, testNow)
	seedObject(t, db, "r2", "/c", "immutable", second, testNow)

	objects, err := db.AllObjects(ctx)
	if err != nil || len(objects) != 3 {
		t.Fatalf("objects = %+v, %v", objects, err)
	}
	blobs, err := db.AllBlobs(ctx)
	if err != nil || len(blobs) != 2 || blobs[0].SHA256 > blobs[1].SHA256 {
		t.Fatalf("blobs = %+v, %v", blobs, err)
	}
	page, err := db.BlobPage(ctx, blobs[0].SHA256, 1)
	if err != nil || len(page) != 1 || page[0].SHA256 != blobs[1].SHA256 {
		t.Fatalf("page = %+v, %v", page, err)
	}
	repoBlobs, err := db.BlobsForRepo(ctx, "r2")
	if err != nil || len(repoBlobs) != 1 || repoBlobs[0].SHA256 != second.SHA256 {
		t.Fatalf("repo blobs = %+v, %v", repoBlobs, err)
	}
	if _, err := db.BlobsForRepo(ctx, ""); err == nil {
		t.Fatal("empty repo accepted")
	}
	count, err := db.BlobCount(ctx)
	if err != nil || count != 2 {
		t.Fatalf("blob count = %d, %v", count, err)
	}

	if err := db.UpsertExpectations(ctx, []Expectation{
		{Repo: "r1", Key: "/a", Algo: "sha256", Digest: first.SHA256, Source: "/index", CreatedAt: testNow},
		{Repo: "r1", Key: "/b", Algo: "sha256", Digest: testSHA("wrong"), Source: "/index", CreatedAt: testNow},
		{Repo: "r2", Key: "/gone", Algo: "sha256", Digest: testSHA("gone"), Source: "/index", CreatedAt: testNow},
		{Repo: "r2", Key: "/c", Algo: "sha256", Digest: second.SHA256, Source: "/index", CreatedAt: testNow},
	}); err != nil {
		t.Fatal(err)
	}
	mismatches, err := db.ExpectationMismatches(ctx, "r1")
	if err != nil || len(mismatches) != 1 || mismatches[0].Key != "/b" {
		t.Fatalf("mismatches = %+v, %v", mismatches, err)
	}
	if all, err := db.ExpectationMismatches(ctx, ""); err != nil || len(all) != 1 {
		t.Fatalf("all mismatches = %+v, %v", all, err)
	}
	rows, err := db.ExpectationRows(ctx, "")
	if err != nil || len(rows) != 3 {
		t.Fatalf("expectation rows = %+v, %v", rows, err)
	}
	if filtered, err := db.ExpectationRows(ctx, "r2"); err != nil || len(filtered) != 1 {
		t.Fatalf("filtered expectation rows = %+v, %v", filtered, err)
	}
	unverified, err := db.UnverifiedCount(ctx, "")
	if err != nil || unverified != 3 {
		t.Fatalf("unverified = %d, %v", unverified, err)
	}
	if _, err := db.UnverifiedCount(ctx, "r2"); err != nil {
		t.Fatal(err)
	}
	orphans, err := db.ExpectationOrphanCount(ctx)
	if err != nil || orphans != 1 {
		t.Fatalf("expectation orphans = %d, %v", orphans, err)
	}
	removed, err := db.DeleteExpectationOrphans(ctx)
	if err != nil || removed != 1 {
		t.Fatalf("removed expectations = %d, %v", removed, err)
	}

	snapshot := filepath.Join(t.TempDir(), "snapshot.db")
	if err := db.VacuumInto(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	readOnly, err := OpenReadOnly(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := readOnly.BlobCount(ctx); err != nil || count != 2 {
		t.Fatalf("snapshot count = %d, %v", count, err)
	}
	_ = readOnly.Close()
	if err := db.VacuumInto(ctx, ""); err == nil {
		t.Fatal("empty snapshot path accepted")
	}
}

func TestDeleteBlobAndObjects(t *testing.T) {
	db := openIndex(t)
	ctx := context.Background()
	shared := seedBlob(t, db, "delete blob objects", "identity", 20)
	seedObject(t, db, "r1", "/a", "immutable", shared, testNow)
	seedObject(t, db, "r2", "/b", "mutable", shared, testNow)
	refs, err := db.DeleteBlobAndObjects(ctx, shared.SHA256)
	if err != nil || len(refs) != 2 {
		t.Fatalf("refs = %+v, %v", refs, err)
	}
	if _, err := db.GetBlob(ctx, shared.SHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("blob survived: %v", err)
	}
	if _, err := db.CurrentObject(ctx, "r1", "/a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("object survived: %v", err)
	}
	refs, err = db.DeleteBlobAndObjects(ctx, testSHA("absent"))
	if err != nil || len(refs) != 0 {
		t.Fatalf("absent refs = %+v, %v", refs, err)
	}
}
