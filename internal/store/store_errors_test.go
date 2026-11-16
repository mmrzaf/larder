package store

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreCommitMkdirFailure(t *testing.T) {
	s := newStore(t)
	blocker := filepath.Join(s.DataDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	s.blobsDir = blocker
	w, err := s.NewWriter(WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.Commit(); err == nil {
		t.Fatal("commit succeeded with a file in place of the blobs directory")
	}
	assertTmpEmpty(t, s)
}

func TestStoreCommitPathNotRegular(t *testing.T) {
	s := newStore(t)
	payload := []byte("directory target")
	sha := sha256Hex(payload)
	final := filepath.Join(s.DataDir(), "blobs", sha[:2], sha[2:4], sha)
	if err := os.MkdirAll(final, 0o750); err != nil {
		t.Fatal(err)
	}
	w, err := s.NewWriter(WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.Commit(); err == nil {
		t.Fatal("commit succeeded with a directory at the blob path")
	}
	assertTmpEmpty(t, s)
}

func TestStoreExistingIdentityEncoding(t *testing.T) {
	s := newStore(t)
	raw := []byte("identity twice")
	first, _ := writeBlob(t, s, raw, WriterOptions{})
	second, _ := writeBlob(t, s, raw, WriterOptions{})
	if first.SHA256 != second.SHA256 || second.StoredEnc != EncodingIdentity {
		t.Fatalf("second write = %#v", second)
	}
}

func TestStoreInvalidCompressedBlob(t *testing.T) {
	s := newStore(t)
	raw := []byte("invalid gzip")
	sha := sha256Hex(raw)
	path := filepath.Join(s.DataDir(), "blobs", sha[:2], sha[2:4], sha)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte{0x1f, 0x8b, 0x00}, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(sha, EncodingGzip); err == nil {
		t.Fatal("invalid gzip blob opened")
	}
	if _, err := s.OpenRange(sha, EncodingGzip, 0, 1); err == nil {
		t.Fatal("invalid gzip range opened")
	}
}

func TestStoreOpenWithLockDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "larder.lock"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("Open succeeded with a directory as the lock file")
	}
}

func TestStoreUsageError(t *testing.T) {
	s := newStore(t)
	if err := os.RemoveAll(filepath.Join(s.DataDir(), "blobs")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Usage(); err == nil {
		t.Fatal("Usage succeeded without the blob directory")
	}
}

func TestStoreFreeSpaceError(t *testing.T) {
	s := newStore(t)
	if err := os.RemoveAll(s.DataDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FreeSpace(); err == nil {
		t.Fatal("FreeSpace succeeded for a missing data directory")
	}
}

func TestStoreCommitAfterWriteFailure(t *testing.T) {
	s := newStore(t)
	w, err := s.NewWriter(WriterOptions{MaxBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("too large")); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("write error = %v", err)
	}
	if _, _, err := w.Commit(); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("commit error = %v", err)
	}
	assertTmpEmpty(t, s)
}

func TestStoreRangeReadClosers(t *testing.T) {
	s := newStore(t)
	blob, _ := writeBlob(t, s, []byte("range closer"), WriterOptions{Compress: true})
	r, err := s.OpenRange(blob.SHA256, EncodingGzip, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenRange(sha256Hex([]byte("missing")), EncodingGzip, 0, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing gzip range error = %v", err)
	}
}
