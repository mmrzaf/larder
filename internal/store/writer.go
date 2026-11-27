package store

import (
	"compress/gzip"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type commitHooks struct {
	afterOpen    func() error
	afterWrite   func() error
	afterSync    func() error
	afterMkdir   func() error
	beforeRename func() error
	afterRename  func() error
	afterDirSync func() error
}

// Expectation is a digest that a writer must satisfy before committing.
type Expectation struct {
	Algo   string
	Digest string
}

// WriterOptions controls how a blob is written.
type WriterOptions struct {
	Compress     bool
	ExtraDigests []string
	Expectations []Expectation
	MaxBytes     int64
	ExpectedSize *int64
}

// Writer streams bytes into a temporary file and commits them atomically.
type Writer struct {
	store *Store
	file  *os.File
	path  string
	gzip  *gzip.Writer
	sink  io.Writer
	hash  map[string]hash.Hash

	opts      WriterOptions
	rawSize   int64
	closed    bool
	failed    error
	committed bool
}

// NewWriter creates a temporary file in the store's tmp directory.
func (s *Store) NewWriter(opts WriterOptions) (*Writer, error) {
	if opts.MaxBytes < 0 {
		return nil, fmt.Errorf("writer: max bytes must not be negative")
	}
	algorithms := map[string]bool{"sha256": true}
	for _, algo := range opts.ExtraDigests {
		algo = strings.ToLower(algo)
		if _, err := newHasher(algo); err != nil {
			return nil, err
		}
		algorithms[algo] = true
	}
	for _, e := range opts.Expectations {
		algo := strings.ToLower(e.Algo)
		h, err := newHasher(algo)
		if err != nil {
			return nil, err
		}
		if len(e.Digest) != hex.EncodedLen(h.Size()) {
			return nil, fmt.Errorf("expectation %s: digest must be %d hex characters", algo, hex.EncodedLen(h.Size()))
		}
		if _, err := hex.DecodeString(e.Digest); err != nil {
			return nil, fmt.Errorf("expectation %s: digest is not hexadecimal", algo)
		}
		algorithms[algo] = true
	}

	file, err := os.CreateTemp(s.tmpDir, "*.part")
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
	}
	if err := file.Chmod(0o640); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, fmt.Errorf("set temp file mode: %w", err)
	}

	w := &Writer{
		store: s,
		file:  file,
		path:  file.Name(),
		hash:  make(map[string]hash.Hash, len(algorithms)),
		opts:  opts,
	}
	for algo := range algorithms {
		h, _ := newHasher(algo)
		w.hash[algo] = h
	}
	writers := make([]io.Writer, 0, len(w.hash))
	for _, h := range w.hash {
		writers = append(writers, h)
	}
	rawSink := io.MultiWriter(writers...)
	if opts.Compress {
		w.gzip = gzip.NewWriter(file)
		w.sink = io.MultiWriter(w.gzip, rawSink)
	} else {
		w.sink = io.MultiWriter(file, rawSink)
	}

	if err := s.runHook(s.hooks, func(h *commitHooks) func() error { return h.afterOpen }); err != nil {
		_ = w.Abort()
		return nil, err
	}
	return w, nil
}

// Write appends raw bytes to the blob.
func (w *Writer) Write(p []byte) (int, error) {
	if w.closed || w.committed {
		return 0, fmt.Errorf("write blob: writer is closed")
	}
	if w.failed != nil {
		return 0, w.failed
	}
	if w.opts.MaxBytes > 0 && w.rawSize+int64(len(p)) > w.opts.MaxBytes {
		w.failed = fmt.Errorf("%w: limit %d", ErrTooLarge, w.opts.MaxBytes)
		return 0, w.failed
	}
	n, err := w.sink.Write(p)
	w.rawSize += int64(n)
	if err != nil {
		w.failed = fmt.Errorf("write temp file: %w", err)
	}
	return n, err
}

