package integrity

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mmrzaf/larder/internal/config"
)

func TestMaintenanceRunOnce(t *testing.T) {
	f := newScrubFixture(t)
	for i := 0; i < 5; i++ {
		f.add(t, "repo", "/maintenance-"+string(rune('a'+i)), []byte("maintenance content "+string(rune('a'+i))), false)
	}
	cfg := config.Defaults()
	cfg.DataDir = f.dir
	cfg.Storage.Scrub.DailyPercent = 40
	cfg.Storage.SnapshotKeep = 2
	pruned := 0
	maintainer, err := NewMaintainer(MaintenanceOptions{
		Scrubber: f.scrubber(t), Index: f.index, DataDir: f.dir,
		Config: func() *config.Config { return cfg }, Clock: f.clock,
		Prune: func(context.Context) error { pruned++; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := maintainer.Close(); err != nil {
		t.Fatal(err)
	}
	checked := int64(0)
	for i := 0; i < 3; i++ {
		result, err := maintainer.RunOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		checked += result.Verify.Checked
		if result.Snapshot == "" {
			t.Fatal("no snapshot written")
		}
		if _, err := os.Stat(result.Snapshot); err != nil {
			t.Fatal(err)
		}
		f.clock.Advance(time.Hour)
	}
	if checked != 5 {
		t.Fatalf("maintenance checked %d blobs", checked)
	}
	if pruned != 3 {
		t.Fatalf("prune callback ran %d times", pruned)
	}
	entries, err := os.ReadDir(filepath.Join(f.dir, "snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("snapshots retained = %d", len(entries))
	}
	if _, err := NewMaintainer(MaintenanceOptions{}); err == nil {
		t.Fatal("maintainer without dependencies accepted")
	}
}
