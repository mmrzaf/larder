package index

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mmrzaf/larder/testutil"
)

var testNow = time.Unix(1_700_000_000, 0).UTC()

func openIndex(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func testSHA(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

func seedBlob(t *testing.T, db *DB, data string, enc string, storedSize int64) Blob {
	t.Helper()
	blob := Blob{
		SHA256:     testSHA(data),
		RawSize:    int64(len(data)),
		StoredSize: storedSize,
		StoredEnc:  enc,
		CreatedAt:  testNow,
	}
	if err := db.PutBlob(context.Background(), blob); err != nil {
		t.Fatal(err)
	}
	return blob
}

func seedObject(t *testing.T, db *DB, repo, key, class string, blob Blob, at time.Time) Object {
	t.Helper()
	obj, err := db.PutObject(context.Background(), PutObjectParams{
		Repo:         repo,
		Key:          key,
		Class:        class,
		SHA256:       blob.SHA256,
		ContentType:  "application/octet-stream",
		ETag:         `"etag"`,
		LastModified: "Mon, 02 Jan 2006 15:04:05 GMT",
		Upstream:     "upstream.example",
		FetchedAt:    at,
		ValidatedAt:  at,
	})
	if err != nil {
		t.Fatal(err)
	}
	return obj
}

func TestMatrix_S17_RefcountSharedBlob(t *testing.T) {
	db := openIndex(t)
	ctx := context.Background()
	shared := seedBlob(t, db, "shared bytes", "identity", 12)

	first := seedObject(t, db, "r1", "k1", "immutable", shared, testNow)
	second := seedObject(t, db, "r2", "k2", "immutable", shared, testNow.Add(time.Second))

	refcount, err := db.BlobRefCount(ctx, shared.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if refcount != 2 {
		t.Fatalf("refcount = %d, want 2", refcount)
	}

	if err := db.DeleteObject(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	refcount, err = db.BlobRefCount(ctx, shared.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if refcount != 1 {
		t.Fatalf("refcount after first delete = %d, want 1", refcount)
	}
	if _, err := db.GetBlob(ctx, shared.SHA256); err != nil {
		t.Fatalf("shared blob disappeared early: %v", err)
	}

	if err := db.DeleteObject(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	refcount, err = db.BlobRefCount(ctx, shared.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if refcount != 0 {
		t.Fatalf("refcount after last delete = %d, want 0", refcount)
	}
	if _, err := db.GetBlob(ctx, shared.SHA256); err != nil {
		t.Fatalf("blob row removed before garbage collection: %v", err)
	}
	if _, err := db.CurrentObject(ctx, "r1", "k1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted object still present: %v", err)
	}
}

func TestMatrix_C32_AccessRecorder(t *testing.T) {
	db := openIndex(t)
	ctx := context.Background()
	blob := seedBlob(t, db, "accessed bytes", "identity", 14)
	obj := seedObject(t, db, "repo", "key", "immutable", blob, testNow)

	before, err := db.CurrentObject(ctx, "repo", "key")
	if err != nil {
		t.Fatal(err)
	}
	if before.Hits != 0 || !before.LastAccess.Equal(testNow) {
		t.Fatalf("unexpected initial access values: %#v", before)
	}

	clock := testutil.NewFakeClock(testNow)
	recorder := NewAccessRecorder(db, clock, 30*time.Second, nil)
	flushed := make(chan struct{}, 4)
	var once sync.Once
	recorder.onFlush = func() { once.Do(func() { flushed <- struct{}{} }) }

	recorder.Record(obj.ID)
	unchanged, err := db.CurrentObject(ctx, "repo", "key")
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Hits != 0 {
		t.Fatalf("Record wrote synchronously: hits = %d", unchanged.Hits)
	}

	recorder.Start()
	clock.Advance(30 * time.Second)
	select {
	case <-flushed:
	case <-time.After(2 * time.Second):
		t.Fatal("periodic flush did not run")
	}
	flushedObj, err := db.CurrentObject(ctx, "repo", "key")
	if err != nil {
		t.Fatal(err)
	}
	if flushedObj.Hits != 1 {
		t.Fatalf("hits after periodic flush = %d, want 1", flushedObj.Hits)
	}
	if !flushedObj.LastAccess.Equal(testNow) {
		t.Fatalf("last_access after periodic flush = %v, want %v", flushedObj.LastAccess, testNow)
	}

	clock.Advance(5 * time.Second)
	recorder.Record(obj.ID)
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	final, err := db.CurrentObject(ctx, "repo", "key")
	if err != nil {
		t.Fatal(err)
	}
	if final.Hits != 2 {
		t.Fatalf("hits after close = %d, want 2", final.Hits)
	}
	if want := testNow.Add(35 * time.Second); !final.LastAccess.Equal(want) {
		t.Fatalf("last_access after close = %v, want %v", final.LastAccess, want)
	}
}

func TestMigrationsFreshAndRerun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.SchemaVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("schema version = %d, want 1", version)
	}
	for _, table := range []string{"meta", "blobs", "objects", "expectations", "pin_sets", "pin_items", "pin_rules"} {
		var name string
		if err := db.sql.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil {
			t.Fatalf("table %s missing: %v", table, err)
		}
	}
	var journal, foreign, busy, synchronous string
	if err := db.sql.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if err := db.sql.QueryRow(`PRAGMA foreign_keys`).Scan(&foreign); err != nil {
		t.Fatal(err)
	}
	if err := db.sql.QueryRow(`PRAGMA busy_timeout`).Scan(&busy); err != nil {
		t.Fatal(err)
	}
	if err := db.sql.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	if journal != "wal" || foreign != "1" || busy != "5000" || synchronous != "2" {
		t.Fatalf("pragmas = journal %q, foreign_keys %q, busy_timeout %q, synchronous %q", journal, foreign, busy, synchronous)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("index mode = %o, want 640", info.Mode().Perm())
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	version, err = reopened.SchemaVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("schema version after reopen = %d, want 1", version)
	}
}

func TestSchemaTooNew(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`UPDATE meta SET value = '999' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("Open error = %v, want ErrSchemaTooNew", err)
	}
}

func TestPutObjectGenerationsAndRefresh(t *testing.T) {
	db := openIndex(t)
	ctx := context.Background()
	blobA := seedBlob(t, db, "generation one", "identity", 14)
	blobB := seedBlob(t, db, "generation two", "gzip", 8)

	first := seedObject(t, db, "repo", "key", "mutable", blobA, testNow)
	if first.Generation != 1 || !first.Current {
		t.Fatalf("first object = %#v", first)
	}

	refreshed, err := db.PutObject(ctx, PutObjectParams{
		Repo: "repo", Key: "key", Class: "mutable", SHA256: blobA.SHA256,
		ContentType: "application/json", ETag: `"updated"`, Upstream: "mirror.example",
		FetchedAt: testNow.Add(time.Minute), ValidatedAt: testNow.Add(time.Minute), Verified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Generation != 1 || refreshed.ID != first.ID {
		t.Fatalf("same digest created a generation: %#v", refreshed)
	}
	if refreshed.ContentType != "application/json" || !refreshed.Verified {
		t.Fatalf("refresh did not update metadata: %#v", refreshed)
	}

	second, err := db.PutObject(ctx, PutObjectParams{
		Repo: "repo", Key: "key", Class: "mutable", SHA256: blobB.SHA256,
		FetchedAt: testNow.Add(2 * time.Minute), ValidatedAt: testNow.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Generation != 2 || !second.Current {
		t.Fatalf("second object = %#v", second)
	}
	old, err := db.CurrentObject(ctx, "repo", "key")
	if err != nil {
		t.Fatal(err)
	}
	if old.ID != second.ID {
		t.Fatal("current object was not switched")
	}
	generations, err := db.Generations(ctx, "repo", "key")
	if err != nil {
		t.Fatal(err)
	}
	if len(generations) != 2 || generations[0].ID != second.ID || generations[1].Current {
		t.Fatalf("generations = %#v", generations)
	}

	if err := db.TouchValidated(ctx, "repo", "key", testNow.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := db.SetVerified(ctx, "repo", "key", true); err != nil {
		t.Fatal(err)
	}
	current, err := db.CurrentObject(ctx, "repo", "key")
	if err != nil {
		t.Fatal(err)
	}
	if !current.Verified || !current.ValidatedAt.Equal(testNow.Add(3*time.Minute)) {
		t.Fatalf("touch did not update current: %#v", current)
	}

	deleted, err := db.DeleteKey(ctx, "repo", "key")
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	for _, sha := range []string{blobA.SHA256, blobB.SHA256} {
		refcount, err := db.BlobRefCount(ctx, sha)
		if err != nil {
			t.Fatal(err)
		}
		if refcount != 0 {
			t.Fatalf("refcount for %s = %d, want 0", sha, refcount)
		}
	}
}

func TestPutObjectRequiresBlob(t *testing.T) {
	db := openIndex(t)
	_, err := db.PutObject(context.Background(), PutObjectParams{
		Repo: "repo", Key: "key", Class: "immutable", SHA256: testSHA("missing"),
		FetchedAt: testNow, ValidatedAt: testNow,
	})
	if err == nil {
		t.Fatal("PutObject accepted a missing blob")
	}
}

func TestExpectations(t *testing.T) {
	db := openIndex(t)
	ctx := context.Background()
	blob := seedBlob(t, db, "expected", "identity", 8)
	_ = blob

	e := Expectation{
		Repo: "repo", Key: "file", Algo: "sha256", Digest: testSHA("expected"),
		Source: "index.json", CreatedAt: testNow,
	}
	if err := db.UpsertExpectation(ctx, e); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetExpectation(ctx, "repo", "file", "sha256")
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != e.Digest || got.Source != e.Source {
		t.Fatalf("expectation = %#v", got)
	}

	e.Digest = testSHA("replacement")
	e.Source = "next-index.json"
	e.CreatedAt = testNow.Add(time.Minute)
	if err := db.UpsertExpectation(ctx, e); err != nil {
		t.Fatal(err)
	}
	all, err := db.LookupExpectations(ctx, "repo", "file")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Digest != e.Digest || all[0].Source != e.Source {
		t.Fatalf("expectations = %#v", all)
	}
	if _, err := db.GetExpectation(ctx, "repo", "file", "sha1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing expectation error = %v", err)
	}
	if err := db.UpsertExpectation(ctx, Expectation{Repo: "repo", Key: "file", Algo: "crc", Digest: "00", Source: "x", CreatedAt: testNow}); err == nil {
		t.Fatal("invalid algorithm accepted")
	}
}

func TestPins(t *testing.T) {
	db := openIndex(t)
	ctx := context.Background()
	if err := db.CreatePinSet(ctx, PinSet{Name: "baseline", CreatedAt: testNow, Note: "keep"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreatePinSet(ctx, PinSet{Name: "baseline", CreatedAt: testNow}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate set error = %v", err)
	}
	if err := db.AddPinItem(ctx, PinItem{SetName: "baseline", Repo: "npm", Key: "typescript"}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddPinRule(ctx, PinRule{SetName: "baseline", Repo: "npm", Pattern: "react/*"}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddPinItem(ctx, PinItem{SetName: "baseline", Repo: "npm", Key: "typescript"}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate item error = %v", err)
	}
	if err := db.AddPinItem(ctx, PinItem{SetName: "missing", Repo: "npm", Key: "x"}); err == nil {
		t.Fatal("item accepted for missing set")
	}

	sets, err := db.ListPinSets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 || sets[0].Items != 1 || sets[0].Rules != 1 {
		t.Fatalf("pin sets = %#v", sets)
	}
	for _, tt := range []struct {
		key  string
		want bool
	}{
		{"typescript", true},
		{"react/dom", true},
		{"react", false},
		{"vue", false},
	} {
		got, err := db.Pinned(ctx, "npm", tt.key)
		if err != nil {
			t.Fatal(err)
		}
		if got != tt.want {
			t.Errorf("Pinned(%q) = %v, want %v", tt.key, got, tt.want)
		}
	}

	items, rules, err := db.PinSetMembers(ctx, "baseline")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || len(rules) != 1 {
		t.Fatalf("members = %#v %#v", items, rules)
	}
	if err := db.RemovePinItem(ctx, items[0]); err != nil {
		t.Fatal(err)
	}
	if err := db.RemovePinRule(ctx, rules[0]); err != nil {
		t.Fatal(err)
	}
	if err := db.RemovePinItem(ctx, items[0]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remove missing item error = %v", err)
	}
	if err := db.DeletePinSet(ctx, "baseline"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.PinSetMembers(ctx, "baseline"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing set error = %v", err)
	}
	if err := db.DeletePinSet(ctx, "baseline"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing set error = %v", err)
	}
}

func TestEvictionQueries(t *testing.T) {
	db := openIndex(t)
	shared := seedBlob(t, db, "shared blob", "identity", 11)
	mutable := seedBlob(t, db, "mutable blob", "gzip", 7)

	seedObject(t, db, "r1", "k1", "immutable", shared, testNow.Add(time.Minute))
	seedObject(t, db, "r1", "k2", "immutable", shared, testNow.Add(2*time.Minute))
	seedObject(t, db, "r1", "meta", "mutable", mutable, testNow.Add(3*time.Minute))
	seedObject(t, db, "r2", "k3", "immutable", shared, testNow)

	candidates, err := db.EvictionCandidates(context.Background(), "immutable", time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 3 {
		t.Fatalf("candidates = %d, want 3", len(candidates))
	}
	if candidates[0].Repo != "r2" || candidates[1].Key != "k1" || candidates[2].Key != "k2" {
		t.Fatalf("candidate order = %#v", candidates)
	}
	if candidates[0].StoredSize != 11 || candidates[0].StoredEnc != "identity" {
		t.Fatalf("candidate blob data = %#v", candidates[0])
	}

	cutoff, err := db.EvictionCandidates(context.Background(), "immutable", testNow.Add(2*time.Minute), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(cutoff) != 2 || cutoff[0].Repo != "r2" || cutoff[1].Key != "k1" {
		t.Fatalf("cutoff candidates = %#v", cutoff)
	}

	totals, err := db.RepoClassTotals(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(totals) != 3 {
		t.Fatalf("totals = %#v", totals)
	}
	if totals[0] != (RepoClassTotal{Repo: "r1", Class: "immutable", Blobs: 1, StoredBytes: 11}) {
		t.Fatalf("r1 immutable total = %#v", totals[0])
	}
	if totals[1] != (RepoClassTotal{Repo: "r1", Class: "mutable", Blobs: 1, StoredBytes: 7}) {
		t.Fatalf("r1 mutable total = %#v", totals[1])
	}
	if totals[2] != (RepoClassTotal{Repo: "r2", Class: "immutable", Blobs: 1, StoredBytes: 11}) {
		t.Fatalf("r2 immutable total = %#v", totals[2])
	}
}

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern string
		name    string
		want    bool
	}{
		{"typescript", "typescript", true},
		{"typescript", "vue", false},
		{"react/*", "react/dom", true},
		{"react/*", "react/dom/extra", false},
		{"react/**", "react/dom/extra", true},
		{"a/**/c", "a/c", true},
		{"a/**/c", "a/b/c", true},
		{"a/**/c", "a/b/d/c", true},
		{"file?.txt", "file1.txt", true},
		{"file?.txt", "file/.txt", false},
		{"[a-c]x", "bx", true},
		{"[!a-c]x", "dx", true},
		{"[!a-c]x", "bx", false},
		{"literal\\*", "literal*", true},
		{"literal\\*", "literalx", false},
	}
	for _, tt := range tests {
		if got := matchGlob(tt.pattern, tt.name); got != tt.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tt.pattern, tt.name, got, tt.want)
		}
	}
}

func FuzzMatchGlob(f *testing.F) {
	for _, seed := range [][2]string{{"", ""}, {"*", "a/b"}, {"**", "a/b"}, {"a/**/c", "a/b/c"}, {"[a-c]x", "bx"}} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, pattern, name string) {
		_ = matchGlob(pattern, name)
	})
}
