package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

// Clock is the time source used by the access recorder.
type Clock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time {
	return time.After(d)
}

type accessEntry struct {
	lastAccess time.Time
	hits       int64
}

// AccessRecorder batches last-access and hit updates outside the read path.
type AccessRecorder struct {
	db       *DB
	clock    Clock
	interval time.Duration
	logger   *slog.Logger

	mu      sync.Mutex
	pending map[int64]*accessEntry
	lastErr error

	stop      chan struct{}
	done      chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
	started   bool
	closed    bool

	onFlush func()
}

// NewAccessRecorder returns a recorder that flushes every interval.
func NewAccessRecorder(db *DB, clock Clock, interval time.Duration, logger *slog.Logger) *AccessRecorder {
	if clock == nil {
		clock = systemClock{}
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &AccessRecorder{
		db:       db,
		clock:    clock,
		interval: interval,
		logger:   logger,
		pending:  make(map[int64]*accessEntry),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Record adds one hit for an object. It performs no database I/O.
func (r *AccessRecorder) Record(id int64) {
	if id <= 0 {
		return
	}
	now := r.clock.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	entry := r.pending[id]
	if entry == nil {
		entry = &accessEntry{}
		r.pending[id] = entry
	}
	entry.hits++
	if now.After(entry.lastAccess) {
		entry.lastAccess = now
	}
}

// Flush writes all pending updates in one transaction.
func (r *AccessRecorder) Flush(ctx context.Context) error {
	r.mu.Lock()
	pending := r.pending
	r.pending = make(map[int64]*accessEntry)
	r.mu.Unlock()
	if len(pending) == 0 {
		return nil
	}

	err := r.db.withTx(ctx, func(tx *sql.Tx) error {
		for id, entry := range pending {
			if _, err := tx.ExecContext(ctx,
				`UPDATE objects SET last_access = ?, hits = hits + ? WHERE id = ?`,
				unixTime(entry.lastAccess), entry.hits, id,
			); err != nil {
				return fmt.Errorf("update access for object %d: %w", id, err)
			}
		}
		return nil
	})
	if err != nil {
		r.mu.Lock()
		for id, entry := range pending {
			existing := r.pending[id]
			if existing == nil {
				r.pending[id] = entry
				continue
			}
			existing.hits += entry.hits
			if entry.lastAccess.After(existing.lastAccess) {
				existing.lastAccess = entry.lastAccess
			}
		}
		r.mu.Unlock()
		return err
	}
	if r.onFlush != nil {
		r.onFlush()
	}
	return nil
}

// Start begins periodic flushing.
func (r *AccessRecorder) Start() {
	r.startOnce.Do(func() {
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return
		}
		r.started = true
		r.mu.Unlock()
		timer := r.clock.After(r.interval)
		go r.loop(timer)
	})
}

func (r *AccessRecorder) loop(timer <-chan time.Time) {
	defer close(r.done)
	for {
		select {
		case <-r.stop:
			return
		case <-timer:
			if err := r.Flush(context.Background()); err != nil {
				r.mu.Lock()
				r.lastErr = err
				r.mu.Unlock()
				r.logger.Error("access_flush_failed", "error", err)
			}
			timer = r.clock.After(r.interval)
		}
	}
}

// Close stops periodic flushing and flushes the remaining updates.
func (r *AccessRecorder) Close() error {
	var closeErr error
	r.closeOnce.Do(func() {
		r.mu.Lock()
		started := r.started
		r.closed = true
		r.mu.Unlock()
		if started {
			close(r.stop)
			<-r.done
		}
		closeErr = r.Flush(context.Background())

		r.mu.Lock()
		lastErr := r.lastErr
		r.mu.Unlock()
		closeErr = errors.Join(closeErr, lastErr)
	})
	return closeErr
}

// SetAccess replaces the last-access time and hit count of one object. Import,
// restore and tests use it to reproduce an existing access history.
func (db *DB) SetAccess(ctx context.Context, id int64, lastAccess time.Time, hits int64) error {
	if id <= 0 {
		return fmt.Errorf("set access: object id is required")
	}
	if hits < 0 {
		return fmt.Errorf("set access: hits must not be negative")
	}
	if _, err := db.sql.ExecContext(ctx,
		`UPDATE objects SET last_access = ?, hits = ? WHERE id = ?`,
		unixTime(lastAccess), hits, id,
	); err != nil {
		return fmt.Errorf("set access for object %d: %w", id, err)
	}
	return nil
}
