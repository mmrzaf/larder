package store

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/mmrzaf/larder/testutil"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(testutil.TempDataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func writeBlob(t *testing.T, s *Store, data []byte, opts WriterOptions) (Blob, map[string]string) {
	t.Helper()
	w, err := s.NewWriter(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	blob, digests, err := w.Commit()
	if err != nil {
		t.Fatal(err)
	}
	return blob, digests
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestMatrix_C27_GzipAtRest(t *testing.T) {
	s := newStore(t)
	raw := bytes.Repeat([]byte(`{"name":"larder","version":"1.0.0","description":"repetitive metadata"},`), 50)
	blob, digests := writeBlob(t, s, raw, WriterOptions{Compress: true, ExtraDigests: []string{"sha512"}})

	if blob.SHA256 != sha256Hex(raw) {
		t.Fatalf("sha256 = %s, want %s", blob.SHA256, sha256Hex(raw))
	}
	if blob.StoredEnc != EncodingGzip {
		t.Fatalf("stored encoding = %q, want gzip", blob.StoredEnc)
	}
	if blob.StoredSize >= blob.RawSize {
		t.Fatalf("stored size %d is not smaller than raw size %d", blob.StoredSize, blob.RawSize)
	}
	sum := sha512.Sum512(raw)
	if digests["sha512"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha512 digest = %s", digests["sha512"])
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
		t.Fatal("decompressed blob differs from the raw bytes")
	}

	stored, err := s.OpenStored(blob.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	storedBytes, err := io.ReadAll(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := stored.Close(); err != nil {
		t.Fatal(err)
	}
	if len(storedBytes) < 2 || storedBytes[0] != 0x1f || storedBytes[1] != 0x8b {
		t.Fatal("stored bytes are not gzip")
	}
	gz, err := gzip.NewReader(bytes.NewReader(storedBytes))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, raw) {
		t.Fatal("stored gzip bytes do not decode to the raw bytes")
	}

	rangeReader, err := s.OpenRange(blob.SHA256, EncodingGzip, 10, 20)
	if err != nil {
		t.Fatal(err)
	}
	rangeBytes, err := io.ReadAll(rangeReader)
	if err != nil {
		t.Fatal(err)
	}
	if err := rangeReader.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rangeBytes, raw[10:30]) {
		t.Fatalf("range = %q, want %q", rangeBytes, raw[10:30])
	}
}

func TestMatrix_C29_CommitFailure(t *testing.T) {
	payload := []byte("payload that must never be visible in a partial state")
	sha := sha256Hex(payload)
	steps := []string{"afterOpen", "afterWrite", "afterSync", "afterMkdir", "beforeRename", "afterRename", "afterDirSync"}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			s := newStore(t)
			injected := errors.New("injected failure")
			hook := func() error { return injected }
			s.hooks = &commitHooks{}
			switch step {
			case "afterOpen":
				s.hooks.afterOpen = hook
			case "afterWrite":
				s.hooks.afterWrite = hook
			case "afterSync":
				s.hooks.afterSync = hook
			case "afterMkdir":
				s.hooks.afterMkdir = hook
			case "beforeRename":
				s.hooks.beforeRename = hook
			case "afterRename":
				s.hooks.afterRename = hook
			case "afterDirSync":
				s.hooks.afterDirSync = hook
			}

			w, err := s.NewWriter(WriterOptions{})
			if step == "afterOpen" {
				if err == nil {
					t.Fatal("NewWriter succeeded with an injected failure")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := w.Write(payload); err != nil {
					t.Fatal(err)
				}
				if _, _, err := w.Commit(); err == nil {
					t.Fatal("Commit succeeded with an injected failure")
				}
			}

			final := filepath.Join(s.DataDir(), "blobs", sha[:2], sha[2:4], sha)
			visible := step == "afterRename" || step == "afterDirSync"
			data, readErr := os.ReadFile(final)
			if visible {
				if readErr != nil {
					t.Fatalf("complete blob is missing after %s: %v", step, readErr)
				}
				if !bytes.Equal(data, payload) {
					t.Fatalf("visible blob is partial after %s", step)
				}
			} else if readErr == nil {
				t.Fatalf("partial or unexpected blob is visible after %s", step)
			}

			assertTmpEmpty(t, s)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(s.DataDir())
			if err != nil {
				t.Fatal(err)
			}
			assertTmpEmpty(t, reopened)
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMatrix_C29_StartupCleanup(t *testing.T) {
	dir := testutil.TempDataDir(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tmp", "left.part"), []byte("partial"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "tmp", "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tmp", "sub", "nested.part"), []byte("partial"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertTmpEmpty(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertTmpEmpty(t *testing.T, s *Store) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(s.DataDir(), "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("tmp directory is not empty: %v", entries)
	}
}

func TestStoreLockExclusive(t *testing.T) {
	dir := testutil.TempDataDir(t)
	first, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "larder.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("lock mode = %o, want 640", info.Mode().Perm())
	}
	if _, err := Open(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open error = %v, want ErrLocked", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(dir)
	if err != nil {
		t.Fatalf("Open after close: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRangeReads(t *testing.T) {
	raw := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	for _, enc := range []Encoding{EncodingIdentity, EncodingGzip} {
		t.Run(string(enc), func(t *testing.T) {
			s := newStore(t)
			blob, _ := writeBlob(t, s, raw, WriterOptions{Compress: enc == EncodingGzip})
			if blob.StoredEnc != enc {
				t.Fatalf("encoding = %q, want %q", blob.StoredEnc, enc)
			}

			r, err := s.OpenRange(blob.SHA256, enc, 5, 10)
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
			if string(got) != "56789abcde" {
				t.Fatalf("%s range = %q", enc, got)
			}

			r, err = s.OpenRange(blob.SHA256, enc, 30, 100)
			if err != nil {
				t.Fatal(err)
			}
			got, err = io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if string(got) != "uvwxyz" {
				t.Fatalf("%s clamped range = %q", enc, got)
			}
		})
	}
	s := newStore(t)
	if _, err := s.OpenRange(sha256Hex(raw), EncodingIdentity, -1, 1); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("negative offset error = %v", err)
	}
}

func TestStoreUsageDeleteQuarantine(t *testing.T) {
	s := newStore(t)
	a, _ := writeBlob(t, s, []byte("alpha"), WriterOptions{})
	b, _ := writeBlob(t, s, []byte("bravo"), WriterOptions{Compress: true})

	usage, err := s.Usage()
	if err != nil {
		t.Fatal(err)
	}
	if usage.Blobs != 2 || usage.StoredBytes == 0 {
		t.Fatalf("usage = %#v", usage)
	}
	if err := s.Delete(a.SHA256); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(a.SHA256); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
	if _, err := s.Open(a.SHA256, EncodingIdentity); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open deleted blob error = %v", err)
	}

	if err := s.Quarantine(b.SHA256); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(b.SHA256, EncodingGzip); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open quarantined blob error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.DataDir(), "quarantine", b.SHA256)); err != nil {
		t.Fatalf("quarantined file missing: %v", err)
	}
	if err := s.Quarantine(b.SHA256); err != nil {
		t.Fatalf("idempotent quarantine: %v", err)
	}
}

func TestStoreExpectations(t *testing.T) {
	s := newStore(t)
	raw := []byte("verified bytes")
	sha := sha256Hex(raw)
	sha512sum := sha512.Sum512(raw)
	w, err := s.NewWriter(WriterOptions{
		Expectations: []Expectation{
			{Algo: "sha256", Digest: sha},
			{Algo: "sha512", Digest: hex.EncodeToString(sha512sum[:])},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.Commit(); err != nil {
		t.Fatal(err)
	}

	w, err = s.NewWriter(WriterOptions{Expectations: []Expectation{{Algo: "sha256", Digest: sha256Hex([]byte("other"))}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.Commit(); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("commit error = %v, want ErrDigestMismatch", err)
	}
	assertTmpEmpty(t, s)

	if _, err := s.NewWriter(WriterOptions{ExtraDigests: []string{"crc32"}}); err == nil {
		t.Fatal("unsupported digest algorithm accepted")
	}
}

func TestStoreMaxBytesAndAbort(t *testing.T) {
	s := newStore(t)
	w, err := s.NewWriter(WriterOptions{MaxBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("four")); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("write error = %v, want ErrTooLarge", err)
	}
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
	assertTmpEmpty(t, s)
}

func TestStoreRejectsInvalidDigests(t *testing.T) {
	s := newStore(t)
	invalid := []string{"", "../escape", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "abc"}
	for _, sha := range invalid {
		if _, err := s.Open(sha, EncodingIdentity); !errors.Is(err, ErrInvalidDigest) {
			t.Errorf("Open(%q) error = %v", sha, err)
		}
		if err := s.Delete(sha); !errors.Is(err, ErrInvalidDigest) {
			t.Errorf("Delete(%q) error = %v", sha, err)
		}
		if err := s.Quarantine(sha); !errors.Is(err, ErrInvalidDigest) {
			t.Errorf("Quarantine(%q) error = %v", sha, err)
		}
	}
}

func TestStoreDuplicateBlob(t *testing.T) {
	s := newStore(t)
	raw := []byte("same bytes")
	a, _ := writeBlob(t, s, raw, WriterOptions{Compress: true})
	b, _ := writeBlob(t, s, raw, WriterOptions{})
	if a.SHA256 != b.SHA256 {
		t.Fatalf("sha differ: %s %s", a.SHA256, b.SHA256)
	}
	if b.StoredEnc != EncodingGzip {
		t.Fatalf("existing encoding = %q, want gzip", b.StoredEnc)
	}
	usage, err := s.Usage()
	if err != nil {
		t.Fatal(err)
	}
	if usage.Blobs != 1 {
		t.Fatalf("blob count = %d, want 1", usage.Blobs)
	}
}

func TestStoreFreeSpace(t *testing.T) {
	s := newStore(t)
	free, err := s.FreeSpace()
	if err != nil {
		t.Fatal(err)
	}
	if free == 0 {
		t.Fatal("free space is zero")
	}
	capacity, err := s.Capacity()
	if err != nil {
		t.Fatal(err)
	}
	if capacity < free {
		t.Fatalf("capacity %d is smaller than free %d", capacity, free)
	}
}

func TestStoreFileModes(t *testing.T) {
	s := newStore(t)
	blob, _ := writeBlob(t, s, []byte("mode"), WriterOptions{})
	path := filepath.Join(s.DataDir(), "blobs", blob.SHA256[:2], blob.SHA256[2:4], blob.SHA256)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("blob mode = %o, want 640", got)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o750 {
		t.Fatalf("blob directory mode = %o, want 750", got)
	}
}

func FuzzBlobPath(f *testing.F) {
	for _, seed := range []string{"", "../x", "abc", sha256Hex([]byte("x"))} {
		f.Add(seed)
	}
	s := &Store{blobsDir: "/tmp/blobs"}
	f.Fuzz(func(t *testing.T, sha string) {
		path, err := s.blobPath(sha)
		if err == nil && !filepath.IsAbs(path) {
			t.Fatalf("path %q is not absolute", path)
		}
	})
}
