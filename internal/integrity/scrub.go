package integrity

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/metrics"
	"github.com/mmrzaf/larder/internal/store"
)

// Clock is the time source used by the scrubber.
type Clock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// RateLimiter throttles reads to a byte rate.
type RateLimiter struct {
	rate     int64
	clock    Clock
	start    time.Time
	consumed int64
	wait     func(context.Context, time.Duration) error
}

// NewRateLimiter builds a limiter. A non-positive rate disables throttling.
func NewRateLimiter(bytesPerSecond int64, clock Clock) *RateLimiter {
	if clock == nil {
		clock = realClock{}
	}
	return &RateLimiter{rate: bytesPerSecond, clock: clock, wait: sleepContext}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Wait accounts for n bytes and blocks until the average rate allows them.
func (l *RateLimiter) Wait(ctx context.Context, n int64) error {
	if l == nil || l.rate <= 0 || n <= 0 {
		return nil
	}
	now := l.clock.Now()
	if l.start.IsZero() {
		l.start = now
	}
	l.consumed += n
	target := time.Duration(float64(l.consumed) / float64(l.rate) * float64(time.Second))
	delay := target - now.Sub(l.start)
	if delay <= 0 {
		return nil
	}
	return l.wait(ctx, delay)
}

// Scrubber verifies stored blobs and quarantines corrupt content.
type Scrubber struct {
	store   *store.Store
	index   *index.DB
	metrics *metrics.Metrics
	logger  *slog.Logger
	clock   Clock
}

// ScrubOptions controls one verification pass.
type ScrubOptions struct {
	Repo              string
	Sample            int
	RateMBs           int
	Limit             int
	Cursor            string
	DryRun            bool
	CheckExpectations bool
	Repair            func(context.Context, []index.ObjectRef) (int, error)
	Progress          func(checked, bytes int64)
}

// CorruptBlob describes one failed verification.
type CorruptBlob struct {
	SHA256  string `json:"sha256"`
	Reason  string `json:"reason"`
	Objects int    `json:"objects"`
}

// ExpectationMismatch is a stored object that disagrees with its metadata.
type ExpectationMismatch struct {
	Repo      string `json:"repo"`
	Key       string `json:"key"`
	SHA256    string `json:"sha256"`
	Expected  string `json:"expected"`
	Algorithm string `json:"algo"`
}

// ScrubResult summarizes a verification pass.
type ScrubResult struct {
	Checked        int64                 `json:"checked"`
	Corrupt        int64                 `json:"corrupt"`
	Quarantined    int64                 `json:"quarantined"`
	Missing        int64                 `json:"missing"`
	RemovedObjects int64                 `json:"removed_objects"`
	Repaired       int64                 `json:"repaired"`
	BytesRead      int64                 `json:"bytes_read"`
	NextCursor     string                `json:"next_cursor,omitempty"`
	CorruptBlobs   []CorruptBlob         `json:"corrupt_blobs,omitempty"`
	Expectations   []ExpectationMismatch `json:"expectation_mismatches,omitempty"`
	Unverified     int64                 `json:"unverified"`
}

// NewScrubber builds a scrubber. A nil metrics registry disables counters.
func NewScrubber(st *store.Store, idx *index.DB, metricSet *metrics.Metrics, logger *slog.Logger, clock Clock) (*Scrubber, error) {
	if st == nil || idx == nil {
		return nil, fmt.Errorf("integrity: store and index are required")
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if clock == nil {
		clock = realClock{}
	}
	return &Scrubber{store: st, index: idx, metrics: metricSet, logger: logger, clock: clock}, nil
}

// Scan verifies the selected blobs without holding a write transaction while
// reading them.
func (s *Scrubber) Scan(ctx context.Context, opts ScrubOptions) (ScrubResult, error) {
	if opts.Sample < 0 || opts.Sample > 100 {
		return ScrubResult{}, fmt.Errorf("verify: sample must be between 0 and 100")
	}
	blobs, next, err := s.selectBlobs(ctx, opts)
	if err != nil {
		return ScrubResult{}, err
	}
	rate := int64(opts.RateMBs) << 20
	limiter := NewRateLimiter(rate, s.clock)
	result := ScrubResult{NextCursor: next}
	for _, blob := range blobs {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if !sampled(blob.SHA256, opts.Sample) {
			continue
		}
		reason, read, err := s.verifyBlob(ctx, blob, limiter)
		result.Checked++
		result.BytesRead += read
		if err != nil {
			return result, err
		}
		if opts.Progress != nil && result.Checked%100 == 0 {
			opts.Progress(result.Checked, result.BytesRead)
		}
		if reason == "" {
			continue
		}
		result.Corrupt++
		if reason == "missing" {
			result.Missing++
		}
		entry := CorruptBlob{SHA256: blob.SHA256, Reason: reason}
		if opts.DryRun {
			result.CorruptBlobs = append(result.CorruptBlobs, entry)
			continue
		}
		refs, err := s.index.DeleteBlobAndObjects(ctx, blob.SHA256)
		if err != nil {
			return result, err
		}
		entry.Objects = len(refs)
		result.RemovedObjects += int64(len(refs))
		if reason != "missing" {
			if err := s.store.Quarantine(blob.SHA256); err != nil {
				s.logger.Warn("quarantine_failed", "sha256", blob.SHA256, "error", err)
			} else {
				result.Quarantined++
			}
		}
		result.CorruptBlobs = append(result.CorruptBlobs, entry)
		if s.metrics != nil {
			s.metrics.ScrubCorrupt.With().Inc()
		}
		s.logger.Error("scrub_corrupt", "sha256", blob.SHA256, "reason", reason, "objects", len(refs))
		if opts.Repair != nil && len(refs) > 0 {
			repaired, err := opts.Repair(ctx, refs)
			if err != nil {
				s.logger.Warn("scrub_repair_failed", "sha256", blob.SHA256, "error", err)
			} else {
				result.Repaired += int64(repaired)
			}
		}
	}
	if opts.Progress != nil {
		opts.Progress(result.Checked, result.BytesRead)
	}
	if opts.CheckExpectations {
		rows, err := s.index.ExpectationRows(ctx, opts.Repo)
		if err != nil {
			return result, err
		}
		for _, row := range rows {
			if row.Algo == "sha256" {
				if !strings.EqualFold(row.SHA256, row.Digest) {
					result.Expectations = append(result.Expectations, ExpectationMismatch{
						Repo: row.Repo, Key: row.Key, SHA256: row.SHA256,
						Expected: row.Digest, Algorithm: row.Algo,
					})
				}
				continue
			}
			digest, ok := s.objectDigest(ctx, row.SHA256, row.Algo)
			if !ok || !strings.EqualFold(digest, row.Digest) {
				result.Expectations = append(result.Expectations, ExpectationMismatch{
					Repo: row.Repo, Key: row.Key, SHA256: row.SHA256,
					Expected: row.Digest, Algorithm: row.Algo,
				})
			}
		}
		unverified, err := s.index.UnverifiedCount(ctx, opts.Repo)
		if err != nil {
			return result, err
		}
		result.Unverified = unverified
	}
	return result, nil
}

func (s *Scrubber) selectBlobs(ctx context.Context, opts ScrubOptions) ([]index.Blob, string, error) {
	if opts.Repo != "" {
		blobs, err := s.index.BlobsForRepo(ctx, opts.Repo)
		return blobs, "", err
	}
	if opts.Limit > 0 {
		blobs, err := s.index.BlobPage(ctx, opts.Cursor, opts.Limit)
		if err != nil {
			return nil, "", err
		}
		next := ""
		if len(blobs) == opts.Limit {
			next = blobs[len(blobs)-1].SHA256
		}
		return blobs, next, nil
	}
	blobs, err := s.index.AllBlobs(ctx)
	return blobs, "", err
}

func (s *Scrubber) verifyBlob(ctx context.Context, blob index.Blob, limiter *RateLimiter) (string, int64, error) {
	size, err := s.store.SizeOnDisk(blob.SHA256)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "missing", 0, nil
		}
		return "", 0, err
	}
	if err := limiter.Wait(ctx, size); err != nil {
		return "", 0, err
	}
	reader, err := s.store.Open(blob.SHA256, store.Encoding(blob.StoredEnc))
	if err != nil {
		return "unreadable", 0, nil
	}
	hasher := sha256.New()
	raw, read, readErr := hashReader(ctx, reader, hasher)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		return "unreadable", read, nil
	}
	if size != blob.StoredSize {
		return "size", raw, nil
	}
	if raw != blob.RawSize {
		return "size", raw, nil
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(digest, blob.SHA256) {
		return "digest", raw, nil
	}
	return "", raw, nil
}

