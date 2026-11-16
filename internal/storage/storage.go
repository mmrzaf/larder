// Package storage implements budget accounting, pin resolution, garbage
// collection and the emergency bypass state.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/metrics"
	"github.com/mmrzaf/larder/internal/store"
)

// Clock is the time source used by storage management.
type Clock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

var (
	// ErrBusy means another garbage collection is already running.
	ErrBusy = errors.New("garbage collection already running")
	// ErrInvalidTarget means a GC target is not a usable size.
	ErrInvalidTarget = errors.New("invalid GC target")
)

// Options configures a storage manager.
type Options struct {
	Config  *config.Config
	Store   *store.Store
	Index   *index.DB
	Metrics *metrics.Metrics
	Clock   Clock
	Logger  *slog.Logger
	// Capacity and FreeSpace override the filesystem probes in tests.
	Capacity  func() (uint64, error)
	FreeSpace func() (uint64, error)
}

// Usage is the current budget and consumption.
type Usage struct {
	BudgetBytes      int64 `json:"budget_bytes"`
	UsedBytes        int64 `json:"used_bytes"`
	BlobBytes        int64 `json:"blob_bytes"`
	IndexBytes       int64 `json:"index_bytes"`
	SnapshotBytes    int64 `json:"snapshot_bytes"`
	PinnedBytes      int64 `json:"pinned_bytes"`
	FreeBytes        int64 `json:"disk_free_bytes"`
	CapacityBytes    int64 `json:"disk_capacity_bytes"`
	Blobs            int64 `json:"blobs"`
	Objects          int64 `json:"objects"`
	PinnedOverBudget bool  `json:"pinned_over_budget"`
	BypassActive     bool  `json:"bypass_active"`
}

// Manager owns budget accounting, pins, garbage collection and bypass state.
type Manager struct {
	cfg     atomic.Pointer[config.Config]
	store   *store.Store
	index   *index.DB
	metrics *metrics.Metrics
	clock   Clock
	logger  *slog.Logger

	capacity  func() (uint64, error)
	freeSpace func() (uint64, error)

	usageMu  sync.Mutex
	usage    Usage
	usageAt  time.Time
	usageTTL time.Duration

	reserveMu       sync.Mutex
	reserved        int64
	bypass          atomic.Bool
	pinnedOver      atomic.Bool
	lastFreeCheck   atomic.Int64
	gcMu            sync.Mutex
	startOnce       sync.Once
	closeOnce       sync.Once
	started         atomic.Bool
	stop            chan struct{}
	done            chan struct{}
	testBeforeBatch func()
	runMu           sync.Mutex
	runCancel       context.CancelFunc
}

