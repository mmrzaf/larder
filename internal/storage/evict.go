package storage

import (
	"context"
	"fmt"
	"sort"

	"github.com/mmrzaf/larder/internal/index"
)

// EvictOptions selects objects for manual removal.
type EvictOptions struct {
	Repo          string
	Glob          string
	IncludePinned bool
	OlderThanDays int
	LargerThan    int64
	DryRun        bool
	Progress      func(Progress)
}

// EvictResult summarizes a manual eviction.
type EvictResult struct {
	DryRun  bool     `json:"dry_run"`
	Repo    string   `json:"repo"`
	Glob    string   `json:"glob"`
	Objects int64    `json:"objects"`
	Bytes   int64    `json:"bytes"`
	Victims []Victim `json:"victims,omitempty"`
}

// Evict removes every generation of keys matching a glob and filters.
func (m *Manager) Evict(ctx context.Context, opts EvictOptions) (EvictResult, error) {
	if opts.Repo == "" || opts.Glob == "" {
		return EvictResult{}, fmt.Errorf("evict: repo and glob are required")
	}
	if opts.OlderThanDays < 0 {
		return EvictResult{}, fmt.Errorf("evict: older-than must not be negative")
	}
	if opts.LargerThan < 0 {
		return EvictResult{}, fmt.Errorf("evict: larger-than must not be negative")
	}
	if !m.gcMu.TryLock() {
		return EvictResult{}, ErrBusy
	}
	defer m.gcMu.Unlock()

	now := m.clock.Now()
	objects, err := m.index.CurrentObjects(ctx, opts.Repo)
	if err != nil {
		return EvictResult{}, err
	}
	matcher, err := m.loadPins(ctx)
	if err != nil {
		return EvictResult{}, err
	}
	var keys []string
	for _, object := range objects {
		if !pinMatch(opts.Glob, object.Key) {
			continue
		}
		if !opts.IncludePinned && matcher.pinned(object.Repo, object.Key) {
			continue
		}
		if opts.OlderThanDays > 0 {
			cutoff := now.AddDate(0, 0, -opts.OlderThanDays)
			if !accessedAt(object).Before(cutoff) {
				continue
			}
		}
		if opts.LargerThan > 0 && object.StoredSize <= opts.LargerThan {
			continue
		}
		keys = append(keys, object.Key)
	}
	sort.Strings(keys)

	result := EvictResult{DryRun: opts.DryRun, Repo: opts.Repo, Glob: opts.Glob}
	tracker, err := newVictimTracker(ctx, m.index)
	if err != nil {
		return EvictResult{}, err
	}
	selected := make(map[int64]bool)
	var candidates []index.Candidate
	for _, key := range keys {
		generations, err := m.index.KeyCandidates(ctx, opts.Repo, key)
		if err != nil {
			return EvictResult{}, err
		}
		candidates = append(candidates, generations...)
	}
	tier := TierResult{Tier: -1}
	if err := m.applyCandidates(ctx, &tier, candidates, tracker, opts.DryRun, selected, "evict"); err != nil {
		return EvictResult{}, err
	}
	result.Objects = tier.Objects
	result.Bytes = tier.Bytes
	result.Victims = tier.Victims
	if opts.Progress != nil {
		opts.Progress(Progress{Items: result.Objects, Bytes: result.Bytes, Message: "evict"})
	}
	if !opts.DryRun {
		m.invalidateUsage()
	}
	m.logger.Info("evict_done", "repo", opts.Repo, "glob", opts.Glob, "dry_run", opts.DryRun,
		"objects", result.Objects, "bytes", result.Bytes)
	return result, nil
}

// PruneOptions controls history pruning.
type PruneOptions struct {
	Keep   int
	Repo   string
	DryRun bool
}

// PruneResult summarizes history pruning.
type PruneResult struct {
	DryRun  bool     `json:"dry_run"`
	Keep    int      `json:"keep"`
	Repo    string   `json:"repo,omitempty"`
	Objects int64    `json:"objects"`
	Bytes   int64    `json:"bytes"`
	Victims []Victim `json:"victims,omitempty"`
}

// Prune removes retired metadata generations beyond the retention count.
func (m *Manager) Prune(ctx context.Context, opts PruneOptions) (PruneResult, error) {
	cfg := m.cfg.Load()
	if cfg == nil {
		return PruneResult{}, fmt.Errorf("storage: no configuration")
	}
	keep := opts.Keep
	if keep < -1 {
		return PruneResult{}, fmt.Errorf("prune: keep must not be negative")
	}
	if keep == -1 {
		keep = cfg.Cache.MetadataHistory + 1
	}
	if keep < 0 {
		return PruneResult{}, fmt.Errorf("prune: keep must not be negative")
	}
	if !m.gcMu.TryLock() {
		return PruneResult{}, ErrBusy
	}
	defer m.gcMu.Unlock()

	candidates, err := m.index.HistoryCandidates(ctx, opts.Repo, keep)
	if err != nil {
		return PruneResult{}, err
	}
	matcher, err := m.loadPins(ctx)
	if err != nil {
		return PruneResult{}, err
	}
	candidates = filterPinned(candidates, matcher)
	tracker, err := newVictimTracker(ctx, m.index)
	if err != nil {
		return PruneResult{}, err
	}
	tier := TierResult{Tier: -1}
	if err := m.applyCandidates(ctx, &tier, candidates, tracker, opts.DryRun, make(map[int64]bool), "history"); err != nil {
		return PruneResult{}, err
	}
	result := PruneResult{
		DryRun: opts.DryRun, Keep: keep, Repo: opts.Repo,
		Objects: tier.Objects, Bytes: tier.Bytes, Victims: tier.Victims,
	}
	if !opts.DryRun {
		m.invalidateUsage()
	}
	m.logger.Info("prune_done", "keep", keep, "repo", opts.Repo, "dry_run", opts.DryRun,
		"objects", result.Objects, "bytes", result.Bytes)
	return result, nil
}

// Rollback makes an earlier generation current.
func (m *Manager) Rollback(ctx context.Context, repo, key string, generation int64) (index.Object, error) {
	if generation < 0 {
		return index.Object{}, fmt.Errorf("rollback: generation must not be negative")
	}
	if !m.gcMu.TryLock() {
		return index.Object{}, ErrBusy
	}
	defer m.gcMu.Unlock()
	object, err := m.index.Rollback(ctx, repo, key, generation)
	if err != nil {
		return index.Object{}, err
	}
	m.invalidateUsage()
	return object, nil
}
