package index

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSystemClock(t *testing.T) {
	clock := systemClock{}
	if clock.Now().IsZero() {
		t.Fatal("system clock returned zero time")
	}
	select {
	case <-clock.After(0):
	case <-time.After(time.Second):
		t.Fatal("system clock timer did not fire")
	}
}

func TestAccessRecorderDefaultsAndFlushError(t *testing.T) {
	db := openIndex(t)
	recorder := NewAccessRecorder(db, nil, 0, nil)
	recorder.Record(0)
	recorder.Record(1)
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}

	recorder = NewAccessRecorder(db, nil, time.Second, nil)
	recorder.Record(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := recorder.Flush(ctx); err == nil {
		t.Fatal("flush with a canceled context succeeded")
	}
	recorder.mu.Lock()
	pending := len(recorder.pending)
	recorder.mu.Unlock()
	if pending != 1 {
		t.Fatalf("pending after failed flush = %d, want 1", pending)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBlobValidationAndMissing(t *testing.T) {
	db := openIndex(t)
	ctx := context.Background()
	for _, blob := range []Blob{
		{SHA256: "bad", RawSize: 1, StoredSize: 1, CreatedAt: testNow},
		{SHA256: testSHA("x"), RawSize: -1, StoredSize: 1, CreatedAt: testNow},
		{SHA256: testSHA("x"), RawSize: 1, StoredSize: 1, StoredEnc: "brotli", CreatedAt: testNow},
		{SHA256: testSHA("x"), RawSize: 1, StoredSize: 1},
	} {
		if err := db.PutBlob(ctx, blob); err == nil {
			t.Errorf("PutBlob(%#v) succeeded", blob)
		}
	}
	if _, err := db.GetBlob(ctx, testSHA("absent")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing blob error = %v", err)
	}
	if _, err := db.BlobRefCount(ctx, testSHA("absent")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing refcount error = %v", err)
	}
	blob := seedBlob(t, db, "duplicate", "identity", 9)
	if err := db.PutBlob(ctx, blob); err != nil {
		t.Fatalf("duplicate blob: %v", err)
	}
}

func TestObjectValidationAndMissing(t *testing.T) {
	db := openIndex(t)
	ctx := context.Background()
	blob := seedBlob(t, db, "object", "identity", 6)

	for _, p := range []PutObjectParams{
		{Repo: "", Key: "k", Class: "immutable", SHA256: blob.SHA256, FetchedAt: testNow, ValidatedAt: testNow},
		{Repo: "r", Key: "", Class: "immutable", SHA256: blob.SHA256, FetchedAt: testNow, ValidatedAt: testNow},
		{Repo: "r", Key: "k", Class: "deny", SHA256: blob.SHA256, FetchedAt: testNow, ValidatedAt: testNow},
		{Repo: "r", Key: "k", Class: "immutable", SHA256: "bad", FetchedAt: testNow, ValidatedAt: testNow},
		{Repo: "r", Key: "k", Class: "immutable", SHA256: blob.SHA256},
	} {
		if _, err := db.PutObject(ctx, p); err == nil {
			t.Errorf("PutObject(%#v) succeeded", p)
		}
	}
	if err := db.DeleteObject(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing delete error = %v", err)
	}
	if err := db.TouchValidated(ctx, "r", "missing", testNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing touch error = %v", err)
	}
	if err := db.SetVerified(ctx, "r", "missing", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing set verified error = %v", err)
	}
	if n, err := db.DeleteKey(ctx, "r", "missing"); err != nil || n != 0 {
		t.Fatalf("missing DeleteKey = %d, %v", n, err)
	}
	if generations, err := db.Generations(ctx, "r", "missing"); err != nil || len(generations) != 0 {
		t.Fatalf("missing generations = %#v, %v", generations, err)
	}
	if _, err := db.EvictionCandidates(ctx, "deny", time.Time{}, 0); err == nil {
		t.Fatal("invalid eviction class accepted")
	}
}

func TestPinValidation(t *testing.T) {
	db := openIndex(t)
	ctx := context.Background()
	if err := db.CreatePinSet(ctx, PinSet{}); err == nil {
		t.Fatal("empty pin set accepted")
	}
	if err := db.CreatePinSet(ctx, PinSet{Name: "x"}); err == nil {
		t.Fatal("pin set without created_at accepted")
	}
	if err := db.DeletePinSet(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing delete set error = %v", err)
	}
	if err := db.AddPinItem(ctx, PinItem{}); err == nil {
		t.Fatal("empty pin item accepted")
	}
	if err := db.AddPinRule(ctx, PinRule{}); err == nil {
		t.Fatal("empty pin rule accepted")
	}
	if err := db.RemovePinRule(ctx, PinRule{SetName: "x", Repo: "r", Pattern: "p"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing remove rule error = %v", err)
	}
	if err := db.CreatePinSet(ctx, PinSet{Name: "x", CreatedAt: testNow}); err != nil {
		t.Fatal(err)
	}
	rule := PinRule{SetName: "x", Repo: "r", Pattern: "a*"}
	if err := db.AddPinRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	if err := db.AddPinRule(ctx, rule); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate rule error = %v", err)
	}
	if err := db.RemovePinRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
}

func TestExpectationValidation(t *testing.T) {
	db := openIndex(t)
	ctx := context.Background()
	for _, e := range []Expectation{
		{Repo: "", Key: "k", Algo: "sha256", Digest: testSHA("x"), Source: "s", CreatedAt: testNow},
		{Repo: "r", Key: "", Algo: "sha256", Digest: testSHA("x"), Source: "s", CreatedAt: testNow},
		{Repo: "r", Key: "k", Algo: "crc", Digest: "00", Source: "s", CreatedAt: testNow},
		{Repo: "r", Key: "k", Algo: "sha256", Digest: "zz", Source: "s", CreatedAt: testNow},
		{Repo: "r", Key: "k", Algo: "sha256", Digest: testSHA("x"), Source: "", CreatedAt: testNow},
		{Repo: "r", Key: "k", Algo: "sha256", Digest: testSHA("x"), Source: "s"},
	} {
		if err := db.UpsertExpectation(ctx, e); err == nil {
			t.Errorf("UpsertExpectation(%#v) succeeded", e)
		}
	}
}

func TestGlobEdgeCases(t *testing.T) {
	tests := []struct {
		pattern string
		name    string
		want    bool
	}{
		{"", "", true},
		{"", "x", false},
		{"[]", "]", false},
		{"[!]", "x", false},
		{"[a-]", "-", true},
		{"a\\", "a\\", true},
		{"[a-c", "[a-c", true},
		{"**/a", "a", true},
		{"**/a", "x/a", true},
	}
	for _, tt := range tests {
		if got := matchGlob(tt.pattern, tt.name); got != tt.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tt.pattern, tt.name, got, tt.want)
		}
	}
}

func TestMigrationInvalidVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`UPDATE meta SET value = 'not-a-number' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "parse schema version") {
		t.Fatalf("Open error = %v", err)
	}
}

func TestBatchExpectationsAndMarkStale(t *testing.T) {
	db := openIndex(t)
	ctx := context.Background()
	blob := seedBlob(t, db, "metadata", "identity", 8)
	seedObject(t, db, "repo", "a/current", "mutable", blob, testNow)
	seedObject(t, db, "repo", "b/current", "mutable", blob, testNow)
	seedObject(t, db, "repo", "a/immutable", "immutable", blob, testNow)

	batch := []Expectation{
		{Repo: "repo", Key: "one", Algo: "sha256", Digest: testSHA("one"), Source: "doc", CreatedAt: testNow},
		{Repo: "repo", Key: "two", Algo: "sha1", Digest: strings.Repeat("ab", 20), Source: "doc", CreatedAt: testNow},
	}
	if err := db.UpsertExpectations(ctx, batch); err != nil {
		t.Fatal(err)
	}
	for _, e := range batch {
		if _, err := db.GetExpectation(ctx, e.Repo, e.Key, e.Algo); err != nil {
			t.Fatalf("batch expectation missing: %v", err)
		}
	}
	bad := []Expectation{{Repo: "repo", Key: "bad", Algo: "crc", Digest: "00", Source: "doc", CreatedAt: testNow}}
	if err := db.UpsertExpectations(ctx, bad); err == nil {
		t.Fatal("invalid batch accepted")
	}
	if _, err := db.GetExpectation(ctx, "repo", "bad", "crc"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invalid batch partially applied: %v", err)
	}

	updated, err := db.MarkStale(ctx, "repo", "a/*")
	if err != nil {
		t.Fatal(err)
	}
	if updated != 1 {
		t.Fatalf("updated = %d, want 1", updated)
	}
	object, err := db.CurrentObject(ctx, "repo", "a/current")
	if err != nil {
		t.Fatal(err)
	}
	if !object.ValidatedAt.IsZero() {
		t.Fatalf("mutable object was not marked stale: %v", object.ValidatedAt)
	}
	immutable, err := db.CurrentObject(ctx, "repo", "a/immutable")
	if err != nil {
		t.Fatal(err)
	}
	if !immutable.ValidatedAt.Equal(testNow) {
		t.Fatal("immutable object was marked stale")
	}
	if _, err := db.MarkStale(ctx, "", "x"); err == nil {
		t.Fatal("empty repo accepted")
	}
}

func TestPingAndQuickCheck(t *testing.T) {
	db := openIndex(t)
	ctx := context.Background()
	if err := db.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.QuickCheck(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestListUnverifiedAndH1Expectations(t *testing.T) {
	db := openIndex(t)
	ctx := context.Background()
	blob := seedBlob(t, db, "unverified", "identity", 10)
	immutable := seedObject(t, db, "go", "example.com/hello/@v/v1.0.0.zip", "immutable", blob, testNow)
	seedObject(t, db, "go", "example.com/hello/@v/list", "mutable", blob, testNow)
	verified := seedObject(t, db, "go", "example.com/hello/@v/v1.0.0.mod", "immutable", blob, testNow)
	if err := db.SetVerified(ctx, "go", verified.Key, true); err != nil {
		t.Fatal(err)
	}

	objects, err := db.ListUnverified(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 1 || objects[0].ID != immutable.ID {
		t.Fatalf("unverified objects = %#v", objects)
	}
	if _, err := db.ListUnverified(ctx, 0); err != nil {
		t.Fatal(err)
	}

	h1 := Expectation{Repo: "go", Key: "example.com/hello/@v/v1.0.0.zip", Algo: "h1", Digest: "47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=", Source: "sumdb", CreatedAt: testNow}
	if err := db.UpsertExpectation(ctx, h1); err != nil {
		t.Fatal(err)
	}
	stored, err := db.GetExpectation(ctx, h1.Repo, h1.Key, "h1")
	if err != nil || stored.Digest != h1.Digest {
		t.Fatalf("h1 expectation = %#v, %v", stored, err)
	}
	for _, digest := range []string{"", "not base64!"} {
		bad := h1
		bad.Digest = digest
		if err := db.UpsertExpectation(ctx, bad); err == nil {
			t.Fatalf("invalid h1 digest %q accepted", digest)
		}
	}
}

func TestBatchExpectationPerformance(t *testing.T) {
	db := openIndex(t)
	ctx := context.Background()
	rows := make([]Expectation, 0, 100000)
	digest := strings.Repeat("ab", 32)
	for i := 0; i < 100000; i++ {
		rows = append(rows, Expectation{
			Repo: "apt", Key: fmt.Sprintf("/pool/demo-%d.deb", i), Algo: "sha256",
			Digest: digest, Source: "Packages", CreatedAt: testNow,
		})
	}
	start := time.Now()
	if err := db.UpsertExpectations(ctx, rows); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 120*time.Second {
		t.Fatalf("100000 expectation inserts took %s", elapsed)
	}
	expectation, err := db.GetExpectation(ctx, "apt", "/pool/demo-99999.deb", "sha256")
	if err != nil || expectation.Digest != digest {
		t.Fatalf("last expectation = %#v, %v", expectation, err)
	}
}
