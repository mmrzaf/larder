package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/kinds"
	"github.com/mmrzaf/larder/internal/kinds/generickind"
	"github.com/mmrzaf/larder/internal/metrics"
	"github.com/mmrzaf/larder/internal/server"
	"github.com/mmrzaf/larder/internal/store"
)

type cliFixture struct {
	path    string
	cfg     *config.Config
	dir     string
	store   *store.Store
	index   *index.DB
	counter int
}

func newCLIFixture(t *testing.T, objects int) *cliFixture {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "larder.yaml")
	content := fmt.Sprintf(`version: 1
data_dir: %s
listen: 127.0.0.1:0
admin:
  listen: ""
cache:
  metadata_history: 1
storage:
  max_bytes: 1GiB
  min_free_bytes: 1MiB
  hard_free_bytes: 0
  protect_recent_days: 0
repos:
  - name: repo
    kind: generic
    mount: /repo
    upstreams:
      - url: http://127.0.0.1:9
`, dir)
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := index.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	fixture := &cliFixture{path: path, cfg: cfg, dir: dir, store: st, index: idx}
	t.Cleanup(func() {
		_ = idx.Close()
		_ = st.Close()
	})
	now := time.Unix(1_700_000_000, 0)
	for i := 0; i < objects; i++ {
		fixture.put(t, "repo", fmt.Sprintf("/object-%02d", i), "immutable", int64(1000+i), now.Add(-time.Duration(i)*time.Hour))
	}
	fixture.put(t, "repo", "/meta", "mutable", 2048, now.Add(-time.Hour))
	fixture.put(t, "repo", "/meta", "mutable", 4096, now)
	return fixture
}

