package integrity

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/index"
)

// MaintenanceOptions configures the daily maintenance worker.
type MaintenanceOptions struct {
	Scrubber *Scrubber
	Index    *index.DB
	DataDir  string
	Config   func() *config.Config
	Clock    Clock
	Logger   *slog.Logger
	Prune    func(context.Context) error
	Interval time.Duration
}

// MaintenanceResult reports one maintenance pass.
type MaintenanceResult struct {
	Verify           ScrubResult `json:"verify"`
	Snapshot         string      `json:"snapshot,omitempty"`
	RemovedSnapshots []string    `json:"removed_snapshots,omitempty"`
}

// Maintainer runs the daily scrub slice, history prune and index snapshot.
type Maintainer struct {
	scrubber *Scrubber
	index    *index.DB
	dataDir  string
	config   func() *config.Config
	clock    Clock
	logger   *slog.Logger
	prune    func(context.Context) error
	interval time.Duration
	cursor   string

	mu        sync.Mutex
	runCancel context.CancelFunc
	started   atomic.Bool
	stop      chan struct{}
	done      chan struct{}
	once      sync.Once
}

// NewMaintainer builds a maintenance worker.
func NewMaintainer(opts MaintenanceOptions) (*Maintainer, error) {
	if opts.Scrubber == nil || opts.Index == nil || opts.DataDir == "" || opts.Config == nil {
		return nil, fmt.Errorf("integrity: scrubber, index, data directory and config are required")
	}
	clock := opts.Clock
	if clock == nil {
		clock = realClock{}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	return &Maintainer{
		scrubber: opts.Scrubber, index: opts.Index, dataDir: opts.DataDir,
		config: opts.Config, clock: clock, logger: logger, prune: opts.Prune,
		interval: interval, stop: make(chan struct{}), done: make(chan struct{}),
	}, nil
}

// Start begins the daily loop.
func (m *Maintainer) Start() {
	m.once.Do(func() {
		m.started.Store(true)
		go m.loop()
	})
}

func (m *Maintainer) loop() {
	defer close(m.done)
	timer := m.clock.After(m.interval)
	for {
		select {
		case <-m.stop:
			return
		case <-timer:
			ctx, cancel := context.WithTimeout(context.Background(), m.interval)
			m.mu.Lock()
			m.runCancel = cancel
			m.mu.Unlock()
			if _, err := m.RunOnce(ctx); err != nil && ctx.Err() == nil {
				m.logger.Warn("maintenance_failed", "error", err)
			}
			cancel()
			m.mu.Lock()
			m.runCancel = nil
			m.mu.Unlock()
			timer = m.clock.After(m.interval)
		}
	}
}

// RunOnce performs one scrub slice, snapshot and history prune.
func (m *Maintainer) RunOnce(ctx context.Context) (MaintenanceResult, error) {
	var result MaintenanceResult
	cfg := m.config()
	if cfg == nil {
		return result, fmt.Errorf("maintenance: no configuration")
	}
	m.mu.Lock()
	cursor := m.cursor
	m.mu.Unlock()
	if cfg.Storage.Scrub.DailyPercent > 0 {
		count, err := m.index.BlobCount(ctx)
		if err != nil {
			return result, err
		}
		limit := int((count*int64(cfg.Storage.Scrub.DailyPercent) + 99) / 100)
		if limit < 1 && count > 0 {
			limit = 1
		}
		verify, err := m.scrubber.Scan(ctx, ScrubOptions{
			Sample:  100,
			RateMBs: cfg.Storage.Scrub.RateMBs,
			Limit:   limit,
			Cursor:  cursor,
		})
		if err != nil {
			return result, err
		}
		result.Verify = verify
		m.mu.Lock()
		m.cursor = verify.NextCursor
		m.mu.Unlock()
	}
	path, err := SnapshotIn(ctx, m.index, m.dataDir, m.clock.Now())
	if err != nil {
		return result, err
	}
	result.Snapshot = path
	removed, err := PruneSnapshots(filepath.Dir(path), cfg.Storage.SnapshotKeep)
	if err != nil {
		return result, err
	}
	result.RemovedSnapshots = removed
	if m.prune != nil {
		if err := m.prune(ctx); err != nil {
			m.logger.Warn("maintenance_prune_failed", "error", err)
		}
	}
	m.logger.Info("maintenance_done", "checked", result.Verify.Checked,
		"corrupt", result.Verify.Corrupt, "snapshot", result.Snapshot)
	return result, nil
}

// Close stops the daily loop.
func (m *Maintainer) Close() error {
	if !m.started.Load() {
		return nil
	}
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
	select {
	case <-m.done:
	case <-time.After(5 * time.Second):
	}
	return nil
}