// Commit finishes the file, verifies expectations and moves it into place.
func (w *Writer) Commit() (Blob, map[string]string, error) {
	if w.committed {
		return Blob{}, nil, fmt.Errorf("commit blob: already committed")
	}
	if w.failed != nil {
		_ = w.Abort()
		return Blob{}, nil, w.failed
	}
	if w.closed {
		return Blob{}, nil, fmt.Errorf("commit blob: writer is closed")
	}

	if err := w.store.runHook(w.store.hooks, func(h *commitHooks) func() error { return h.afterWrite }); err != nil {
		_ = w.Abort()
		return Blob{}, nil, err
	}
	if w.gzip != nil {
		if err := w.gzip.Close(); err != nil {
			_ = w.Abort()
			return Blob{}, nil, fmt.Errorf("flush compressed blob: %w", err)
		}
		w.gzip = nil
	}
	if err := w.file.Sync(); err != nil {
		_ = w.Abort()
		return Blob{}, nil, fmt.Errorf("sync temp file: %w", err)
	}
	if err := w.store.runHook(w.store.hooks, func(h *commitHooks) func() error { return h.afterSync }); err != nil {
		_ = w.Abort()
		return Blob{}, nil, err
	}
	info, err := w.file.Stat()
	if err != nil {
		_ = w.Abort()
		return Blob{}, nil, fmt.Errorf("stat temp file: %w", err)
	}
	if err := w.file.Close(); err != nil {
		w.closed = true
		_ = os.Remove(w.path)
		return Blob{}, nil, fmt.Errorf("close temp file: %w", err)
	}
	w.closed = true

	digests := make(map[string]string, len(w.hash))
	for algo, h := range w.hash {
		digests[algo] = hex.EncodeToString(h.Sum(nil))
	}
	if w.rawSize == 0 {
		_ = os.Remove(w.path)
		w.path = ""
		return Blob{}, nil, ErrEmpty
	}
	if w.opts.ExpectedSize != nil && w.rawSize != *w.opts.ExpectedSize {
		_ = os.Remove(w.path)
		w.path = ""
		return Blob{}, nil, fmt.Errorf("%w: expected %d bytes, received %d", ErrLengthMismatch, *w.opts.ExpectedSize, w.rawSize)
	}
	if err := w.checkExpectations(digests); err != nil {
		_ = os.Remove(w.path)
		w.path = ""
		return Blob{}, nil, err
	}

	sha := digests["sha256"]
	final, err := w.store.blobPath(sha)
	if err != nil {
		_ = os.Remove(w.path)
		w.path = ""
		return Blob{}, nil, err
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o750); err != nil {
		_ = os.Remove(w.path)
		w.path = ""
		return Blob{}, nil, fmt.Errorf("create blob directory: %w", err)
	}
	if err := w.store.runHook(w.store.hooks, func(h *commitHooks) func() error { return h.afterMkdir }); err != nil {
		_ = os.Remove(w.path)
		w.path = ""
		return Blob{}, nil, err
	}
	if err := w.store.runHook(w.store.hooks, func(h *commitHooks) func() error { return h.beforeRename }); err != nil {
		_ = os.Remove(w.path)
		w.path = ""
		return Blob{}, nil, err
	}

	unlock := w.store.lockBlob(sha)
	defer unlock()

	storedSize := info.Size()
	encoding := EncodingIdentity
	if w.opts.Compress {
		encoding = EncodingGzip
	}

	if existing, statErr := os.Stat(final); statErr == nil {
		if !existing.Mode().IsRegular() {
			_ = os.Remove(w.path)
			w.path = ""
			return Blob{}, nil, fmt.Errorf("existing blob path is not a regular file: %s", final)
		}
		// A file already at the digest path is only usable when its bytes
		// match the digest. Anything else replaces it with the bytes just
		// written and verified, so a damaged file never becomes metadata.
		if adopted, verifyErr := inspectStoredBlob(final, sha); verifyErr == nil {
			if err := os.Remove(w.path); err != nil {
				return Blob{}, nil, fmt.Errorf("remove duplicate temp blob: %w", err)
			}
			w.path = ""
			w.committed = true
			return adopted, digests, nil
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		_ = os.Remove(w.path)
		w.path = ""
		return Blob{}, nil, fmt.Errorf("stat existing blob: %w", statErr)
	}

	if err := os.Rename(w.path, final); err != nil {
		_ = os.Remove(w.path)
		w.path = ""
		return Blob{}, nil, fmt.Errorf("rename blob into place: %w", err)
	}
	w.path = ""
	if err := w.store.runHook(w.store.hooks, func(h *commitHooks) func() error { return h.afterRename }); err != nil {
		return Blob{}, nil, err
	}
	if err := syncDir(filepath.Dir(final)); err != nil {
		return Blob{}, nil, err
	}
	if err := syncDir(filepath.Dir(filepath.Dir(final))); err != nil {
		return Blob{}, nil, err
	}
	if err := syncDir(w.store.blobsDir); err != nil {
		return Blob{}, nil, err
	}
	if err := w.store.runHook(w.store.hooks, func(h *commitHooks) func() error { return h.afterDirSync }); err != nil {
		return Blob{}, nil, err
	}

	w.committed = true
	return Blob{
		SHA256:     sha,
		RawSize:    w.rawSize,
		StoredSize: storedSize,
		StoredEnc:  encoding,
	}, digests, nil
}

// RawSize returns the number of raw bytes written so far.
func (w *Writer) RawSize() int64 { return w.rawSize }

// TempFile returns the open temporary file for pre-commit validation.
// It is only meaningful for uncompressed writes.
func (w *Writer) TempFile() *os.File { return w.file }

// Abort discards the temporary file.
func (w *Writer) Abort() error {
	if w == nil {
		return nil
	}
	var errs []error
	if w.gzip != nil {
		if err := w.gzip.Close(); err != nil {
			errs = append(errs, err)
		}
		w.gzip = nil
	}
	if !w.closed && w.file != nil {
		if err := w.file.Close(); err != nil {
			errs = append(errs, err)
		}
		w.closed = true
	}
	if w.path != "" {
		if err := os.Remove(w.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
		w.path = ""
	}
	return errors.Join(errs...)
}

func (w *Writer) checkExpectations(digests map[string]string) error {
	for _, e := range w.opts.Expectations {
		algo := strings.ToLower(e.Algo)
		want := strings.ToLower(e.Digest)
		got := digests[algo]
		if got != want {
			return fmt.Errorf("%w: %s expected %s, got %s", ErrDigestMismatch, algo, want, got)
		}
	}
	return nil
}

func newHasher(algo string) (hash.Hash, error) {
	switch strings.ToLower(algo) {
	case "sha256":
		return sha256.New(), nil
	case "sha512":
		return sha512.New(), nil
	case "sha1":
		return sha1.New(), nil
	case "md5":
		return md5.New(), nil
	default:
		return nil, fmt.Errorf("unsupported digest algorithm %q", algo)
	}
}

func (s *Store) runHook(h *commitHooks, pick func(*commitHooks) func() error) error {
	if h == nil {
		return nil
	}
	fn := pick(h)
	if fn == nil {
		return nil
	}
	if err := fn(); err != nil {
		return fmt.Errorf("commit step failed: %w", err)
	}
	return nil
}

func syncDir(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open blob directory for sync: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync blob directory: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close blob directory: %w", err)
	}
	return nil
}

// inspectStoredBlob verifies that the file at path hashes to sha and reports
// the metadata implied by its bytes. A file that does not match is unusable.
func inspectStoredBlob(path, sha string) (Blob, error) {
	file, err := os.Open(path)
	if err != nil {
		return Blob{}, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return Blob{}, fmt.Errorf("stat blob: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Blob{}, fmt.Errorf("blob path is not a regular file")
	}
	enc, source, closeSource, err := decodeStored(file)
	if err != nil {
		return Blob{}, err
	}
	hasher := sha256.New()
	raw, copyErr := io.Copy(hasher, source)
	var closeErr error
	if closeSource != nil {
		closeErr = closeSource()
	}
	if copyErr != nil {
		return Blob{}, fmt.Errorf("read blob: %w", copyErr)
	}
	if closeErr != nil {
		return Blob{}, fmt.Errorf("close blob: %w", closeErr)
	}
	if hex.EncodeToString(hasher.Sum(nil)) != sha {
		return Blob{}, fmt.Errorf("blob digest does not match its name")
	}
	return Blob{SHA256: sha, RawSize: raw, StoredSize: info.Size(), StoredEnc: enc}, nil
}

// decodeStored detects gzip-at-rest and returns a reader over the raw bytes.
func decodeStored(file *os.File) (Encoding, io.Reader, func() error, error) {
	var magic [2]byte
	n, err := io.ReadFull(file, magic[:])
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", nil, nil, fmt.Errorf("read blob header: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", nil, nil, fmt.Errorf("seek blob: %w", err)
	}
	if n == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(file)
		if err != nil {
			return "", nil, nil, fmt.Errorf("open compressed blob: %w", err)
		}
		return EncodingGzip, gz, gz.Close, nil
	}
	return EncodingIdentity, file, nil, nil
}
