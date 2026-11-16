package store

import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreReaderMetadataAndSeek(t *testing.T) {
	s := newStore(t)
	raw := []byte("0123456789")
	blob, _ := writeBlob(t, s, raw, WriterOptions{})

	r, err := s.Open(blob.SHA256, EncodingIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if r.Size() != int64(len(raw)) || r.Encoding() != EncodingIdentity {
		t.Fatalf("reader metadata = size %d, encoding %q", r.Size(), r.Encoding())
	}
	if pos, err := r.Seek(2, io.SeekStart); err != nil || pos != 2 {
		t.Fatalf("Seek start = %d, %v", pos, err)
	}
	buf := make([]byte, 2)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "23" {
		t.Fatalf("read after seek = %q", buf)
	}
	if pos, err := r.Seek(0, io.SeekCurrent); err != nil || pos != 4 {
		t.Fatalf("Seek current = %d, %v", pos, err)
	}
	if pos, err := r.Seek(-1, io.SeekEnd); err != nil || pos != 9 {
		t.Fatalf("Seek end = %d, %v", pos, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("double close: %v", err)
	}
	if _, err := r.Read(buf); err == nil {
		t.Fatal("Read after close succeeded")
	}
	if _, err := r.Seek(0, io.SeekStart); err == nil {
		t.Fatal("Seek after close succeeded")
	}

	gz, _ := writeBlob(t, s, []byte("compressed content that is long enough to be compressed"), WriterOptions{Compress: true})
	r, err = s.Open(gz.SHA256, EncodingGzip)
	if err != nil {
		t.Fatal(err)
	}
	if r.Size() != -1 {
		t.Fatalf("gzip size = %d, want -1", r.Size())
	}
	if _, err := r.Seek(0, io.SeekEnd); err == nil {
		t.Fatal("SeekEnd on gzip succeeded")
	}
	if _, err := r.Seek(-1, io.SeekStart); err == nil {
		t.Fatal("negative gzip seek succeeded")
	}
	if pos, err := r.Seek(5, io.SeekStart); err != nil || pos != 5 {
		t.Fatalf("gzip seek = %d, %v", pos, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreSizeOnDisk(t *testing.T) {
	s := newStore(t)
	blob, _ := writeBlob(t, s, []byte("size"), WriterOptions{})
	size, err := s.SizeOnDisk(blob.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if size != blob.StoredSize {
		t.Fatalf("size = %d, want %d", size, blob.StoredSize)
	}
	if _, err := s.SizeOnDisk(sha256Hex([]byte("missing"))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing size error = %v", err)
	}
	if _, err := s.SizeOnDisk("bad"); !errors.Is(err, ErrInvalidDigest) {
		t.Fatalf("invalid digest error = %v", err)
	}
}

func TestStoreOpenErrors(t *testing.T) {
	s := newStore(t)
	if _, err := s.Open(sha256Hex([]byte("missing")), EncodingIdentity); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Open error = %v", err)
	}
	if _, err := s.OpenStored(sha256Hex([]byte("missing"))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing OpenStored error = %v", err)
	}
	blob, _ := writeBlob(t, s, []byte("encoding"), WriterOptions{})
	if _, err := s.Open(blob.SHA256, Encoding("brotli")); err == nil {
		t.Fatal("unknown encoding accepted")
	}
	if _, err := s.OpenRange(blob.SHA256, Encoding("brotli"), 0, 1); err == nil {
		t.Fatal("unknown range encoding accepted")
	}
	if _, err := Open(""); err == nil {
		t.Fatal("empty data directory accepted")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(file); err == nil {
		t.Fatal("file data directory accepted")
	}
}

func TestStoreQuarantineMissingAndExisting(t *testing.T) {
	s := newStore(t)
	blob, _ := writeBlob(t, s, []byte("quarantine me"), WriterOptions{})
	if err := os.WriteFile(filepath.Join(s.DataDir(), "quarantine", blob.SHA256), []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := s.Quarantine(blob.SHA256); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(blob.SHA256, EncodingIdentity); !errors.Is(err, ErrNotFound) {
		t.Fatalf("source still present: %v", err)
	}
	if err := s.Quarantine(sha256Hex([]byte("absent"))); err != nil {
		t.Fatalf("missing quarantine: %v", err)
	}
}

func TestStoreDigestAlgorithms(t *testing.T) {
	s := newStore(t)
	raw := []byte("all digests")
	blob, digests := writeBlob(t, s, raw, WriterOptions{ExtraDigests: []string{"sha512", "sha1", "md5"}})
	sha1sum := sha1.Sum(raw)
	md5sum := md5.Sum(raw)
	if digests["sha1"] != hex.EncodeToString(sha1sum[:]) {
		t.Errorf("sha1 = %s", digests["sha1"])
	}
	if digests["md5"] != hex.EncodeToString(md5sum[:]) {
		t.Errorf("md5 = %s", digests["md5"])
	}
	if blob.SHA256 != sha256Hex(raw) {
		t.Error("sha256 mismatch")
	}

	if _, err := s.NewWriter(WriterOptions{Expectations: []Expectation{{Algo: "sha256", Digest: "abc"}}}); err == nil {
		t.Fatal("short expectation accepted")
	}
	if _, err := s.NewWriter(WriterOptions{Expectations: []Expectation{{Algo: "sha256", Digest: string(bytes.Repeat([]byte("z"), 64))}}}); err == nil {
		t.Fatal("non-hex expectation accepted")
	}
	if _, err := s.NewWriter(WriterOptions{MaxBytes: -1}); err == nil {
		t.Fatal("negative max bytes accepted")
	}
}

func TestStoreWriterLifecycle(t *testing.T) {
	s := newStore(t)
	w, err := s.NewWriter(WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("more")); err == nil {
		t.Fatal("write after commit succeeded")
	}
	if _, _, err := w.Commit(); err == nil {
		t.Fatal("second commit succeeded")
	}
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}

	aborted, err := s.NewWriter(WriterOptions{Compress: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := aborted.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := aborted.Abort(); err != nil {
		t.Fatalf("double abort: %v", err)
	}
	if _, _, err := aborted.Commit(); err == nil {
		t.Fatal("commit after abort succeeded")
	}
}

func TestStoreOpenRangeEdges(t *testing.T) {
	s := newStore(t)
	raw := []byte("range")
	blob, _ := writeBlob(t, s, raw, WriterOptions{})
	if _, err := s.OpenRange(blob.SHA256, EncodingIdentity, 0, -1); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("negative length error = %v", err)
	}
	if _, err := s.OpenRange(blob.SHA256, EncodingIdentity, int64(len(raw))+1, 1); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("offset past end error = %v", err)
	}
	r, err := s.OpenRange(blob.SHA256, EncodingIdentity, int64(len(raw)), 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("zero range = %q", got)
	}
	if _, err := s.OpenRange(sha256Hex([]byte("missing")), EncodingIdentity, 0, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing range error = %v", err)
	}
}

func TestStoreSyncDirError(t *testing.T) {
	if err := syncDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("sync of missing directory succeeded")
	}
}

func TestStoreUsageEmpty(t *testing.T) {
	s := newStore(t)
	usage, err := s.Usage()
	if err != nil {
		t.Fatal(err)
	}
	if usage.Blobs != 0 || usage.StoredBytes != 0 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestStoreMaxBytesBoundary(t *testing.T) {
	s := newStore(t)
	w, err := s.NewWriter(WriterOptions{MaxBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreExpectedLength(t *testing.T) {
	s := newStore(t)
	three := int64(3)
	w, err := s.NewWriter(WriterOptions{ExpectedSize: &three})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.Commit(); err != nil {
		t.Fatal(err)
	}

	w, err = s.NewWriter(WriterOptions{ExpectedSize: &three})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("abcd")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.Commit(); !errors.Is(err, ErrLengthMismatch) {
		t.Fatalf("length mismatch error = %v", err)
	}
	assertTmpEmpty(t, s)
}

func TestOpenReadOnlyLayout(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	readOnly, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	if readOnly.DataDir() != dir {
		t.Fatalf("data dir = %q", readOnly.DataDir())
	}
	if _, err := readOnly.Usage(); err != nil {
		t.Fatal(err)
	}
	if err := readOnly.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing directory accepted")
	}
	if _, err := OpenReadOnly(""); err == nil {
		t.Fatal("empty directory accepted")
	}
}

func TestMatrix_F10_ReclaimWaitsForPublication(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	inside := make(chan struct{})
	release := make(chan struct{})
	published := make(chan struct{})
	go func() {
		defer close(published)
		_ = st.Publish(func() error {
			close(inside)
			<-release
			return nil
		})
	}()
	<-inside

	reclaimed := make(chan struct{})
	go func() {
		defer close(reclaimed)
		_ = st.Reclaim(func() error { return nil })
	}()
	select {
	case <-reclaimed:
		t.Fatal("reclaim ran while a blob was being published")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-published
	select {
	case <-reclaimed:
	case <-time.After(5 * time.Second):
		t.Fatal("reclaim did not run after publication finished")
	}
}