// New builds a storage manager.
func New(opts Options) (*Manager, error) {
	if opts.Config == nil || opts.Store == nil || opts.Index == nil {
		return nil, fmt.Errorf("storage: config, store and index are required")
	}
	clock := opts.Clock
	if clock == nil {
		clock = realClock{}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	m := &Manager{
		store: opts.Store, index: opts.Index, metrics: opts.Metrics,
		clock: clock, logger: logger,
		capacity: opts.Capacity, freeSpace: opts.FreeSpace,
		usageTTL: 10 * time.Second,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	if m.capacity == nil {
		m.capacity = opts.Store.Capacity
	}
	if m.freeSpace == nil {
		m.freeSpace = opts.Store.FreeSpace
	}
	m.cfg.Store(opts.Config)
	return m, nil
}

// UpdateConfig swaps the configuration used by later accounting and GC runs.
func (m *Manager) UpdateConfig(cfg *config.Config) {
	if cfg != nil {
		m.cfg.Store(cfg)
		m.usageMu.Lock()
		m.usageAt = time.Time{}
		m.usageMu.Unlock()
	}
}

// Config returns the active configuration.
func (m *Manager) Config() *config.Config { return m.cfg.Load() }

// Bypass reports whether objects must be streamed instead of stored.
func (m *Manager) Bypass(string) bool {
	m.checkFree()
	return m.bypass.Load()
}

// Reserve claims space for a download of the given size. It refuses when the
// bytes already promised to downloads in progress, plus this one, would leave
// less than the hard free-space floor, so several large transfers cannot fill
// the disk between two free-space probes.
func (m *Manager) Reserve(size int64) (func(), bool) {
	free, err := m.freeSpace()
	cfg := m.cfg.Load()
	if err != nil || cfg == nil {
		return func() {}, true
	}
	floor := cfg.Storage.HardFreeBytes.Bytes()
	m.reserveMu.Lock()
	defer m.reserveMu.Unlock()
	if int64(free)-m.reserved-size < floor {
		return nil, false
	}
	m.reserved += size
	var once sync.Once
	return func() {
		once.Do(func() {
			m.reserveMu.Lock()
			m.reserved -= size
			m.reserveMu.Unlock()
		})
	}, true
}

// BypassActive reports the current bypass state without probing the disk.
func (m *Manager) BypassActive() bool { return m.bypass.Load() }

// PinnedOverBudget reports whether pinned content exceeds the budget.
func (m *Manager) PinnedOverBudget() bool { return m.pinnedOver.Load() }

func (m *Manager) checkFree() {
	now := m.clock.Now().UnixNano()
	last := m.lastFreeCheck.Load()
	if last != 0 && now-last < int64(time.Second) {
		return
	}
	if !m.lastFreeCheck.CompareAndSwap(last, now) {
		return
	}
	free, err := m.freeSpace()
	if err != nil {
		return
	}
	cfg := m.cfg.Load()
	if cfg == nil {
		return
	}
	hard := uint64(cfg.Storage.HardFreeBytes.Bytes())
	if free < hard {
		m.enterBypass("hard_free_bytes")
		return
	}
	if !m.bypass.Load() {
		return
	}
	m.usageMu.Lock()
	usage := m.usage
	known := !m.usageAt.IsZero()
	m.usageMu.Unlock()
	if !known {
		return
	}
	maxBytes := cfg.Storage.MaxBytes.Bytes()
	if maxBytes > 0 && float64(usage.UsedBytes) >= cfg.Storage.HighWatermark*float64(usage.BudgetBytes) {
		return
	}
	m.exitBypass("free_space_recovered")
}

func (m *Manager) enterBypass(reason string) {
	if m.bypass.CompareAndSwap(false, true) {
		m.logger.Warn("bypass_start", "reason", reason)
	}
	if m.metrics != nil {
		m.metrics.BypassActive.With().Set(1)
	}
}

func (m *Manager) exitBypass(reason string) {
	if m.bypass.CompareAndSwap(true, false) {
		m.logger.Info("bypass_end", "reason", reason)
	}
	if m.metrics != nil {
		m.metrics.BypassActive.With().Set(0)
	}
}

func (m *Manager) setPinnedOver(over bool) {
	m.pinnedOver.Store(over)
	if m.metrics != nil {
		value := 0.0
		if over {
			value = 1
		}
		m.metrics.PinnedOverBudget.With().Set(value)
	}
	if over {
		m.logger.Error("pinned_over_budget", "message", "pinned content exceeds the storage budget")
	}
}

// Start begins the periodic usage check. It is safe to call once.
func (m *Manager) Start() {
	m.startOnce.Do(func() {
		m.started.Store(true)
		go m.loop()
	})
}

func (m *Manager) loop() {
	defer close(m.done)
	cfg := m.cfg.Load()
	interval := 5 * time.Minute
	if cfg != nil && cfg.Storage.GCInterval.Duration() > 0 {
		interval = cfg.Storage.GCInterval.Duration()
	}
	timer := m.clock.After(interval)
	for {
		select {
		case <-m.stop:
			return
		case <-timer:
			m.runScheduled()
			cfg := m.cfg.Load()
			interval = 5 * time.Minute
			if cfg != nil && cfg.Storage.GCInterval.Duration() > 0 {
				interval = cfg.Storage.GCInterval.Duration()
			}
			timer = m.clock.After(interval)
		}
	}
}

func (m *Manager) runScheduled() {
	cfg := m.cfg.Load()
	if cfg == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Storage.GCMaxDuration.Duration())
	m.runMu.Lock()
	m.runCancel = cancel
	m.runMu.Unlock()
	defer func() {
		cancel()
		m.runMu.Lock()
		m.runCancel = nil
		m.runMu.Unlock()
	}()
	if _, err := m.GC(ctx, GCOptions{Trigger: "schedule"}); err != nil && !errors.Is(err, ErrBusy) && !errors.Is(err, context.Canceled) {
		m.logger.Warn("gc_failed", "trigger", "schedule", "error", err)
	}
}

// Close stops the periodic worker.
func (m *Manager) Close() error {
	m.closeOnce.Do(func() {
		if !m.started.Load() {
			return
		}
		close(m.stop)
		m.runMu.Lock()
		cancel := m.runCancel
		m.runMu.Unlock()
		if cancel != nil {
			cancel()
		}
		select {
		case <-m.done:
		case <-time.After(30 * time.Second):
		}
	})
	return nil
}

// Usage returns the cached accounting, recomputing it after the TTL.
func (m *Manager) Usage(ctx context.Context, force bool) (Usage, error) {
	m.usageMu.Lock()
	if !force && !m.usageAt.IsZero() && m.clock.Now().Sub(m.usageAt) < m.usageTTL {
		usage := m.usage
		m.usageMu.Unlock()
		return usage, nil
	}
	m.usageMu.Unlock()

	usage, err := m.computeUsage(ctx)
	if err != nil {
		return Usage{}, err
	}
	m.usageMu.Lock()
	m.usage = usage
	m.usageAt = m.clock.Now()
	m.usageMu.Unlock()
	m.publish(usage)
	m.evaluateBudget(usage)
	return usage, nil
}

func (m *Manager) invalidateUsage() {
	m.usageMu.Lock()
	m.usageAt = time.Time{}
	m.usageMu.Unlock()
}

func (m *Manager) computeUsage(ctx context.Context) (Usage, error) {
	cfg := m.cfg.Load()
	if cfg == nil {
		return Usage{}, fmt.Errorf("storage: no configuration")
	}
	blobTotals, err := m.index.BlobTotals(ctx)
	if err != nil {
		return Usage{}, err
	}
	counts, err := m.index.ObjectCounts(ctx)
	if err != nil {
		return Usage{}, err
	}
	objects := int64(0)
	for _, count := range counts {
		objects += count.Objects
	}
	capacity, _ := m.capacity()
	free, _ := m.freeSpace()
	budget := effectiveBudget(cfg, capacity)
	indexBytes := indexFileBytes(cfg.DataDir)
	snapshotBytes := directoryBytes(filepath.Join(cfg.DataDir, "snapshots"))
	used := blobTotals.StoredBytes + indexBytes + snapshotBytes
	pinned, err := m.pinnedBytes(ctx, cfg)
	if err != nil {
		return Usage{}, err
	}
	return Usage{
		BudgetBytes:   budget,
		UsedBytes:     used,
		BlobBytes:     blobTotals.StoredBytes,
		IndexBytes:    indexBytes,
		SnapshotBytes: snapshotBytes,
		PinnedBytes:   pinned,
		FreeBytes:     int64(free),
		CapacityBytes: int64(capacity),
		Blobs:         blobTotals.Blobs,
		Objects:       objects,
	}, nil
}

func effectiveBudget(cfg *config.Config, capacity uint64) int64 {
	budget := cfg.Storage.MaxBytes.Bytes()
	if capacity > 0 {
		disk := int64(capacity) - cfg.Storage.MinFreeBytes.Bytes()
		if disk < 0 {
			disk = 0
		}
		if budget <= 0 || disk < budget {
			budget = disk
		}
	}
	return budget
}

func (m *Manager) publish(usage Usage) {
	if m.metrics == nil {
		return
	}
	m.metrics.BudgetBytes.With().Set(float64(usage.BudgetBytes))
	m.metrics.UsedBytes.With().Set(float64(usage.UsedBytes))
	m.metrics.DiskFreeBytes.With().Set(float64(usage.FreeBytes))
	m.metrics.PinnedBytes.With().Set(float64(usage.PinnedBytes))
	cfg := m.cfg.Load()
	if cfg == nil {
		return
	}
	counts, err := m.index.ObjectCounts(context.Background())
	if err != nil {
		return
	}
	totals, err := m.index.RepoClassTotals(context.Background())
	if err != nil {
		return
	}
	for _, count := range counts {
		m.metrics.CacheObjects.With(count.Repo, count.Class).Set(float64(count.Objects))
	}
	for _, total := range totals {
		m.metrics.CacheBytes.With(total.Repo, total.Class).Set(float64(total.StoredBytes))
	}
}

func (m *Manager) evaluateBudget(usage Usage) {
	cfg := m.cfg.Load()
	if cfg == nil {
		return
	}
	overPinned := usage.BudgetBytes > 0 && usage.PinnedBytes > usage.BudgetBytes
	m.setPinnedOver(overPinned)

	hard := uint64(cfg.Storage.HardFreeBytes.Bytes())
	if usage.FreeBytes >= 0 && uint64(usage.FreeBytes) < hard {
		m.enterBypass("hard_free_bytes")
		return
	}
	if !m.bypass.Load() {
		return
	}
	if m.pinnedOver.Load() {
		return
	}
	if usage.BudgetBytes <= 0 || float64(usage.UsedBytes) < cfg.Storage.HighWatermark*float64(usage.BudgetBytes) {
		m.exitBypass("below_high_watermark")
	}
}

func indexFileBytes(dataDir string) int64 {
	var total int64
	for _, name := range []string{"index.db", "index.db-wal", "index.db-shm"} {
		if info, err := os.Stat(filepath.Join(dataDir, name)); err == nil {
			total += info.Size()
		}
	}
	return total
}

func directoryBytes(dir string) int64 {
	var total int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if info, err := entry.Info(); err == nil {
			total += info.Size()
		}
	}
	return total
}
