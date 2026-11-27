package index

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestMatrix_F19_PublishIsAtomic(t *testing.T) {
	db := openIndex(t)
	ctx := context.Background()
	blob := Blob{
		SHA256: testSHA("atomic publish"), RawSize: int64(len("atomic publish")),
		StoredSize: int64(len("atomic publish")), StoredEnc: "identity", CreatedAt: testNow,
	}
	if _, err := db.sql.Exec(
		`CREATE TRIGGER fail_objects BEFORE INSERT ON objects
		 BEGIN SELECT RAISE(ABORT, 'publish failed'); END`); err != nil {
		t.Fatal(err)
	}
	_, err := db.PublishObject(ctx, blob, PutObjectParams{
		Repo: "r1", Key: "/k1", Class: "immutable", SHA256: blob.SHA256,
		FetchedAt: testNow, ValidatedAt: testNow,
	})
	if err == nil {
		t.Fatal("publish succeeded despite the trigger")
	}
	if count := blobRows(t, db, blob.SHA256); count != 0 {
		t.Fatalf("blob rows after failed publish = %d, want 0", count)
	}
	if count := objectRows(t, db); count != 0 {
		t.Fatalf("object rows after failed publish = %d, want 0", count)
	}

	if _, err := db.sql.Exec(`DROP TRIGGER fail_objects`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PublishObject(ctx, blob, PutObjectParams{
		Repo: "r1", Key: "/k1", Class: "immutable", SHA256: blob.SHA256,
		FetchedAt: testNow, ValidatedAt: testNow,
	}); err != nil {
		t.Fatal(err)
	}
	if count := blobRows(t, db, blob.SHA256); count != 1 {
		t.Fatalf("blob rows after publish = %d, want 1", count)
	}
	if count := objectRows(t, db); count != 1 {
		t.Fatalf("object rows after publish = %d, want 1", count)
	}
}

func TestPublishObjectRejectsEmptyBlob(t *testing.T) {
	db := openIndex(t)
	ctx := context.Background()
	blob := Blob{
		SHA256: testSHA("damaged"), RawSize: 7, StoredSize: 0,
		StoredEnc: "identity", CreatedAt: testNow,
	}
	_, err := db.PublishObject(ctx, blob, PutObjectParams{
		Repo: "r1", Key: "/k1", Class: "immutable", SHA256: blob.SHA256,
		FetchedAt: testNow, ValidatedAt: testNow,
	})
	if err == nil {
		t.Fatal("publish accepted a non-empty blob with no stored bytes")
	}
	if count := blobRows(t, db, blob.SHA256); count != 0 {
		t.Fatalf("blob rows = %d, want 0", count)
	}
}

// TestMatrix_S21_PublishRetriesBusyLock holds the write lock from a second
// connection and checks that the writer waits, retries and succeeds.
func TestMatrix_S21_PublishRetriesBusyLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	retried := make(chan struct{}, 1)
	db, err := open(path, openOptions{
		busyTimeout: 20 * time.Millisecond,
		attempts:    8,
		sleep: func(ctx context.Context, d time.Duration) error {
			select {
			case retried <- struct{}{}:
			default:
			}
			return sleepContext(ctx, d)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	holder, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(1000)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	holder.SetMaxOpenConns(1)
	ctx := context.Background()
	conn, err := holder.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}

	blob := Blob{
		SHA256: testSHA("busy"), RawSize: 4, StoredSize: 4,
		StoredEnc: "identity", CreatedAt: testNow,
	}
	done := make(chan error, 1)
	go func() {
		done <- db.PutBlob(ctx, blob)
	}()

	select {
	case <-retried:
	case err := <-done:
		_ = conn.Close()
		t.Fatalf("publish finished before the first busy retry: %v", err)
	case <-time.After(5 * time.Second):
		_ = conn.Close()
		t.Fatal("publish never reached a busy retry")
	}
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("publish after lock release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("publish did not finish after the lock was released")
	}
	if count := blobRows(t, db, blob.SHA256); count != 1 {
		t.Fatalf("blob rows = %d, want 1", count)
	}
}

func blobRows(t *testing.T, db *DB, sha string) int {
	t.Helper()
	var count int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM blobs WHERE sha256 = ?`, sha).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func objectRows(t *testing.T, db *DB) int {
	t.Helper()
	var count int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM objects`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