func (f *cliFixture) put(t *testing.T, repo, key, class string, size int64, at time.Time) {
	t.Helper()
	f.counter++
	content := make([]byte, size)
	for i := range content {
		content[i] = byte(f.counter + i)
	}
	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])
	if err := f.index.PutBlob(context.Background(), index.Blob{
		SHA256: sha, RawSize: size, StoredSize: size, StoredEnc: "identity", CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	blobPath := filepath.Join(f.dir, "blobs", sha[:2], sha[2:4], sha)
	if err := os.MkdirAll(filepath.Dir(blobPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blobPath, content, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := f.index.PutObject(context.Background(), index.PutObjectParams{
		Repo: repo, Key: key, Class: class, SHA256: sha,
		FetchedAt: at, ValidatedAt: at, Verified: true,
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *cliFixture) close() {
	_ = f.index.Close()
	_ = f.store.Close()
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestMatrix_L03_ReportingCommands(t *testing.T) {
	f := newCLIFixture(t, 3)
	f.close()

	code, stdout, stderr := runCLI(t, "-c", f.path, "--json", "stats")
	if code != 0 {
		t.Fatalf("stats exit %d: %s", code, stderr)
	}
	var stats struct {
		BudgetBytes int64 `json:"budget_bytes"`
		UsedBytes   int64 `json:"used_bytes"`
		Objects     int64 `json:"objects"`
		Blobs       int64 `json:"blobs"`
	}
	if err := json.Unmarshal([]byte(stdout), &stats); err != nil {
		t.Fatalf("stats JSON: %v\n%s", err, stdout)
	}
	if stats.Objects != 4 || stats.Blobs != 5 || stats.UsedBytes < 1000 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.BudgetBytes != int64(1<<30) {
		t.Fatalf("budget = %d", stats.BudgetBytes)
	}

	code, stdout, stderr = runCLI(t, "-c", f.path, "du", "--by", "repo", "--json")
	if code != 0 {
		t.Fatalf("du exit %d: %s", code, stderr)
	}
	var du struct {
		By    string `json:"by"`
		Repos []struct {
			Repo    string `json:"repo"`
			Objects int64  `json:"objects"`
			Bytes   int64  `json:"stored_bytes"`
		} `json:"repos"`
	}
	if err := json.Unmarshal([]byte(stdout), &du); err != nil {
		t.Fatalf("du JSON: %v\n%s", err, stdout)
	}
	if du.By != "repo" || len(du.Repos) != 1 || du.Repos[0].Repo != "repo" || du.Repos[0].Objects != 4 {
		t.Fatalf("du = %+v", du)
	}

	code, stdout, stderr = runCLI(t, "-c", f.path, "ls", "repo", "--sort", "size", "--json")
	if code != 0 {
		t.Fatalf("ls exit %d: %s", code, stderr)
	}
	var objects []struct {
		Key   string `json:"key"`
		Size  int64  `json:"stored_size"`
		Class string `json:"class"`
	}
	if err := json.Unmarshal([]byte(stdout), &objects); err != nil {
		t.Fatalf("ls JSON: %v\n%s", err, stdout)
	}
	if len(objects) != 4 || objects[0].Key != "/meta" || objects[0].Size != 4096 {
		t.Fatalf("ls = %+v", objects)
	}

	code, stdout, stderr = runCLI(t, "-c", f.path, "objects", "repo", "/meta", "--history", "--json")
	if code != 0 {
		t.Fatalf("objects exit %d: %s", code, stderr)
	}
	var details struct {
		Object struct {
			Key        string `json:"key"`
			Generation int64  `json:"generation"`
		} `json:"object"`
		Generations []struct {
			Generation int64 `json:"generation"`
		} `json:"generations"`
	}
	if err := json.Unmarshal([]byte(stdout), &details); err != nil {
		t.Fatalf("objects JSON: %v\n%s", err, stdout)
	}
	if details.Object.Key != "/meta" || details.Object.Generation != 2 || len(details.Generations) != 2 {
		t.Fatalf("details = %+v", details)
	}
}

func TestMatrix_L04_Pins(t *testing.T) {
	f := newCLIFixture(t, 2)
	f.close()

	code, _, stderr := runCLI(t, "-c", f.path, "pin", "--set", "baseline", "--repo", "repo", "--match", "object-00")
	if code != 0 {
		t.Fatalf("pin exit %d: %s", code, stderr)
	}
	code, stdout, stderr := runCLI(t, "-c", f.path, "--json", "pins")
	if code != 0 {
		t.Fatalf("pins exit %d: %s", code, stderr)
	}
	var pins []struct {
		Name           string `json:"name"`
		Source         string `json:"source"`
		Rules          int64  `json:"rules"`
		MatchedObjects int64  `json:"matched_objects"`
		Bytes          int64  `json:"bytes"`
	}
	if err := json.Unmarshal([]byte(stdout), &pins); err != nil {
		t.Fatalf("pins JSON: %v\n%s", err, stdout)
	}
	if len(pins) != 1 || pins[0].Name != "baseline" || pins[0].Source != "pin" ||
		pins[0].Rules != 1 || pins[0].MatchedObjects != 1 || pins[0].Bytes != 1000 {
		t.Fatalf("pins = %+v", pins)
	}

	code, _, stderr = runCLI(t, "-c", f.path, "unpin", "--set", "baseline")
	if code != 0 {
		t.Fatalf("unpin exit %d: %s", code, stderr)
	}
	code, stdout, _ = runCLI(t, "-c", f.path, "--json", "pins")
	if code != 0 {
		t.Fatalf("pins after unpin exit %d", code)
	}
	if err := json.Unmarshal([]byte(stdout), &pins); err != nil {
		t.Fatal(err)
	}
	if len(pins) != 0 {
		t.Fatalf("pin set survived unpin: %+v", pins)
	}
}

func TestMatrix_L05_AdminAndDirectGC(t *testing.T) {
	f := newCLIFixture(t, 4)
	f.close()

	st, err := store.Open(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := index.Open(filepath.Join(f.dir, "index.db"))
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	registry := kinds.NewRegistry()
	if err := registry.Register("generic", generickind.New); err != nil {
		t.Fatal(err)
	}
	metricSet, err := metrics.New("test")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Options{
		Config: f.cfg, ConfigPath: f.path, Store: st, Index: idx,
		Kinds: registry, Metrics: metricSet,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		ListenOverride: "127.0.0.1:0", AdminListenOverride: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	adminURL := "http://" + srv.AdminAddr()

	code, stdout, stderr := runCLI(t, "-c", f.path, "gc", "--dry-run", "--json", "--admin", adminURL)
	if code != 0 {
		t.Fatalf("admin gc exit %d: %s", code, stderr)
	}
	var remoteGC, directGC struct {
		UsedBefore  int64 `json:"used_before"`
		UsedAfter   int64 `json:"used_after"`
		BudgetBytes int64 `json:"budget_bytes"`
		Tiers       []struct {
			Tier    int   `json:"tier"`
			Objects int64 `json:"objects"`
			Bytes   int64 `json:"bytes"`
		} `json:"tiers"`
	}
	if err := json.Unmarshal([]byte(stdout), &remoteGC); err != nil {
		t.Fatalf("admin gc JSON: %v\n%s", err, stdout)
	}
	code, stdout, stderr = runCLI(t, "-c", f.path, "evict", "repo", "object-*", "--dry-run", "--json", "--admin", adminURL)
	if code != 0 {
		t.Fatalf("admin evict exit %d: %s", code, stderr)
	}
	var remoteEvict, directEvict struct {
		Objects int64 `json:"objects"`
		Bytes   int64 `json:"bytes"`
	}
	if err := json.Unmarshal([]byte(stdout), &remoteEvict); err != nil {
		t.Fatalf("admin evict JSON: %v\n%s", err, stdout)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = idx.Close()
	_ = st.Close()

	code, stdout, stderr = runCLI(t, "-c", f.path, "gc", "--dry-run", "--json", "--admin", adminURL)
	if code != 0 {
		t.Fatalf("direct gc exit %d: %s", code, stderr)
	}
	if err := json.Unmarshal([]byte(stdout), &directGC); err != nil {
		t.Fatalf("direct gc JSON: %v\n%s", err, stdout)
	}
	if remoteGC.UsedBefore != directGC.UsedBefore || remoteGC.UsedAfter != directGC.UsedAfter ||
		remoteGC.BudgetBytes != directGC.BudgetBytes || !reflect.DeepEqual(remoteGC.Tiers, directGC.Tiers) {
		t.Fatalf("GC differs: remote=%+v direct=%+v", remoteGC, directGC)
	}
	code, stdout, stderr = runCLI(t, "-c", f.path, "evict", "repo", "object-*", "--dry-run", "--json", "--admin", adminURL)
	if code != 0 {
		t.Fatalf("direct evict exit %d: %s", code, stderr)
	}
	if err := json.Unmarshal([]byte(stdout), &directEvict); err != nil {
		t.Fatalf("direct evict JSON: %v\n%s", err, stdout)
	}
	if remoteEvict.Objects != directEvict.Objects || remoteEvict.Bytes != directEvict.Bytes {
		t.Fatalf("evict differs: remote=%+v direct=%+v", remoteEvict, directEvict)
	}
}
