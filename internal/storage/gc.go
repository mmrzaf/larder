package storage

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/index"
)

const (
	deleteBatchSize = 200
	blobBatchSize   = 500
)

// GCOptions controls one garbage collection run.
type GCOptions struct {
	DryRun      bool
	HasTarget   bool
	TargetBytes int64
	TargetPct   float64
	Policy      string
	Trigger     string
	Progress    func(Progress)
}

// Progress reports GC progress to an admin job.
type Progress struct {
	Items   int64
	Bytes   int64
	Message string
}

// Victim is one object or file selected for removal.
type Victim struct {
	Repo       string `json:"repo,omitempty"`
	Key        string `json:"key,omitempty"`
	Generation int64  `json:"generation,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Path       string `json:"path,omitempty"`
	Size       int64  `json:"size"`
	Freed      int64  `json:"freed"`
}

// TierResult is the outcome of one eviction tier.
type TierResult struct {
	Tier    int      `json:"tier"`
	Objects int64    `json:"objects"`
	Bytes   int64    `json:"bytes"`
	Victims []Victim `json:"victims,omitempty"`
}

// GCResult summarizes one garbage collection run.
type GCResult struct {
	DryRun        bool         `json:"dry_run"`
	Trigger       string       `json:"trigger"`
	Policy        string       `json:"policy"`
	BudgetBytes   int64        `json:"budget_bytes"`
	UsedBefore    int64        `json:"used_before"`
	UsedAfter     int64        `json:"used_after"`
	TargetBytes   int64        `json:"target_bytes"`
	TargetReached bool         `json:"target_reached"`
	PinnedOver    bool         `json:"pinned_over_budget"`
	Bypass        bool         `json:"bypass_active"`
	Stopped       string       `json:"stopped,omitempty"`
	Tiers         []TierResult `json:"tiers"`
}

type victimTracker struct {
	refcounts map[string]int64
	freed     int64
}

func newVictimTracker(ctx context.Context, db *index.DB) (*victimTracker, error) {
	refcounts, err := db.RefCounts(ctx)
	if err != nil {
		return nil, err
	}
	return &victimTracker{refcounts: refcounts}, nil
}

func (t *victimTracker) claim(candidate index.Candidate) int64 {
	remaining, ok := t.refcounts[candidate.SHA256]
	if !ok {
		return 0
	}
	if remaining <= 0 {
		return 0
	}
	remaining--
	t.refcounts[candidate.SHA256] = remaining
	if remaining > 0 {
		return 0
	}
	t.freed += candidate.StoredSize
	return candidate.StoredSize
}

// GC runs the tiered eviction described by the storage specification.
func (m *Manager) GC(ctx context.Context, opts GCOptions) (GCResult, error) {
	if !m.gcMu.TryLock() {
		return GCResult{}, ErrBusy
	}
	defer m.gcMu.Unlock()

	cfg := m.cfg.Load()
	if cfg == nil {
		return GCResult{}, fmt.Errorf("storage: no configuration")
	}
	policy := opts.Policy
	if policy == "" {
		policy = cfg.Storage.EvictionPolicy
	}
	switch policy {
	case "lru", "lfu", "score":
	default:
		return GCResult{}, fmt.Errorf("storage: unknown eviction policy %q", policy)
	}
	if opts.HasTarget && opts.TargetBytes < 0 {
		return GCResult{}, ErrInvalidTarget
	}
	if opts.TargetPct < 0 || opts.TargetPct > 100 {
		return GCResult{}, ErrInvalidTarget
	}

	now := m.clock.Now()
	trigger := opts.Trigger
	if trigger == "" {
		trigger = "manual"
	}
	usage, err := m.Usage(ctx, true)
	if err != nil {
		return GCResult{}, err
	}
	result := GCResult{
		DryRun: opts.DryRun, Trigger: trigger, Policy: policy,
		BudgetBytes: usage.BudgetBytes, UsedBefore: usage.UsedBytes,
		Tiers: make([]TierResult, 6),
	}
	for i := range result.Tiers {
		result.Tiers[i].Tier = i
	}

	target, targetSet := m.gcTarget(cfg, usage, opts)
	if opts.HasTarget || opts.TargetPct > 0 {
		if usage.BudgetBytes <= 0 && opts.TargetPct > 0 {
			return GCResult{}, ErrInvalidTarget
		}
	}
	result.TargetBytes = target
	if !targetSet {
		target = usage.UsedBytes
	}
	m.logger.Info("gc_start", "trigger", trigger, "dry_run", opts.DryRun,
		"budget", usage.BudgetBytes, "used", usage.UsedBytes, "target", target)

	runCtx := ctx
	var cancel context.CancelFunc
	if cfg.Storage.GCMaxDuration.Duration() > 0 {
		runCtx, cancel = context.WithTimeout(ctx, cfg.Storage.GCMaxDuration.Duration())
		defer cancel()
	}

	tracker, err := newVictimTracker(runCtx, m.index)
	if err != nil {
		return GCResult{}, err
	}
	selected := make(map[int64]bool)
	pinnedOver := usage.BudgetBytes > 0 && usage.PinnedBytes > usage.BudgetBytes
	m.setPinnedOver(pinnedOver)
	result.PinnedOver = pinnedOver

	matcher, err := m.loadPins(runCtx)
	if err != nil {
		return GCResult{}, err
	}

	if err := m.tierFiles(runCtx, cfg, now, opts.DryRun, &result.Tiers[0]); err != nil {
		return GCResult{}, err
	}
	m.reportProgress(opts, &result, 0)
	if runCtx.Err() != nil {
		result.Stopped = stopReason(runCtx)
		return m.finish(ctx, &result, opts, usage, target, targetSet)
	}

	history, err := m.index.HistoryCandidates(runCtx, "", cfg.Cache.MetadataHistory+1)
	if err != nil {
		return GCResult{}, err
	}
	history = filterPinned(history, matcher)
	if err := m.applyCandidates(runCtx, &result.Tiers[1], history, tracker, opts.DryRun, selected, "history"); err != nil {
		return GCResult{}, err
	}
	zeroRef, err := m.index.ZeroRefBlobs(runCtx, 0)
	if err != nil {
		return GCResult{}, err
	}
	m.applyBlobsIndex(runCtx, &result.Tiers[1], zeroRef, opts.DryRun)
	m.reportProgress(opts, &result, 1)
	if runCtx.Err() != nil {
		result.Stopped = stopReason(runCtx)
		return m.finish(ctx, &result, opts, usage, target, targetSet)
	}

	used := usage.UsedBytes
	used -= result.Tiers[0].Bytes + result.Tiers[1].Bytes
	if used < 0 {
		used = 0
	}

	if pinnedOver {
		result.Stopped = "pinned_over_budget"
		return m.finish(ctx, &result, opts, usage, target, targetSet)
	}

	if err := m.tierQuota(runCtx, cfg, now, policy, opts.DryRun, matcher, tracker, selected, &result.Tiers[2], &used); err != nil {
		return GCResult{}, err
	}
	m.reportProgress(opts, &result, 2)
	if runCtx.Err() != nil {
		result.Stopped = stopReason(runCtx)
		return m.finish(ctx, &result, opts, usage, target, targetSet)
	}

	immutable, err := m.index.EvictionCandidates(runCtx, "immutable", time.Time{}, 0)
	if err != nil {
		return GCResult{}, err
	}
	immutable = filterUnselected(filterPinned(immutable, matcher), selected)
	cutoff := now.AddDate(0, 0, -cfg.Storage.ProtectRecentDays)
	unusedCutoff := unusedCutoffs(cfg, immutable, now)
	var tier3, tier4 []index.Candidate
	for _, candidate := range immutable {
		if accessedAt(candidate).Before(cutoff) {
			tier3 = append(tier3, candidate)
		} else {
			tier4 = append(tier4, candidate)
		}
	}
	sortCandidates(tier3, policy, cfg, now)
	sortCandidates(tier4, policy, cfg, now)
	tier3, used = m.evictUntil(runCtx, &result.Tiers[3], tier3, tracker, opts.DryRun, selected, cfg, now, unusedCutoff, used, target, targetSet)
	m.reportProgress(opts, &result, 3)
	if runCtx.Err() != nil {
		result.Stopped = stopReason(runCtx)
		return m.finish(ctx, &result, opts, usage, target, targetSet)
	}
	tier4, used = m.evictUntil(runCtx, &result.Tiers[4], tier4, tracker, opts.DryRun, selected, cfg, now, unusedCutoff, used, target, targetSet)
	m.reportProgress(opts, &result, 4)
	if runCtx.Err() != nil {
		result.Stopped = stopReason(runCtx)
		return m.finish(ctx, &result, opts, usage, target, targetSet)
	}

	if targetSet && used > target && cfg.Storage.MetadataUnusedDays > 0 {
		mutable, err := m.index.EvictionCandidates(runCtx, "mutable", time.Time{}, 0)
		if err != nil {
			return GCResult{}, err
		}
		mutable = filterUnselected(filterPinned(mutable, matcher), selected)
		metadataCutoff := now.AddDate(0, 0, -cfg.Storage.MetadataUnusedDays)
		var tier5 []index.Candidate
		for _, candidate := range mutable {
			if accessedAt(candidate).Before(metadataCutoff) {
				tier5 = append(tier5, candidate)
			}
		}
		sortCandidates(tier5, policy, cfg, now)
		_, used = m.evictUntil(runCtx, &result.Tiers[5], tier5, tracker, opts.DryRun, selected, cfg, now, nil, used, target, targetSet)
		m.reportProgress(opts, &result, 5)
	}
	if runCtx.Err() != nil {
		result.Stopped = stopReason(runCtx)
	}
	return m.finish(ctx, &result, opts, usage, target, targetSet)
}

func (m *Manager) gcTarget(cfg *config.Config, usage Usage, opts GCOptions) (int64, bool) {
	if opts.HasTarget {
		return opts.TargetBytes, true
	}
	if opts.TargetPct > 0 && usage.BudgetBytes > 0 {
		pct := opts.TargetPct
		if pct > 1 {
			pct /= 100
		}
		return int64(pct * float64(usage.BudgetBytes)), true
	}
	if usage.BudgetBytes > 0 &&
		float64(usage.UsedBytes) > cfg.Storage.HighWatermark*float64(usage.BudgetBytes) {
		return int64(cfg.Storage.LowWatermark * float64(usage.BudgetBytes)), true
	}
	return usage.UsedBytes, false
}

func (m *Manager) finish(ctx context.Context, result *GCResult, opts GCOptions, before Usage, target int64, targetSet bool) (GCResult, error) {
	if !opts.DryRun {
		if err := m.index.Checkpoint(context.WithoutCancel(ctx)); err != nil {
			m.logger.Warn("index_checkpoint_failed", "error", err)
		}
		m.invalidateUsage()
		after, err := m.Usage(context.WithoutCancel(ctx), true)
		if err != nil {
			return GCResult{}, err
		}
		result.UsedAfter = after.UsedBytes
		if targetSet && after.UsedBytes > target && result.Stopped == "" {
			result.Stopped = "target_unmet"
		}
		if targetSet && result.Stopped == "target_unmet" && after.BudgetBytes > 0 {
			m.enterBypass("budget_unmet")
		}
	} else {
		result.UsedAfter = result.UsedBefore
		for _, tier := range result.Tiers {
			result.UsedAfter -= tier.Bytes
		}
		if result.UsedAfter < 0 {
			result.UsedAfter = 0
		}
	}
	result.TargetReached = !targetSet || result.UsedAfter <= target
	result.Bypass = m.bypass.Load()

	if m.metrics != nil && !opts.DryRun {
		m.metrics.GCRuns.With().Inc()
		for _, tier := range result.Tiers {
			label := strconv.Itoa(tier.Tier)
			m.metrics.GCEvictedBytes.With(label).Add(float64(tier.Bytes))
			m.metrics.GCEvictedObjects.With(label).Add(float64(tier.Objects))
		}
	}
	totalObjects, totalBytes := int64(0), int64(0)
	for _, tier := range result.Tiers {
		totalObjects += tier.Objects
		totalBytes += tier.Bytes
	}
	m.logger.Info("gc_done", "trigger", result.Trigger, "dry_run", result.DryRun,
		"evicted_objects", totalObjects, "evicted_bytes", totalBytes,
		"used_before", result.UsedBefore, "used_after", result.UsedAfter,
		"stopped", result.Stopped)
	return *result, nil
}

func stopReason(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "max_duration"
	}
	return "cancelled"
}

func (m *Manager) reportProgress(opts GCOptions, result *GCResult, tier int) {
	if opts.Progress == nil {
		return
	}
	var items, bytes int64
	for _, entry := range result.Tiers {
		items += entry.Objects
		bytes += entry.Bytes
	}
	opts.Progress(Progress{Items: items, Bytes: bytes, Message: "tier " + strconv.Itoa(tier)})
}

func (m *Manager) tierFiles(ctx context.Context, cfg *config.Config, now time.Time, dry bool, tier *TierResult) error {
	tmpCutoff := now.Add(-time.Hour)
	if err := m.tierDirectory(ctx, filepath.Join(cfg.DataDir, "tmp"), tmpCutoff, dry, tier); err != nil {
		return err
	}
	quarantineCutoff := now.AddDate(0, 0, -cfg.Storage.QuarantineKeepDays)
	if err := m.tierDirectory(ctx, filepath.Join(cfg.DataDir, "quarantine"), quarantineCutoff, dry, tier); err != nil {
		return err
	}
	snapshots, err := readFiles(filepath.Join(cfg.DataDir, "snapshots"))
	if err != nil {
		return err
	}
	filteredSnapshots := snapshots[:0]
	for _, snapshot := range snapshots {
		if strings.HasPrefix(snapshot.name, ".") {
			continue
		}
		filteredSnapshots = append(filteredSnapshots, snapshot)
	}
	snapshots = filteredSnapshots
	sort.Slice(snapshots, func(i, j int) bool {
		if !snapshots[i].modTime.Equal(snapshots[j].modTime) {
			return snapshots[i].modTime.Before(snapshots[j].modTime)
		}
		return snapshots[i].name < snapshots[j].name
	})
	if cfg.Storage.SnapshotKeep >= 0 && len(snapshots) > cfg.Storage.SnapshotKeep {
		remove := len(snapshots) - cfg.Storage.SnapshotKeep
		for _, file := range snapshots[:remove] {
			if err := ctx.Err(); err != nil {
				return nil
			}
			if !dry {
				if err := os.Remove(filepath.Join(cfg.DataDir, "snapshots", file.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("remove snapshot %s: %w", file.name, err)
				}
			}
			tier.Objects++
			tier.Bytes += file.size
			tier.Victims = append(tier.Victims, Victim{
				Kind: "snapshot", Path: file.name, Size: file.size, Freed: file.size,
			})
		}
	}
	return nil
}

type fileEntry struct {
	name    string
	size    int64
	modTime time.Time
}

func readFiles(dir string) ([]fileEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	out := make([]fileEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", entry.Name(), err)
		}
		out = append(out, fileEntry{name: entry.Name(), size: info.Size(), modTime: info.ModTime()})
	}
	return out, nil
}

func (m *Manager) tierDirectory(ctx context.Context, dir string, olderThan time.Time, dry bool, tier *TierResult) error {
	files, err := readFiles(dir)
	if err != nil {
		return err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.Before(files[j].modTime) })
	for _, file := range files {
		if !file.modTime.Before(olderThan) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil
		}
		if !dry {
			if err := os.Remove(filepath.Join(dir, file.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove %s: %w", file.name, err)
			}
		}
		tier.Objects++
		tier.Bytes += file.size
		kind := filepath.Base(dir)
		tier.Victims = append(tier.Victims, Victim{
			Kind: kind, Path: file.name, Size: file.size, Freed: file.size,
		})
	}
	return nil
}

func (m *Manager) applyCandidates(ctx context.Context, tier *TierResult, candidates []index.Candidate, tracker *victimTracker, dry bool, selected map[int64]bool, kind string) error {
	for start := 0; start < len(candidates); start += deleteBatchSize {
		if err := ctx.Err(); err != nil {
			return nil
		}
		end := start + deleteBatchSize
		if end > len(candidates) {
			end = len(candidates)
		}
		batch := candidates[start:end]
		if m.testBeforeBatch != nil {
			m.testBeforeBatch()
		}
		freed := int64(0)
		var ids []int64
		for _, candidate := range batch {
			if selected[candidate.ID] {
				continue
			}
			selected[candidate.ID] = true
			claimed := tracker.claim(candidate)
			freed += claimed
			ids = append(ids, candidate.ID)
			tier.Victims = append(tier.Victims, Victim{
				Repo: candidate.Repo, Key: candidate.Key, Generation: candidate.Generation,
				Kind: kind, Size: candidate.StoredSize, Freed: claimed,
			})
			tier.Objects++
		}
		if dry || len(ids) == 0 {
			tier.Bytes += freed
			continue
		}
		actual := int64(0)
		err := m.store.Reclaim(func() error {
			blobs, err := m.index.DeleteObjects(ctx, ids)
			if err != nil {
				return err
			}
			for _, blob := range blobs {
				if err := m.store.Delete(blob.SHA256); err != nil {
					m.logger.Warn("blob_unlink_failed", "sha256", blob.SHA256, "error", err)
				}
				actual += blob.StoredSize
			}
			return nil
		})
		if err != nil {
			return err
		}
		tier.Bytes += actual
	}
	return nil
}

func (m *Manager) applyBlobsIndex(ctx context.Context, tier *TierResult, blobs []index.Blob, dry bool) {
	if len(blobs) == 0 {
		return
	}
	for start := 0; start < len(blobs); start += blobBatchSize {
		end := start + blobBatchSize
		if end > len(blobs) {
			end = len(blobs)
		}
		batch := blobs[start:end]
		if dry {
			for _, blob := range batch {
				tier.Objects++
				tier.Bytes += blob.StoredSize
				tier.Victims = append(tier.Victims, Victim{
					Kind: "blob", Path: blob.SHA256, Size: blob.StoredSize, Freed: blob.StoredSize,
				})
			}
			continue
		}
		shas := make([]string, 0, len(batch))
		for _, blob := range batch {
			shas = append(shas, blob.SHA256)
		}
		var freed []index.FreedBlob
		err := m.store.Reclaim(func() error {
			var err error
			freed, err = m.index.DeleteBlobRows(ctx, shas)
			if err != nil {
				return err
			}
			for _, blob := range freed {
				if err := m.store.Delete(blob.SHA256); err != nil {
					m.logger.Warn("blob_unlink_failed", "sha256", blob.SHA256, "error", err)
				}
			}
			return nil
		})
		if err != nil {
			m.logger.Warn("blob_row_delete_failed", "error", err)
			return
		}
		for _, blob := range freed {
			tier.Objects++
			tier.Bytes += blob.StoredSize
			tier.Victims = append(tier.Victims, Victim{
				Kind: "blob", Path: blob.SHA256, Size: blob.StoredSize, Freed: blob.StoredSize,
			})
		}
	}
}

func filterPinned(candidates []index.Candidate, matcher *pinMatcher) []index.Candidate {
	out := candidates[:0]
	for _, candidate := range candidates {
		if matcher.pinned(candidate.Repo, candidate.Key) {
			continue
		}
		out = append(out, candidate)
	}
	return out
}

func filterUnselected(candidates []index.Candidate, selected map[int64]bool) []index.Candidate {
	out := candidates[:0]
	for _, candidate := range candidates {
		if selected[candidate.ID] {
			continue
		}
		out = append(out, candidate)
	}
	return out
}

func accessedAt(candidate index.Candidate) time.Time {
	if candidate.LastAccess.IsZero() {
		return candidate.FetchedAt
	}
	return candidate.LastAccess
}

func unusedCutoffs(cfg *config.Config, candidates []index.Candidate, now time.Time) map[string]time.Time {
	out := make(map[string]time.Time)
	for i := range cfg.Repos {
		if days := cfg.Repos[i].UnusedDays; days > 0 {
			out[cfg.Repos[i].Name] = now.AddDate(0, 0, -days)
		}
	}
	return out
}

func (m *Manager) evictUntil(ctx context.Context, tier *TierResult, candidates []index.Candidate, tracker *victimTracker, dry bool, selected map[int64]bool, cfg *config.Config, now time.Time, unusedCutoff map[string]time.Time, used, target int64, targetSet bool) ([]index.Candidate, int64) {
	var chosen []index.Candidate
	remaining := make(map[string]int64, len(tracker.refcounts))
	for sha, count := range tracker.refcounts {
		remaining[sha] = count
	}
	for _, candidate := range candidates {
		due := false
		if cutoff, ok := unusedCutoff[candidate.Repo]; ok && accessedAt(candidate).Before(cutoff) {
			due = true
		}
		if targetSet && used > target {
			due = true
		}
		if !due {
			continue
		}
		chosen = append(chosen, candidate)
		if remaining[candidate.SHA256] > 0 {
			remaining[candidate.SHA256]--
			if remaining[candidate.SHA256] == 0 {
				used -= candidate.StoredSize
			}
		}
	}
	if err := m.applyCandidates(ctx, tier, chosen, tracker, dry, selected, "object"); err != nil {
		m.logger.Warn("gc_tier_failed", "tier", tier.Tier, "error", err)
	}
	return chosen, used
}

func (m *Manager) tierQuota(ctx context.Context, cfg *config.Config, now time.Time, policy string, dry bool, matcher *pinMatcher, tracker *victimTracker, selected map[int64]bool, tier *TierResult, used *int64) error {
	quotas := make(map[string]int64)
	for i := range cfg.Repos {
		if cfg.Repos[i].QuotaBytes > 0 {
			quotas[cfg.Repos[i].Name] = cfg.Repos[i].QuotaBytes.Bytes()
		}
	}
	if len(quotas) == 0 {
		return nil
	}
	totals, err := m.index.RepoClassTotals(ctx)
	if err != nil {
		return err
	}
	current := make(map[string]int64)
	for _, total := range totals {
		if total.Class == "immutable" {
			current[total.Repo] = total.StoredBytes
		}
	}
	repos := make([]string, 0, len(quotas))
	for repo := range quotas {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	candidates, err := m.index.EvictionCandidates(ctx, "immutable", time.Time{}, 0)
	if err != nil {
		return err
	}
	for _, repo := range repos {
		if current[repo] <= quotas[repo] {
			continue
		}
		var pool []index.Candidate
		for _, candidate := range candidates {
			if candidate.Repo == repo && !selected[candidate.ID] && !matcher.pinned(candidate.Repo, candidate.Key) {
				pool = append(pool, candidate)
			}
		}
		sortCandidates(pool, policy, cfg, now)
		var chosen []index.Candidate
		seenBlob := make(map[string]bool)
		for _, candidate := range pool {
			if current[repo] <= quotas[repo] {
				break
			}
			chosen = append(chosen, candidate)
			if !seenBlob[candidate.SHA256] {
				seenBlob[candidate.SHA256] = true
				current[repo] -= candidate.StoredSize
				*used -= candidate.StoredSize
			}
		}
		if err := m.applyCandidates(ctx, tier, chosen, tracker, dry, selected, "quota"); err != nil {
			return err
		}
	}
	return nil
}

func sortCandidates(candidates []index.Candidate, policy string, cfg *config.Config, now time.Time) {
	weights := make(map[string]float64, len(cfg.Repos))
	for i := range cfg.Repos {
		weight := cfg.Repos[i].EvictWeight
		if weight <= 0 {
			weight = 1
		}
		weights[cfg.Repos[i].Name] = weight
	}
	switch policy {
	case "lfu":
		sort.SliceStable(candidates, func(i, j int) bool {
			if candidates[i].Hits != candidates[j].Hits {
				return candidates[i].Hits < candidates[j].Hits
			}
			if !accessedAt(candidates[i]).Equal(accessedAt(candidates[j])) {
				return accessedAt(candidates[i]).Before(accessedAt(candidates[j]))
			}
			return candidates[i].ID < candidates[j].ID
		})
	case "score":
		score := func(candidate index.Candidate) float64 {
			idleDays := now.Sub(accessedAt(candidate)).Hours() / 24
			if idleDays < 0 {
				idleDays = 0
			}
			sizeMiB := float64(candidate.StoredSize) / (1024 * 1024)
			weight := weights[candidate.Repo]
			if weight <= 0 {
				weight = 1
			}
			return idleDays * sizeMiB * weight / (1 + math.Log1p(float64(candidate.Hits)))
		}
		sort.SliceStable(candidates, func(i, j int) bool {
			left, right := score(candidates[i]), score(candidates[j])
			if left != right {
				return left > right
			}
			return candidates[i].ID < candidates[j].ID
		})
	default:
		sort.SliceStable(candidates, func(i, j int) bool {
			if !accessedAt(candidates[i]).Equal(accessedAt(candidates[j])) {
				return accessedAt(candidates[i]).Before(accessedAt(candidates[j]))
			}
			return candidates[i].ID < candidates[j].ID
		})
	}
}
