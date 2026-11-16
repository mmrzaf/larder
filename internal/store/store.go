// Package store implements the durable content-addressed blob store.
package store

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

var (
	// ErrLocked means another process holds the data directory lock.
	ErrLocked = errors.New("data directory is locked")
	// ErrNotFound means the requested blob does not exist.
	ErrNotFound = errors.New("blob not found")
	// ErrInvalidDigest means a value is not a lowercase SHA-256 hex digest.
	ErrInvalidDigest = errors.New("invalid sha256 digest")
	// ErrInvalidRange means a range request has invalid bounds.
	ErrInvalidRange = errors.New("invalid range")
	// ErrTooLarge means a writer exceeded its configured byte limit.
	ErrTooLarge = errors.New("blob exceeds size limit")
	// ErrDigestMismatch means a computed digest did not match an expectation.
	ErrDigestMismatch = errors.New("digest mismatch")
	// ErrLengthMismatch means the received body length differs from the declared length.
	ErrLengthMismatch = errors.New("length mismatch")
)

// Encoding is how a blob is stored on disk.
type Encoding string

const (
	// EncodingIdentity stores the raw bytes unchanged.
	EncodingIdentity Encoding = "identity"
	// EncodingGzip stores gzip-compressed bytes.
	EncodingGzip Encoding = "gzip"
)

// Blob describes a stored blob.
type Blob struct {
	SHA256     string
	RawSize    int64
	StoredSize int64
	StoredEnc  Encoding
}

// Usage is the blob count and stored bytes on disk.
type Usage struct {
	Blobs       int64
	StoredBytes int64
}

// Store owns the data directory layout and its exclusive lock.
type Store struct {
	dir           string
	blobsDir      string
	tmpDir        string
	quarantineDir string
	snapshotsDir  string
	lockFile      *os.File

	// reclaimMu orders publication of new blobs against deletion of
	// unreferenced ones. A blob file exists before its index rows do, so
	// without it a sweep can unlink a blob that is about to be referenced.
	reclaimMu sync.RWMutex

	closeOnce sync.Once
	closeErr  error
	hooks     *commitHooks
}

// Open creates the data layout, takes the exclusive lock and removes leftover temporary files.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("store: data directory is required")
	}
	dir = filepath.Clean(dir)
	s := &Store{
		dir:           dir,
		blobsDir:      filepath.Join(dir, "blobs"),
		tmpDir:        filepath.Join(dir, "tmp"),
		quarantineDir: filepath.Join(dir, "quarantine"),
		snapshotsDir:  filepath.Join(dir, "snapshots"),
	}
	for _, d := range []string{s.dir, s.blobsDir, s.tmpDir, s.quarantineDir, s.snapshotsDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, fmt.Errorf("create data directory %s: %w", d, err)
		}
	}

	lockPath := filepath.Join(s.dir, "larder.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o640)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lockFile.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock data directory: %w", err)
	}
	s.lockFile = lockFile

	if err := s.cleanupTmp(); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the lock. It is safe to call more than once.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		if s.lockFile == nil {
			return
		}
		unlockErr := unix.Flock(int(s.lockFile.Fd()), unix.LOCK_UN)
		closeErr := s.lockFile.Close()
		s.lockFile = nil
		s.closeErr = errors.Join(unlockErr, closeErr)
	})
	return s.closeErr
}

// Publish runs fn while no Reclaim can run. Callers wrap the whole sequence
// that commits a blob and makes it reachable from the index, so a concurrent
// sweep never sees the blob in between.
func (s *Store) Publish(fn func() error) error {
	s.reclaimMu.RLock()
	defer s.reclaimMu.RUnlock()
	return fn()
}

// Reclaim runs fn exclusively with respect to Publish. Callers wrap every
// step that removes blob files or the index rows that justify keeping them.
func (s *Store) Reclaim(fn func() error) error {
	s.reclaimMu.Lock()
	defer s.reclaimMu.Unlock()
	return fn()
}

