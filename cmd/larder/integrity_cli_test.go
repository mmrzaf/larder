package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/kinds"
	"github.com/mmrzaf/larder/internal/kinds/generickind"
	"github.com/mmrzaf/larder/internal/metrics"
	"github.com/mmrzaf/larder/internal/server"
	"github.com/mmrzaf/larder/internal/store"
)

func objectPath(t *testing.T, f *cliFixture, key string) (string, string) {
	t.Helper()
	object, err := f.index.CurrentObject(context.Background(), "repo", key)
	if err != nil {
		t.Fatal(err)
	}
	return object.SHA256, filepath.Join(f.dir, "blobs", object.SHA256[:2], object.SHA256[2:4], object.SHA256)
}

func TestMatrix_L15_VerifyReindexSnapshot(t *testing.T) {
	f := newCLIFixture(t, 2)
	sha, path := objectPath(t, f, "/object-00")
	f.close()

	code, stdout, stderr := runCLI(t, "-c", f.path, "verify", "--json")
	if code != 0 {
		t.Fatalf("verify exit %d: %s", code, stderr)
	}
	var scrub struct {
		Checked     int64 `json:"checked"`
		Corrupt     int64 `json:"corrupt"`
		Quarantined int64 `json:"quarantined"`
	}
	if err := json.Unmarshal([]byte(stdout), &scrub); err != nil {
		t.Fatalf("verify JSON: %v\n%s", err, stdout)
	}
	if scrub.Checked != 4 || scrub.Corrupt != 0 {
		t.Fatalf("verify = %+v", scrub)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), int(info.Size())), 0o640); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runCLI(t, "-c", f.path, "verify", "--json")
	if code != 1 {
		t.Fatalf("verify corrupted exit %d: %s", code, stderr)
	}
	if err := json.Unmarshal([]byte(stdout), &scrub); err != nil {
		t.Fatalf("verify JSON: %v\n%s", err, stdout)
	}
	if scrub.Corrupt != 1 || scrub.Quarantined != 1 {
		t.Fatalf("corrupt verify = %+v", scrub)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "quarantine", sha)); err != nil {
		t.Fatalf("quarantine file missing: %v", err)
	}

	orphan := filepath.Join(f.dir, "blobs", "aa", "bb", "aabb"+string(bytes.Repeat([]byte("0"), 60)))
	if err := os.MkdirAll(filepath.Dir(orphan), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphan, []byte("orphan"), 0o640); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ = runCLI(t, "-c", f.path, "reindex", "--check", "--json")
	if code != 4 {
		t.Fatalf("reindex check exit %d, want 4", code)
	}
	var report struct {
		OrphanFiles []struct {
			Path string `json:"path"`
		} `json:"orphan_files"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("reindex JSON: %v\n%s", err, stdout)
	}
	if len(report.OrphanFiles) != 1 || report.OrphanFiles[0].Path != orphan {
		t.Fatalf("orphan report = %+v", report)
	}
	code, stdout, stderr = runCLI(t, "-c", f.path, "reindex", "--fix", "--json")
	if code != 0 {
		t.Fatalf("reindex fix exit %d: %s\n%s", code, stderr, stdout)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphan survived reindex --fix")
	}

	snapshot := filepath.Join(t.TempDir(), "manual-snapshot.db")
	code, stdout, stderr = runCLI(t, "-c", f.path, "snapshot", "--to", snapshot, "--json")
	if code != 0 {
		t.Fatalf("snapshot exit %d: %s", code, stderr)
	}
	var snapshotResult struct {
		Snapshot string `json:"snapshot"`
	}
	if err := json.Unmarshal([]byte(stdout), &snapshotResult); err != nil {
		t.Fatalf("snapshot JSON: %v\n%s", err, stdout)
	}
	if snapshotResult.Snapshot != snapshot {
		t.Fatalf("snapshot path = %q", snapshotResult.Snapshot)
	}
	if _, err := os.Stat(snapshot); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = runCLI(t, "-c", f.path, "snapshot", "--restore", snapshot)
	if code != 0 {
		t.Fatalf("restore exit %d: %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "index.db.bak")); err != nil {
		t.Fatalf("restore backup missing: %v", err)
	}
}

func TestReindexRebuildBlobsOnlyCommand(t *testing.T) {
	f := newCLIFixture(t, 1)
	f.close()
	for _, name := range []string{"index.db", "index.db-wal", "index.db-shm"} {
		if err := os.Remove(filepath.Join(f.dir, name)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	code, stdout, stderr := runCLI(t, "-c", f.path, "reindex", "--rebuild-blobs-only", "--json")
	if code != 0 {
		t.Fatalf("rebuild exit %d: %s", code, stderr)
	}
	if !bytes.Contains([]byte(stderr), []byte("keys are lost")) {
		t.Fatalf("missing rebuild warning: %s", stderr)
	}
	var result struct {
		Blobs int64 `json:"blobs"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("rebuild JSON: %v\n%s", err, stdout)
	}
	if result.Blobs != 3 {
		t.Fatalf("rebuilt blobs = %d", result.Blobs)
	}
	code, _, _ = runCLI(t, "-c", f.path, "reindex", "--rebuild-blobs-only")
	if code != 1 {
		t.Fatalf("second rebuild exit %d, want 1", code)
	}
}

func TestMatrix_L15_RemoteVerifyRepairFlag(t *testing.T) {
	f := newCLIFixture(t, 1)
	_, path := objectPath(t, f, "/object-00")
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
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("c"), int(info.Size())), 0o640); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "-c", f.path, "verify", "--repair", "--json", "--admin", adminURL)
	if code != 1 {
		t.Fatalf("remote verify exit %d: %s", code, stderr)
	}
	var result struct {
		Corrupt  int64 `json:"corrupt"`
		Repaired int64 `json:"repaired"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("verify JSON: %v\n%s", err, stdout)
	}
	if result.Corrupt != 1 {
		t.Fatalf("corrupt = %d", result.Corrupt)
	}
	liveSnapshot := filepath.Join(t.TempDir(), "live.db")
	code, _, stderr = runCLI(t, "-c", f.path, "snapshot", "--to", liveSnapshot)
	if code != 0 {
		t.Fatalf("live snapshot exit %d: %s", code, stderr)
	}
	if _, err := os.Stat(liveSnapshot); err != nil {
		t.Fatalf("live snapshot missing: %v", err)
	}
	code, _, _ = runCLI(t, "-c", f.path, "reindex", "--check")
	if code != 0 {
		t.Fatalf("live reindex --check exit %d", code)
	}
	code, _, _ = runCLI(t, "-c", f.path, "reindex", "--fix")
	if code != 4 {
		t.Fatalf("reindex --fix with the server running exit %d, want 4", code)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = idx.Close()
	_ = st.Close()
}
