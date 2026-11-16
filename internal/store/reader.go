package store

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// Reader serves a blob as raw bytes, decompressing gzip-at-rest blobs.
type Reader struct {
	file   *os.File
	gzip   *gzip.Reader
	source io.Reader
	enc    Encoding
	pos    int64
	size   int64
	closed bool
}

// Open opens a blob for reading raw bytes.
func (s *Store) Open(sha string, enc Encoding) (*Reader, error) {
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
	r := &Reader{file: file, enc: enc, size: -1}
	switch enc {
	case EncodingIdentity:
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("stat blob %s: %w", sha, err)
		}
		r.size = info.Size()
		r.source = file
	case EncodingGzip:
		gz, err := gzip.NewReader(file)
		if err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("open compressed blob %s: %w", sha, err)
		}
		r.gzip = gz
		r.source = gz
	default:
		_ = file.Close()
		return nil, fmt.Errorf("unknown blob encoding %q", enc)
	}
	return r, nil
}

// Read implements io.Reader.
func (r *Reader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, fmt.Errorf("read blob: reader is closed")
	}
	n, err := r.source.Read(p)
	r.pos += int64(n)
	return n, err
}

// Seek implements io.Seeker over the raw bytes.
func (r *Reader) Seek(offset int64, whence int) (int64, error) {
	if r.closed {
		return 0, fmt.Errorf("seek blob: reader is closed")
	}
	if r.enc == EncodingIdentity {
		pos, err := r.file.Seek(offset, whence)
		if err != nil {
			return 0, err
		}
		r.pos = pos
		return pos, nil
	}

	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = r.pos + offset
	case io.SeekEnd:
		if r.size < 0 {
			return 0, fmt.Errorf("seek compressed blob: end is unknown")
		}
		target = r.size + offset
	default:
		return 0, fmt.Errorf("seek blob: invalid whence %d", whence)
	}
	if target < 0 {
		return 0, fmt.Errorf("seek blob: negative position")
	}

	if r.gzip != nil {
		if err := r.gzip.Close(); err != nil {
			return 0, err
		}
		r.gzip = nil
	}
	if _, err := r.file.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	gz, err := gzip.NewReader(r.file)
	if err != nil {
		return 0, err
	}
	r.gzip = gz
	r.source = gz
	r.pos = 0
	if target > 0 {
		n, err := io.CopyN(io.Discard, gz, target)
		r.pos = n
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if errors.Is(err, io.EOF) {
			return target, nil
		}
	}
	r.pos = target
	return target, nil
}

// Size returns the raw size for identity blobs and -1 for compressed blobs.
func (r *Reader) Size() int64 { return r.size }

// Encoding returns the on-disk encoding.
func (r *Reader) Encoding() Encoding { return r.enc }

// Close releases the file handles.
func (r *Reader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	var errs []error
	if r.gzip != nil {
		errs = append(errs, r.gzip.Close())
		r.gzip = nil
	}
	if r.file != nil {
		errs = append(errs, r.file.Close())
	}
	return errors.Join(errs...)
}

// OpenRange opens length raw bytes starting at offset.
func (s *Store) OpenRange(sha string, enc Encoding, offset, length int64) (io.ReadCloser, error) {
	if offset < 0 || length < 0 {
		return nil, ErrInvalidRange
	}
	switch enc {
	case EncodingIdentity:
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
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("stat blob %s: %w", sha, err)
		}
		if offset > info.Size() || offset == info.Size() && length > 0 {
			_ = file.Close()
			return nil, ErrInvalidRange
		}
		if length > info.Size()-offset {
			length = info.Size() - offset
		}
		return &sectionReadCloser{
			SectionReader: io.NewSectionReader(file, offset, length),
			file:          file,
		}, nil
	case EncodingGzip:
		reader, err := s.Open(sha, enc)
		if err != nil {
			return nil, err
		}
		if _, err := reader.Seek(offset, io.SeekStart); err != nil {
			_ = reader.Close()
			return nil, fmt.Errorf("seek compressed blob %s: %w", sha, err)
		}
		return &limitReadCloser{Reader: io.LimitReader(reader, length), closer: reader}, nil
	default:
		return nil, fmt.Errorf("unknown blob encoding %q", enc)
	}
}

type sectionReadCloser struct {
	*io.SectionReader
	file *os.File
}

func (r *sectionReadCloser) Close() error { return r.file.Close() }

type limitReadCloser struct {
	io.Reader
	closer io.Closer
}

func (r *limitReadCloser) Close() error { return r.closer.Close() }