// DataDir returns the store root.
func (s *Store) DataDir() string { return s.dir }

func (s *Store) cleanupTmp() error {
	entries, err := os.ReadDir(s.tmpDir)
	if err != nil {
		return fmt.Errorf("read temp directory: %w", err)
	}
	for _, entry := range entries {
		path := filepath.Join(s.tmpDir, entry.Name())
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove leftover temp file %s: %w", path, err)
		}
	}
	return nil
}

var shaPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func validSHA(sha string) bool { return shaPattern.MatchString(sha) }

func (s *Store) blobPath(sha string) (string, error) {
	if !validSHA(sha) {
		return "", fmt.Errorf("%w: %q", ErrInvalidDigest, sha)
	}
	return filepath.Join(s.blobsDir, sha[:2], sha[2:4], sha), nil
}

// Quarantine moves a blob out of the serving tree.
func (s *Store) Quarantine(sha string) error {
	src, err := s.blobPath(sha)
	if err != nil {
		return err
	}
	dst := filepath.Join(s.quarantineDir, sha)
	if _, err := os.Stat(src); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat blob %s: %w", sha, err)
	}
	if _, err := os.Stat(dst); err == nil {
		if err := os.Remove(src); err != nil {
			return fmt.Errorf("remove duplicate blob %s: %w", sha, err)
		}
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat quarantined blob %s: %w", sha, err)
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("quarantine blob %s: %w", sha, err)
	}
	now := time.Now()
	_ = os.Chtimes(dst, now, now)
	return nil
}

// Delete removes a blob file. A missing blob is not an error.
func (s *Store) Delete(sha string) error {
	path, err := s.blobPath(sha)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("delete blob %s: %w", sha, err)
	}
	return nil
}

// SizeOnDisk returns the stored size of a blob.
func (s *Store) SizeOnDisk(sha string) (int64, error) {
	path, err := s.blobPath(sha)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("stat blob %s: %w", sha, err)
	}
	return info.Size(), nil
}

// Usage walks the blob tree and returns the number of blobs and their stored bytes.
func (s *Store) Usage() (Usage, error) {
	var usage Usage
	err := filepath.WalkDir(s.blobsDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		usage.Blobs++
		usage.StoredBytes += info.Size()
		return nil
	})
	if err != nil {
		return Usage{}, fmt.Errorf("walk blobs: %w", err)
	}
	return usage, nil
}

// Capacity returns the total bytes of the data filesystem.
func (s *Store) Capacity() (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(s.dir, &stat); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", s.dir, err)
	}
	return stat.Blocks * uint64(stat.Bsize), nil
}

// FreeSpace returns the bytes available to unprivileged users on the data filesystem.
func (s *Store) FreeSpace() (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(s.dir, &stat); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", s.dir, err)
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}

// OpenStored opens the bytes as they are stored, without decompression.
func (s *Store) OpenStored(sha string) (*os.File, error) {
	path, err := s.blobPath(sha)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("open blob %s: %w", sha, err)
	}
	return file, nil
}

var _ io.Closer = (*Store)(nil)

// OpenReadOnly prepares the layout description without taking the lock. It is
// used by reporting commands that run while a server owns the data directory.
func OpenReadOnly(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("store: data directory is required")
	}
	dir = filepath.Clean(dir)
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("open data directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("open data directory: %s is not a directory", dir)
	}
	return &Store{
		dir:           dir,
		blobsDir:      filepath.Join(dir, "blobs"),
		tmpDir:        filepath.Join(dir, "tmp"),
		quarantineDir: filepath.Join(dir, "quarantine"),
		snapshotsDir:  filepath.Join(dir, "snapshots"),
	}, nil
}

// BlobPath returns the on-disk path of a blob digest.
func (s *Store) BlobPath(sha string) (string, error) { return s.blobPath(sha) }

// BlobsDir returns the root of the content-addressed blob tree.
func (s *Store) BlobsDir() string { return s.blobsDir }