func hashReader(ctx context.Context, reader io.Reader, hasher hash.Hash) (int64, int64, error) {
	buffer := make([]byte, 64<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, total, err
		}
		n, err := reader.Read(buffer)
		if n > 0 {
			_, _ = hasher.Write(buffer[:n])
			total += int64(n)
		}
		if errors.Is(err, io.EOF) {
			return total, total, nil
		}
		if err != nil {
			return total, total, err
		}
	}
}

func (s *Scrubber) objectDigest(ctx context.Context, sha, algo string) (string, bool) {
	blob, err := s.index.GetBlob(ctx, sha)
	if err != nil {
		return "", false
	}
	reader, err := s.store.Open(sha, store.Encoding(blob.StoredEnc))
	if err != nil {
		return "", false
	}
	hasher, err := NewHasher(algo)
	if err != nil {
		_ = reader.Close()
		return "", false
	}
	_, copyErr := io.Copy(hasher, reader)
	closeErr := reader.Close()
	if copyErr != nil || closeErr != nil {
		return "", false
	}
	return hasher.Sum()[algo], true
}

func sampled(sha string, pct int) bool {
	if pct >= 100 {
		return true
	}
	if pct <= 0 || len(sha) < 8 {
		return false
	}
	raw, err := hex.DecodeString(sha[:8])
	if err != nil {
		return false
	}
	return int(binary.BigEndian.Uint32(raw)%100) < pct
}
