package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/metrics"
	"github.com/mmrzaf/larder/internal/store"
	"github.com/mmrzaf/larder/testutil"
)

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type fixture struct {
	m       *Manager
	store   *store.Store
	index   *index.DB
	clock   *testutil.FakeClock
	cfg     *config.Config
	metrics *metrics.Metrics
	logs    *safeBuffer
	dir     string
	free    atomic.Int64
	cap     atomic.Int64
	next    atomic.Int64
}

func newFixture(t *testing.T, mutate func(*config.Config)) *fixture {
	t.Helper()
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()
	cfg.Storage.MaxBytes = config.Size(1 << 40)
	cfg.Storage.MinFreeBytes = config.Size(5 << 30)
	cfg.Storage.HardFreeBytes = config.Size(1 << 30)
	cfg.Storage.HighWatermark = 0.90
	cfg.Storage.LowWatermark = 0.75
	cfg.Storage.ProtectRecentDays = 14
	cfg.Storage.MetadataUnusedDays = 365
	cfg.Storage.QuarantineKeepDays = 30
	cfg.Storage.SnapshotKeep = 7
	cfg.Repos = []config.Repo{
		{Name: "repo", Kind: "generic", Mount: "/repo"},
		{Name: "other", Kind: "generic", Mount: "/other"},
	}
	if mutate != nil {
		mutate(cfg)
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := index.Open(cfg.DataDir + "/index.db")
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	m, err := metrics.New("test")
	if err != nil {
		t.Fatal(err)
	}
	logs := &safeBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f := &fixture{
		store: st, index: idx, clock: testutil.NewFakeClock(time.Unix(1_700_000_000, 0)),
		cfg: cfg, metrics: m, logs: logs, dir: cfg.DataDir,
	}
	f.free.Store(20 << 30)
	f.cap.Store(100 << 30)
	f.m, err = New(Options{
		Config: cfg, Store: st, Index: idx, Metrics: m, Clock: f.clock, Logger: logger,
		Capacity:  func() (uint64, error) { return uint64(f.cap.Load()), nil },
		FreeSpace: func() (uint64, error) { return uint64(f.free.Load()), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = m.WritePrometheus(io.Discard)
		_ = idx.Close()
		_ = st.Close()
	})
	return f
}

type placed struct {
	ID   int64
	SHA  string
	Size int64
}

func (f *fixture) put(t *testing.T, repo, key, class string, size int64, access time.Time, hits int64) placed {
	t.Helper()
	sha := f.uniqueSHA()
	f.putBlob(t, sha, size)
	return f.attach(t, repo, key, class, size, access, hits, sha)
}

func (f *fixture) putBlob(t *testing.T, sha string, size int64) {
	t.Helper()
	if err := f.index.PutBlob(context.Background(), index.Blob{
		SHA256: sha, RawSize: size, StoredSize: size, StoredEnc: "identity", CreatedAt: accessBase,
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) attach(t *testing.T, repo, key, class string, size int64, access time.Time, hits int64, sha string) placed {
	t.Helper()
	object, err := f.index.PutObject(context.Background(), index.PutObjectParams{
		Repo: repo, Key: key, Class: class, SHA256: sha,
		FetchedAt: access, ValidatedAt: access,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.index.SetAccess(context.Background(), object.ID, access, hits); err != nil {
		t.Fatal(err)
	}
	return placed{ID: object.ID, SHA: sha, Size: size}
}

func (f *fixture) uniqueSHA() string {
	n := f.next.Add(1)
	sum := sha256.Sum256([]byte(fmt.Sprintf("fixture-%d", n)))
	return hex.EncodeToString(sum[:])
}

var accessBase = time.Unix(1_700_000_000, 0)

func (f *fixture) usage(t *testing.T) Usage {
	t.Helper()
	usage, err := f.m.Usage(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	return usage
}

func (f *fixture) gc(t *testing.T, opts GCOptions) GCResult {
	t.Helper()
	result, err := f.m.GC(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func keysOf(victims []Victim) []string {
	out := make([]string, 0, len(victims))
	for _, victim := range victims {
		out = append(out, victim.Key)
	}
	return out
}

func TestMatrix_S01_UsageAccounting(t *testing.T) {
	f := newFixture(t, nil)
	now := f.clock.Now()
	blob := f.uniqueSHA()
	f.putBlob(t, blob, 100)
	f.attach(t, "repo", "/one", "immutable", 100, now, 0, blob)
	f.attach(t, "repo", "/two", "immutable", 100, now, 0, blob)

	usage := f.usage(t)
	if usage.BlobBytes != 100 {
		t.Fatalf("blob bytes = %d, want 100", usage.BlobBytes)
	}
	if usage.UsedBytes != usage.BlobBytes+usage.IndexBytes+usage.SnapshotBytes {
		t.Fatalf("used = %d, parts = %d + %d + %d", usage.UsedBytes, usage.BlobBytes, usage.IndexBytes, usage.SnapshotBytes)
	}
	if usage.Objects != 2 {
		t.Fatalf("objects = %d, want 2", usage.Objects)
	}
}

func TestMatrix_S02_HighWatermarkTierOrder(t *testing.T) {
	f := newFixture(t, func(cfg *config.Config) {
		cfg.Storage.ProtectRecentDays = 14
	})
	now := f.clock.Now()
	mib := int64(1 << 20)
	f.put(t, "repo", "/old-1", "immutable", mib, now.AddDate(0, 0, -40), 0)
	f.put(t, "repo", "/old-2", "immutable", mib, now.AddDate(0, 0, -30), 0)
	f.put(t, "repo", "/recent-1", "immutable", mib, now.AddDate(0, 0, -2), 0)
	f.put(t, "repo", "/recent-2", "immutable", mib, now.AddDate(0, 0, -1), 0)
	usage := f.usage(t)
	f.cfg.Storage.MaxBytes = config.Size(usage.UsedBytes - 2*mib)
	result := f.gc(t, GCOptions{TargetBytes: -1, HasTarget: false})

	if !result.TargetReached {
		t.Fatalf("target not reached: %+v", result)
	}
	if got := keysOf(result.Tiers[3].Victims); len(got) != 2 || got[0] != "/old-1" || got[1] != "/old-2" {
		t.Fatalf("tier 3 victims = %#v", result.Tiers[3].Victims)
	}
	if got := keysOf(result.Tiers[4].Victims); len(got) != 1 || got[0] != "/recent-1" {
		t.Fatalf("tier 4 victims = %#v", result.Tiers[4].Victims)
	}
	if result.UsedAfter > int64(f.cfg.Storage.LowWatermark*float64(result.BudgetBytes)) {
		t.Fatalf("used after %d above low watermark of %d", result.UsedAfter, result.BudgetBytes)
	}
}

func TestMatrix_S03_PinnedNeverEvicted(t *testing.T) {
	f := newFixture(t, nil)
	now := f.clock.Now()
	mib := int64(1 << 20)
	f.put(t, "repo", "/keep/item", "immutable", mib, now.AddDate(0, 0, -30), 0)
	f.put(t, "repo", "/drop", "immutable", mib, now.AddDate(0, 0, -30), 0)
	if err := f.m.AddPin(context.Background(), AddPinRequest{Set: "baseline", Repo: "repo", Match: "keep/**"}); err != nil {
		t.Fatal(err)
	}
	result := f.gc(t, GCOptions{TargetBytes: 0, HasTarget: true})
	if len(result.Tiers[3].Victims) != 1 || result.Tiers[3].Victims[0].Key != "/drop" {
		t.Fatalf("tier 3 victims = %#v", result.Tiers[3].Victims)
	}
	current, err := f.index.CurrentObject(context.Background(), "repo", "/keep/item")
	if err != nil || current.Key != "/keep/item" {
		t.Fatalf("pinned object missing: %v", err)
	}

	evicted, err := f.m.Evict(context.Background(), EvictOptions{Repo: "repo", Glob: "keep/**"})
	if err != nil {
		t.Fatal(err)
	}
	if evicted.Objects != 0 {
		t.Fatalf("evict removed pinned objects: %+v", evicted)
	}
	evicted, err = f.m.Evict(context.Background(), EvictOptions{Repo: "repo", Glob: "keep/**", IncludePinned: true})
	if err != nil {
		t.Fatal(err)
	}
	if evicted.Objects != 1 {
		t.Fatalf("evict with include-pinned removed %d objects", evicted.Objects)
	}
}

func TestMatrix_S04_PerRepoQuota(t *testing.T) {
	f := newFixture(t, func(cfg *config.Config) {
		cfg.Repos[0].QuotaBytes = config.Size(5 << 20)
	})
	now := f.clock.Now()
	mib := int64(1 << 20)
	var keys []string
	for i := 0; i < 4; i++ {
		key := fmt.Sprintf("/quota-%d", i)
		keys = append(keys, key)
		f.put(t, "repo", key, "immutable", 2*mib, now.Add(-time.Duration(4-i)*time.Hour), 0)
	}
	f.put(t, "other", "/other", "immutable", 3*mib, now.AddDate(0, 0, -30), 0)
	result := f.gc(t, GCOptions{TargetBytes: -1})

	if len(result.Tiers[2].Victims) != 2 {
		t.Fatalf("tier 2 removed %d objects: %#v", len(result.Tiers[2].Victims), result.Tiers[2].Victims)
	}
	if result.Tiers[2].Victims[0].Key != keys[0] || result.Tiers[2].Victims[1].Key != keys[1] {
		t.Fatalf("quota order = %#v", keysOf(result.Tiers[2].Victims))
	}
	if _, err := f.index.CurrentObject(context.Background(), "other", "/other"); err != nil {
		t.Fatal("unrelated repo was trimmed")
	}
}

func TestMatrix_S05_Policies(t *testing.T) {
	mib := int64(1 << 20)
	now := func(f *fixture) time.Time { return f.clock.Now() }
	setup := func(t *testing.T, accesses []time.Duration, sizes []int64, hits []int64) *fixture {
		f := newFixture(t, func(cfg *config.Config) { cfg.Storage.ProtectRecentDays = 0 })
		for i := range accesses {
			f.put(t, "repo", fmt.Sprintf("/object-%d", i), "immutable", sizes[i], now(f).Add(-accesses[i]), hits[i])
		}
		return f
	}
	cases := []struct {
		name     string
		policy   string
		accesses []time.Duration
		sizes    []int64
		hits     []int64
		want     string
	}{
		{
			name: "lru", policy: "lru",
			accesses: []time.Duration{100 * 24 * time.Hour, 50 * 24 * time.Hour, time.Hour},
			sizes:    []int64{mib, mib, mib}, hits: []int64{0, 0, 0},
			want: "/object-0",
		},
		{
			name: "lfu", policy: "lfu",
			accesses: []time.Duration{100 * 24 * time.Hour, 50 * 24 * time.Hour, time.Hour},
			sizes:    []int64{mib, mib, mib}, hits: []int64{5, 0, 2},
			want: "/object-1",
		},
		{
			name: "score", policy: "score",
			accesses: []time.Duration{100 * 24 * time.Hour, 200 * 24 * time.Hour, time.Hour},
			sizes:    []int64{mib, 4 * mib, 2 * mib}, hits: []int64{0, 0, 0},
			want: "/object-1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, tc.accesses, tc.sizes, tc.hits)
			usage := f.usage(t)
			result := f.gc(t, GCOptions{HasTarget: true, TargetBytes: usage.UsedBytes - 1, Policy: tc.policy})
			victims := result.Tiers[3].Victims
			if len(victims) != 1 || victims[0].Key != tc.want {
				t.Fatalf("%s victims = %#v, want %s", tc.policy, keysOf(victims), tc.want)
			}
		})
	}
}

func TestMatrix_S06_MetadataProtection(t *testing.T) {
	f := newFixture(t, func(cfg *config.Config) {
		cfg.Storage.ProtectRecentDays = 0
		cfg.Storage.MetadataUnusedDays = 365
	})
	now := f.clock.Now()
	mib := int64(1 << 20)
	f.put(t, "repo", "/immutable", "immutable", 4*mib, now.AddDate(0, 0, -10), 0)
	f.put(t, "repo", "/meta/current", "mutable", 4*mib, now.AddDate(0, 0, -400), 0)
	usage := f.usage(t)
	result := f.gc(t, GCOptions{HasTarget: true, TargetBytes: usage.UsedBytes - 4*mib})
	if len(result.Tiers[3].Victims) != 1 {
		t.Fatalf("tier 3 victims = %#v", result.Tiers[3].Victims)
	}
	if len(result.Tiers[5].Victims) != 0 {
		t.Fatalf("mutable object evicted before tier 5: %#v", result.Tiers[5].Victims)
	}
	if _, err := f.index.CurrentObject(context.Background(), "repo", "/meta/current"); err != nil {
		t.Fatal("mutable object removed early")
	}

	result = f.gc(t, GCOptions{HasTarget: true, TargetBytes: 0})
	if len(result.Tiers[5].Victims) != 1 || result.Tiers[5].Victims[0].Key != "/meta/current" {
		t.Fatalf("tier 5 victims = %#v", result.Tiers[5].Victims)
	}
}

func TestMatrix_S07_HistoryPruning(t *testing.T) {
	f := newFixture(t, func(cfg *config.Config) { cfg.Cache.MetadataHistory = 2 })
	now := f.clock.Now()
	for i := 0; i < 4; i++ {
		f.put(t, "repo", "/meta", "mutable", int64(1024+i), now.Add(time.Duration(i)*time.Minute), 0)
	}
	result := f.gc(t, GCOptions{TargetBytes: -1})
	if len(result.Tiers[1].Victims) != 1 {
		t.Fatalf("history victims = %#v", result.Tiers[1].Victims)
	}
	generations, err := f.index.Generations(context.Background(), "repo", "/meta")
	if err != nil {
		t.Fatal(err)
	}
	if len(generations) != 3 || !generations[0].Current || generations[0].Generation != 4 {
		t.Fatalf("generations = %#v", generations)
	}
}

func TestMatrix_S08_DryRun(t *testing.T) {
	f := newFixture(t, func(cfg *config.Config) { cfg.Storage.ProtectRecentDays = 0 })
	now := f.clock.Now()
	mib := int64(1 << 20)
	for i := 0; i < 3; i++ {
		f.put(t, "repo", fmt.Sprintf("/dry-%d", i), "immutable", mib, now.AddDate(0, 0, -30), 0)
	}
	before := f.usage(t)
	result := f.gc(t, GCOptions{HasTarget: true, TargetBytes: 0, DryRun: true})
	after := f.usage(t)

	if !result.DryRun || len(result.Tiers[3].Victims) != 3 {
		t.Fatalf("dry-run victims = %#v", result.Tiers[3].Victims)
	}
	if result.Tiers[3].Bytes != 3*mib {
		t.Fatalf("dry-run bytes = %d", result.Tiers[3].Bytes)
	}
	if before.UsedBytes != after.UsedBytes {
		t.Fatalf("dry-run changed usage: %d -> %d", before.UsedBytes, after.UsedBytes)
	}
	if result.UsedAfter != before.UsedBytes-3*mib {
		t.Fatalf("predicted used = %d, want %d", result.UsedAfter, before.UsedBytes-3*mib)
	}
	current, err := f.index.CurrentObjects(context.Background(), "repo")
	if err != nil || len(current) != 3 {
		t.Fatalf("dry-run removed objects: %d, %v", len(current), err)
	}
}

func TestMatrix_S09_TargetBytes(t *testing.T) {
	f := newFixture(t, func(cfg *config.Config) { cfg.Storage.ProtectRecentDays = 0 })
	now := f.clock.Now()
	mib := int64(1 << 20)
	for i := 0; i < 4; i++ {
		f.put(t, "repo", fmt.Sprintf("/target-%d", i), "immutable", mib, now.Add(-time.Duration(4-i)*time.Hour), 0)
	}
	before := f.usage(t)
	target := before.UsedBytes - mib
	result := f.gc(t, GCOptions{HasTarget: true, TargetBytes: target})
	if len(result.Tiers[3].Victims) != 1 || result.Tiers[3].Victims[0].Key != "/target-0" {
		t.Fatalf("target victims = %#v", result.Tiers[3].Victims)
	}
	if result.UsedAfter > target {
		t.Fatalf("used after %d above target %d", result.UsedAfter, target)
	}
	if !result.TargetReached {
		t.Fatal("target not reported reached")
	}
}

func TestMatrix_S10_EvictFilters(t *testing.T) {
	f := newFixture(t, nil)
	now := f.clock.Now()
	mib := int64(1 << 20)
	f.put(t, "repo", "/foo/a", "immutable", mib, now.AddDate(0, 0, -100), 0)
	f.put(t, "repo", "/foo/b", "immutable", 2*mib, now.AddDate(0, 0, -100), 0)
	f.put(t, "repo", "/foo/c", "immutable", mib, now, 0)
	f.put(t, "other", "/foo/a", "immutable", mib, now.AddDate(0, 0, -100), 0)
	if err := f.m.AddPin(context.Background(), AddPinRequest{Set: "keep", Repo: "repo", Key: "/foo/b"}); err != nil {
		t.Fatal(err)
	}
	result, err := f.m.Evict(context.Background(), EvictOptions{
		Repo: "repo", Glob: "foo/**", OlderThanDays: 10, DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Objects != 1 || result.Bytes != mib || len(result.Victims) != 1 || result.Victims[0].Key != "/foo/a" {
		t.Fatalf("dry evict = %+v", result)
	}
	if _, err := f.index.CurrentObject(context.Background(), "repo", "/foo/a"); err != nil {
		t.Fatal("dry evict removed an object")
	}
	result, err = f.m.Evict(context.Background(), EvictOptions{
		Repo: "repo", Glob: "foo/**", OlderThanDays: 10, LargerThan: mib, IncludePinned: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Objects != 1 || result.Bytes != 2*mib {
		t.Fatalf("size filter evict = %+v", result)
	}
	if _, err := f.index.CurrentObject(context.Background(), "other", "/foo/a"); err != nil {
		t.Fatal("evict crossed repositories")
	}
}

func TestMatrix_S11_MinFreeBudget(t *testing.T) {
	f := newFixture(t, func(cfg *config.Config) {
		cfg.Storage.MaxBytes = 0
		cfg.Storage.MinFreeBytes = config.Size(30 << 20)
	})
	f.cap.Store(100 << 20)
	if usage := f.usage(t); usage.BudgetBytes != 70<<20 {
		t.Fatalf("budget = %d, want %d", usage.BudgetBytes, 70<<20)
	}
	f.cfg.Storage.MaxBytes = config.Size(50 << 20)
	if usage := f.usage(t); usage.BudgetBytes != 50<<20 {
		t.Fatalf("budget = %d, want %d", usage.BudgetBytes, 50<<20)
	}
	f.cap.Store(20 << 20)
	if usage := f.usage(t); usage.BudgetBytes != 0 {
		t.Fatalf("budget = %d, want 0", usage.BudgetBytes)
	}
}

func TestMatrix_S12_BypassEnterExit(t *testing.T) {
	f := newFixture(t, func(cfg *config.Config) {
		cfg.Storage.MaxBytes = config.Size(100 << 20)
		cfg.Storage.HardFreeBytes = config.Size(1 << 20)
	})
	if _, err := f.m.Usage(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	f.free.Store(512 << 10)
	f.clock.Advance(2 * time.Second)
	if !f.m.Bypass("repo") {
		t.Fatal("bypass did not activate below the hard floor")
	}
	if value := f.metrics.BypassActive.With().Value(); value != 1 {
		t.Fatalf("bypass metric = %v", value)
	}
	f.free.Store(20 << 30)
	f.clock.Advance(2 * time.Second)
	if f.m.Bypass("repo") {
		t.Fatal("bypass did not clear")
	}
	if value := f.metrics.BypassActive.With().Value(); value != 0 {
		t.Fatalf("bypass metric = %v", value)
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "bypass_start") || !strings.Contains(logs, "bypass_end") {
		t.Fatalf("bypass events missing from logs: %s", logs)
	}
}

func TestMatrix_S13_PinnedOverBudget(t *testing.T) {
	f := newFixture(t, func(cfg *config.Config) { cfg.Storage.MaxBytes = config.Size(1 << 20) })
	now := f.clock.Now()
	mib := int64(1 << 20)
	f.put(t, "repo", "/pinned", "immutable", 2*mib, now.AddDate(0, 0, -30), 0)
	f.put(t, "repo", "/loose", "immutable", mib, now.AddDate(0, 0, -30), 0)
	if err := f.m.AddPin(context.Background(), AddPinRequest{Set: "keep", Repo: "repo", Key: "/pinned"}); err != nil {
		t.Fatal(err)
	}
	result := f.gc(t, GCOptions{HasTarget: true, TargetBytes: 0})
	if !result.PinnedOver || result.Stopped != "pinned_over_budget" {
		t.Fatalf("result = %+v", result)
	}
	if value := f.metrics.PinnedOverBudget.With().Value(); value != 1 {
		t.Fatalf("pinned over metric = %v", value)
	}
	for _, key := range []string{"/pinned", "/loose"} {
		if _, err := f.index.CurrentObject(context.Background(), "repo", key); err != nil {
			t.Fatalf("%s removed while pinned content exceeds the budget", key)
		}
	}
	if !strings.Contains(f.logs.String(), "pinned_over_budget") {
		t.Fatal("pinned over budget was not logged")
	}
}

func TestMatrix_S14_UnusedDays(t *testing.T) {
	f := newFixture(t, func(cfg *config.Config) {
		cfg.Repos[0].UnusedDays = 7
		cfg.Storage.ProtectRecentDays = 14
	})
	now := f.clock.Now()
	mib := int64(1 << 20)
	f.put(t, "repo", "/unused", "immutable", mib, now.AddDate(0, 0, -30), 0)
	f.put(t, "repo", "/recent", "immutable", mib, now.AddDate(0, 0, -1), 0)
	f.put(t, "repo", "/pinned", "immutable", mib, now.AddDate(0, 0, -30), 0)
	if err := f.m.AddPin(context.Background(), AddPinRequest{Set: "keep", Repo: "repo", Key: "/pinned"}); err != nil {
		t.Fatal(err)
	}
	result := f.gc(t, GCOptions{TargetBytes: -1})
	if len(result.Tiers[3].Victims) != 1 || result.Tiers[3].Victims[0].Key != "/unused" {
		t.Fatalf("unused victims = %#v", result.Tiers[3].Victims)
	}
	for _, key := range []string{"/recent", "/pinned"} {
		if _, err := f.index.CurrentObject(context.Background(), "repo", key); err != nil {
			t.Fatalf("%s removed", key)
		}
	}
}

func TestMatrix_S15_ProtectAndPinSets(t *testing.T) {
	f := newFixture(t, func(cfg *config.Config) {
		cfg.Storage.Protect = []config.StorageProtect{{Repo: "repo", Match: "config/**"}}
	})
	now := f.clock.Now()
	mib := int64(1 << 20)
	f.put(t, "repo", "/config/item", "immutable", mib, now.AddDate(0, 0, -30), 0)
	f.put(t, "repo", "/pinned/item", "immutable", mib, now.AddDate(0, 0, -30), 0)
	f.put(t, "repo", "/drop", "immutable", mib, now.AddDate(0, 0, -30), 0)
	if err := f.m.AddPin(context.Background(), AddPinRequest{Set: "baseline", Repo: "repo", Match: "pinned/**"}); err != nil {
		t.Fatal(err)
	}
	result := f.gc(t, GCOptions{HasTarget: true, TargetBytes: 0})
	if len(result.Tiers[3].Victims) != 1 || result.Tiers[3].Victims[0].Key != "/drop" {
		t.Fatalf("victims = %#v", result.Tiers[3].Victims)
	}
	pins, err := f.m.Pins(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 2 {
		t.Fatalf("pins = %#v", pins)
	}
	for _, set := range pins {
		if set.Name != "baseline" && set.Name != "config" {
			t.Fatalf("unexpected pin set %q", set.Name)
		}
		if set.MatchedObjects != 1 || set.Bytes != mib {
			t.Fatalf("set %q = %+v", set.Name, set)
		}
	}
}

func TestMatrix_S17_BlobRefcount(t *testing.T) {
	f := newFixture(t, nil)
	now := f.clock.Now()
	data := []byte("shared blob contents")
	writer, err := f.store.NewWriter(store.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	blob, _, err := writer.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.index.PutBlob(context.Background(), index.Blob{
		SHA256: blob.SHA256, RawSize: blob.RawSize, StoredSize: blob.StoredSize,
		StoredEnc: string(blob.StoredEnc), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	f.attach(t, "repo", "/a", "immutable", blob.StoredSize, now, 0, blob.SHA256)
	f.attach(t, "repo", "/b", "immutable", blob.StoredSize, now, 0, blob.SHA256)

	if _, err := f.m.Evict(context.Background(), EvictOptions{Repo: "repo", Glob: "/a"}); err != nil {
		t.Fatal(err)
	}
	if refcount, err := f.index.BlobRefCount(context.Background(), blob.SHA256); err != nil || refcount != 1 {
		t.Fatalf("refcount = %d, %v", refcount, err)
	}
	if _, err := f.store.OpenStored(blob.SHA256); err != nil {
		t.Fatalf("shared blob was unlinked early: %v", err)
	}
	if _, err := f.m.Evict(context.Background(), EvictOptions{Repo: "repo", Glob: "/b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.index.GetBlob(context.Background(), blob.SHA256); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("blob row survived its last object: %v", err)
	}
	if _, err := f.store.OpenStored(blob.SHA256); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("blob file survived its last object: %v", err)
	}
}

func TestMatrix_S18_GCConcurrencyAndReaders(t *testing.T) {
	f := newFixture(t, nil)
	now := f.clock.Now()
	data := []byte("reader stays valid")
	writer, err := f.store.NewWriter(store.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	blob, _, err := writer.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.index.PutBlob(context.Background(), index.Blob{
		SHA256: blob.SHA256, RawSize: blob.RawSize, StoredSize: blob.StoredSize,
		StoredEnc: string(blob.StoredEnc), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	f.attach(t, "repo", "/reader", "immutable", blob.StoredSize, now.AddDate(0, 0, -30), 0, blob.SHA256)
	reader, err := f.store.OpenStored(blob.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	f.m.testBeforeBatch = func() {
		once.Do(func() {
			close(entered)
			<-release
		})
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.m.GC(context.Background(), GCOptions{HasTarget: true, TargetBytes: 0})
		done <- err
	}()
	<-entered
	if _, err := f.m.GC(context.Background(), GCOptions{HasTarget: true, TargetBytes: 0}); !errors.Is(err, ErrBusy) {
		t.Fatalf("second GC error = %v, want ErrBusy", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reader failed during GC: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("reader saw %q", got)
	}
	if _, err := f.store.OpenStored(blob.SHA256); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("blob still openable after GC: %v", err)
	}
}

func TestMatrix_S20_Rollback(t *testing.T) {
	f := newFixture(t, nil)
	now := f.clock.Now()
	for i := 0; i < 3; i++ {
		f.put(t, "repo", "/meta", "mutable", int64(10+i), now.Add(time.Duration(i)*time.Minute), 0)
	}
	object, err := f.m.Rollback(context.Background(), "repo", "/meta", 0)
	if err != nil {
		t.Fatal(err)
	}
	if object.Generation != 2 || !object.Current {
		t.Fatalf("rollback result = %+v", object)
	}
	details, err := f.m.Details(context.Background(), "repo", "/meta", true)
	if err != nil {
		t.Fatal(err)
	}
	if details.Object.Generation != 2 {
		t.Fatalf("current generation = %d", details.Object.Generation)
	}
	object, err = f.m.Rollback(context.Background(), "repo", "/meta", 1)
	if err != nil || object.Generation != 1 {
		t.Fatalf("explicit rollback = %+v, %v", object, err)
	}
	if _, err := f.m.Rollback(context.Background(), "repo", "/meta", 99); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("missing generation error = %v", err)
	}
}

func TestReportingBreakdowns(t *testing.T) {
	f := newFixture(t, func(cfg *config.Config) {
		cfg.Storage.Protect = []config.StorageProtect{{Repo: "repo", Match: "pinned/**"}}
		cfg.Repos[0].Protect = []string{"config/**"}
	})
	now := f.clock.Now()
	mib := int64(1 << 20)
	f.put(t, "repo", "/old", "immutable", mib, now.AddDate(0, 0, -400), 3)
	f.put(t, "repo", "/pinned/item", "immutable", 2*mib, now.AddDate(0, 0, -40), 1)
	f.put(t, "repo", "/config/item", "immutable", 3*mib, now.AddDate(0, 0, -10), 0)
	f.put(t, "repo", "/recent", "immutable", 4*mib, now.Add(-time.Hour), 9)
	f.put(t, "repo", "/meta", "mutable", 5*mib, now.AddDate(0, 0, -200), 2)
	f.put(t, "other", "/other", "immutable", mib, now, 0)
	if err := f.m.AddPin(context.Background(), AddPinRequest{Set: "exact", Repo: "repo", Key: "/old"}); err != nil {
		t.Fatal(err)
	}

	stats, err := f.m.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Objects != 6 || len(stats.ByRepo) != 2 || len(stats.ByClass) == 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.PinnedBytes != 6*mib {
		t.Fatalf("pinned bytes = %d", stats.PinnedBytes)
	}

	for _, by := range []string{"repo", "class", "age", "top"} {
		result, err := f.m.DU(context.Background(), by, 3)
		if err != nil {
			t.Fatalf("du %s: %v", by, err)
		}
		if result.By != by {
			t.Fatalf("du %s = %+v", by, result)
		}
	}
	top, err := f.m.DU(context.Background(), "top", 2)
	if err != nil || len(top.Top) != 2 || top.Top[0].Key != "/meta" {
		t.Fatalf("top = %+v, %v", top, err)
	}
	if _, err := f.m.DU(context.Background(), "bogus", 0); err == nil {
		t.Fatal("unknown du grouping accepted")
	}
	ages, err := f.m.DU(context.Background(), "age", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ages.Ages) != 7 || ages.Ages[len(ages.Ages)-1].Objects != 1 {
		t.Fatalf("age buckets = %+v", ages.Ages)
	}

	list, err := f.m.List(context.Background(), ListOptions{Repo: "repo", Match: "pinned/**", PinnedOnly: true})
	if err != nil || len(list) != 1 || list[0].Key != "/pinned/item" {
		t.Fatalf("pinned list = %+v, %v", list, err)
	}
	list, err = f.m.List(context.Background(), ListOptions{Repo: "repo", Sort: "hits", Limit: 2, Offset: 1})
	if err != nil || len(list) != 2 || list[0].Hits != 3 {
		t.Fatalf("paged list = %+v, %v", list, err)
	}
	list, err = f.m.List(context.Background(), ListOptions{Repo: "repo", Sort: "atime"})
	if err != nil || list[0].Key != "/recent" {
		t.Fatalf("atime list = %+v, %v", list, err)
	}
	list, err = f.m.List(context.Background(), ListOptions{Repo: "repo", Sort: "size", UnverifiedOnly: true})
	if err != nil || len(list) != 5 {
		t.Fatalf("unverified list = %+v, %v", list, err)
	}
	list, err = f.m.List(context.Background(), ListOptions{Repo: "repo", Limit: 1, Offset: 99})
	if err != nil || len(list) != 0 {
		t.Fatalf("offset list = %+v, %v", list, err)
	}

	if err := f.index.UpsertExpectations(context.Background(), []index.Expectation{{
		Repo: "repo", Key: "/old", Algo: "sha256", Digest: strings.Repeat("a", 64),
		Source: "/index", CreatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	details, err := f.m.Details(context.Background(), "repo", "/old", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(details.Expectations) != 1 || len(details.Pins) != 1 || details.Pins[0] != "exact" {
		t.Fatalf("details = %+v", details)
	}
	if len(details.Generations) != 1 {
		t.Fatalf("non-history details returned %d generations", len(details.Generations))
	}
	pins, err := f.m.Pins(context.Background())
	if err != nil || len(pins) != 2 {
		t.Fatalf("pins = %+v, %v", pins, err)
	}
	names := map[string]bool{}
	for _, pin := range pins {
		names[pin.Name] = true
	}
	for _, want := range []string{"config", "exact"} {
		if !names[want] {
			t.Fatalf("missing pin set %q in %+v", want, pins)
		}
	}
}

func TestPruneAndPinRemoval(t *testing.T) {
	f := newFixture(t, nil)
	now := f.clock.Now()
	for i := 0; i < 4; i++ {
		f.put(t, "repo", "/meta", "mutable", int64(100+i), now.Add(time.Duration(i)*time.Minute), 0)
	}
	dry, err := f.m.Prune(context.Background(), PruneOptions{Keep: 1, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if dry.Objects != 3 {
		t.Fatalf("dry prune = %+v", dry)
	}
	result, err := f.m.Prune(context.Background(), PruneOptions{Keep: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Objects != 3 || result.Bytes == 0 {
		t.Fatalf("prune = %+v", result)
	}
	generations, err := f.index.Generations(context.Background(), "repo", "/meta")
	if err != nil || len(generations) != 1 || generations[0].Generation != 4 {
		t.Fatalf("generations = %+v, %v", generations, err)
	}

	if err := f.m.AddPin(context.Background(), AddPinRequest{Set: "set", Repo: "repo", Match: "meta*"}); err != nil {
		t.Fatal(err)
	}
	if err := f.m.RemovePin(context.Background(), "set", "repo", "meta*"); err != nil {
		t.Fatal(err)
	}
	sets, err := f.index.ListPinSets(context.Background())
	if err != nil || len(sets) != 1 || sets[0].Rules != 0 {
		t.Fatalf("pin sets = %+v, %v", sets, err)
	}
	if err := f.m.RemovePin(context.Background(), "set", "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestGCFileTiersAndZeroRefBlobs(t *testing.T) {
	f := newFixture(t, func(cfg *config.Config) {
		cfg.Storage.SnapshotKeep = 1
		cfg.Storage.QuarantineKeepDays = 7
	})
	now := f.clock.Now()
	mib := int64(1 << 20)
	for i := 0; i < 6; i++ {
		f.put(t, "repo", fmt.Sprintf("/file-%d", i), "immutable", mib, now.AddDate(0, 0, -30), 0)
	}
	tmpDir := filepath.Join(f.dir, "tmp")
	oldTmp := filepath.Join(tmpDir, "old.part")
	recentTmp := filepath.Join(tmpDir, "recent.part")
	if err := os.WriteFile(oldTmp, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recentTmp, []byte("recent"), 0o640); err != nil {
		t.Fatal(err)
	}
	past := now.Add(-2 * time.Hour)
	if err := os.Chtimes(oldTmp, past, past); err != nil {
		t.Fatal(err)
	}
	quarantineDir := filepath.Join(f.dir, "quarantine")
	oldQuarantine := filepath.Join(quarantineDir, "old.blob")
	if err := os.WriteFile(oldQuarantine, []byte("bad"), 0o640); err != nil {
		t.Fatal(err)
	}
	quarantinePast := now.AddDate(0, 0, -30)
	if err := os.Chtimes(oldQuarantine, quarantinePast, quarantinePast); err != nil {
		t.Fatal(err)
	}
	snapshots := filepath.Join(f.dir, "snapshots")
	for i := 0; i < 3; i++ {
		name := filepath.Join(snapshots, fmt.Sprintf("index-%d.db", i))
		if err := os.WriteFile(name, []byte("snapshot"), 0o640); err != nil {
			t.Fatal(err)
		}
		at := now.Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(name, at, at); err != nil {
			t.Fatal(err)
		}
	}
	orphan := f.uniqueSHA()
	f.putBlob(t, orphan, 777)
	if err := f.index.PutBlob(context.Background(), index.Blob{
		SHA256: orphan, RawSize: 777, StoredSize: 777, StoredEnc: "identity", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.dir, "blobs", orphan[:2], orphan[2:4]), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "blobs", orphan[:2], orphan[2:4], orphan), []byte("orphan"), 0o640); err != nil {
		t.Fatal(err)
	}

	result := f.gc(t, GCOptions{HasTarget: true, TargetBytes: 0, DryRun: true})
	if len(result.Tiers[0].Victims) != 4 {
		t.Fatalf("tier 0 dry victims = %+v", result.Tiers[0].Victims)
	}
	if len(result.Tiers[1].Victims) != 1 || result.Tiers[1].Victims[0].Kind != "blob" {
		t.Fatalf("tier 1 dry victims = %+v", result.Tiers[1].Victims)
	}
	result = f.gc(t, GCOptions{HasTarget: true, TargetBytes: 0})
	if len(result.Tiers[0].Victims) != 4 || result.Tiers[0].Bytes != int64(len("old")+len("bad")+2*len("snapshot")) {
		t.Fatalf("tier 0 = %+v", result.Tiers[0])
	}
	if _, err := os.Stat(oldTmp); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old temporary file survived")
	}
	if _, err := os.Stat(recentTmp); err != nil {
		t.Fatal("recent temporary file was removed")
	}
	if _, err := os.Stat(oldQuarantine); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old quarantine entry survived")
	}
	entries, err := os.ReadDir(snapshots)
	if err != nil || len(entries) != 1 {
		t.Fatalf("snapshots = %v, %v", entries, err)
	}
	if _, err := f.index.GetBlob(context.Background(), orphan); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("orphan blob row survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "blobs", orphan[:2], orphan[2:4], orphan)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("orphan blob file survived")
	}
}

func TestGCWorkersAndOverrides(t *testing.T) {
	f := newFixture(t, func(cfg *config.Config) {
		cfg.Storage.GCInterval = config.Duration(2 * time.Second)
		cfg.Storage.MaxBytes = config.Size(1 << 20)
	})
	now := f.clock.Now()
	mib := int64(1 << 20)
	f.put(t, "repo", "/one", "immutable", mib, now.AddDate(0, 0, -30), 0)
	f.put(t, "repo", "/two", "immutable", mib, now.AddDate(0, 0, -30), 0)
	f.m.Start()
	f.m.Start()
	time.Sleep(10 * time.Millisecond)
	f.clock.Advance(3 * time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(f.logs.String(), "gc_start") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(f.logs.String(), "gc_start") {
		t.Fatal("periodic worker did not run")
	}
	if err := f.m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Close(); err != nil {
		t.Fatal(err)
	}

	f.cfg.Storage.MaxBytes = config.Size(4 << 20)
	f.m.UpdateConfig(f.cfg)
	if f.m.Config() != f.cfg {
		t.Fatal("UpdateConfig did not swap the configuration")
	}
	if f.m.BypassActive() || f.m.PinnedOverBudget() {
		t.Fatal("fresh manager reported an inactive state as active")
	}

	if _, err := f.m.GC(context.Background(), GCOptions{HasTarget: true, TargetBytes: 10, TargetPct: 0.5, Policy: "score"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.GC(context.Background(), GCOptions{Policy: "bogus", TargetBytes: -1}); err == nil {
		t.Fatal("unknown policy accepted")
	}
	if _, err := f.m.GC(context.Background(), GCOptions{TargetPct: 101}); err == nil {
		t.Fatal("invalid percentage accepted")
	}
	if _, err := f.m.Evict(context.Background(), EvictOptions{}); err == nil {
		t.Fatal("empty evict accepted")
	}
	if _, err := f.m.Evict(context.Background(), EvictOptions{Repo: "repo", Glob: "*", OlderThanDays: -1}); err == nil {
		t.Fatal("negative older-than accepted")
	}
	if _, err := f.m.Evict(context.Background(), EvictOptions{Repo: "repo", Glob: "*", LargerThan: -1}); err == nil {
		t.Fatal("negative larger-than accepted")
	}
	if _, err := f.m.Prune(context.Background(), PruneOptions{Keep: -2}); err == nil {
		t.Fatal("invalid keep accepted")
	}
}

func TestUsageCacheAndConstruction(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("manager without dependencies was accepted")
	}
	f := newFixture(t, nil)
	now := f.clock.Now()
	f.put(t, "repo", "/cached", "immutable", 100, now, 0)
	first := f.usage(t)
	f.put(t, "repo", "/later", "immutable", 100, now, 0)
	cached, err := f.m.Usage(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if cached.Blobs != first.Blobs {
		t.Fatalf("cached usage changed: %+v -> %+v", first, cached)
	}
	f.clock.Advance(11 * time.Second)
	fresh, err := f.m.Usage(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Blobs != first.Blobs+1 {
		t.Fatalf("usage did not refresh: %+v", fresh)
	}
}

func TestStopReasons(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := stopReason(cancelled); got != "cancelled" {
		t.Fatalf("cancel reason = %q", got)
	}
	expired, expire := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer expire()
	if got := stopReason(expired); got != "max_duration" {
		t.Fatalf("deadline reason = %q", got)
	}
}

func TestMatrix_F11_ReservationsCountDownloadsInProgress(t *testing.T) {
	f := newFixture(t, nil)
	f.free.Store(10 << 30) // hard floor is 1 GiB

	first, ok := f.m.Reserve(5 << 30)
	if !ok {
		t.Fatal("a download that fits was refused")
	}
	second, ok := f.m.Reserve(5 << 30)
	if ok {
		second()
		t.Fatal("two downloads that together break the floor were both admitted")
	}
	first()
	third, ok := f.m.Reserve(5 << 30)
	if !ok {
		t.Fatal("space was not returned when the first download finished")
	}
	third()
	third() // releasing twice must not return the space twice
	if _, ok := f.m.Reserve(10 << 30); ok {
		t.Fatal("a download larger than the free space was admitted")
	}
}
