package store

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestMatrix_F18_CommitReplacesDamagedExistingBlob(t *testing.T) {
	for _, size := range []int64{0, 5} {
		t.Run(fmt.Sprintf("existing-%d-bytes", size), func(t *testing.T) {
			s := newStore(t)
			raw := bytes.Repeat([]byte("tarball bytes "), 64)
			sha := sha256Hex(raw)
			final, err := s.BlobPath(sha)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(final), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(final, raw[:size], 0o640); err != nil {
				t.Fatal(err)
			}

			blob, _ := writeBlob(t, s, raw, WriterOptions{})
			if blob.RawSize != int64(len(raw)) || blob.StoredSize != int64(len(raw)) {
				t.Fatalf("blob sizes = raw %d stored %d, want %d", blob.RawSize, blob.StoredSize, len(raw))
			}
			if blob.StoredEnc != EncodingIdentity {
				t.Fatalf("encoding = %q, want identity", blob.StoredEnc)
			}
			got, err := os.ReadFile(final)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, raw) {
				t.Fatalf("stored file has %d bytes, want %d", len(got), len(raw))
			}
		})
	}
}

func TestMatrix_F18_CommitReplacesDamagedCompressedBlob(t *testing.T) {
	s := newStore(t)
	raw := bytes.Repeat([]byte("repetitive metadata document "), 32)
	first, _ := writeBlob(t, s, raw, WriterOptions{Compress: true})
	final, err := s.BlobPath(first.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(final, 5); err != nil {
		t.Fatal(err)
	}

	blob, _ := writeBlob(t, s, raw, WriterOptions{Compress: true})
	if blob.StoredEnc != EncodingGzip {
		t.Fatalf("encoding = %q, want gzip", blob.StoredEnc)
	}
	if blob.RawSize != int64(len(raw)) || blob.StoredSize <= 0 {
		t.Fatalf("blob sizes = raw %d stored %d", blob.RawSize, blob.StoredSize)
	}
	reader, err := s.Open(blob.SHA256, EncodingGzip)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatal("replaced gzip blob does not decode to the raw bytes")
	}
}

func TestMatrix_F18_CommitRejectsEmptyBody(t *testing.T) {
	s := newStore(t)
	w, err := s.NewWriter(WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.Commit(); !errors.Is(err, ErrEmpty) {
		t.Fatalf("commit empty error = %v, want ErrEmpty", err)
	}
	usage, err := s.Usage()
	if err != nil {
		t.Fatal(err)
	}
	if usage.Blobs != 0 {
		t.Fatalf("blobs after an empty commit = %d, want 0", usage.Blobs)
	}
	assertTmpEmpty(t, s)
}
