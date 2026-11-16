package integrity

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mmrzaf/larder/internal/index"
)

// SnapshotName returns the default file name for a snapshot at t.
func SnapshotName(t time.Time) string {
	return "index-" + t.UTC().Format("20060102-1504") + ".db"
}

// SnapshotTo writes a consistent copy of the index to path.
func SnapshotTo(ctx context.Context, idx *index.DB, path string) error {
	if idx == nil {
		return fmt.Errorf("snapshot: index is required")
	}
	if path == "" {
		return fmt.Errorf("snapshot: destination is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("snapshot: resolve destination: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		return fmt.Errorf("snapshot: create directory: %w", err)
	}
	if _, err := os.Stat(abs); err == nil {
		return fmt.Errorf("snapshot: destination %s already exists", abs)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("snapshot: stat destination: %w", err)
	}
	if err := idx.VacuumInto(ctx, abs); err != nil {
		return err
	}
	if err := os.Chmod(abs, 0o640); err != nil {
		return fmt.Errorf("snapshot: set mode: %w", err)
	}
	return nil
}

// SnapshotIn writes a timestamped snapshot under the snapshots directory.
func SnapshotIn(ctx context.Context, idx *index.DB, dataDir string, t time.Time) (string, error) {
	dir := filepath.Join(dataDir, "snapshots")
	path := filepath.Join(dir, SnapshotName(t))
	if err := SnapshotTo(ctx, idx, path); err != nil {
		return "", err
	}
	return path, nil
}

// PruneSnapshots keeps the newest count snapshots in dir.
func PruneSnapshots(dir string, keep int) ([]string, error) {
	if keep < 0 {
		return nil, fmt.Errorf("snapshot: keep must not be negative")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("snapshot: read directory: %w", err)
	}
	type snapshot struct {
		name    string
		modTime time.Time
	}
	var snapshots []snapshot
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "index-") || !strings.HasSuffix(entry.Name(), ".db") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("snapshot: stat %s: %w", entry.Name(), err)
		}
		snapshots = append(snapshots, snapshot{name: entry.Name(), modTime: info.ModTime()})
	}
	sort.Slice(snapshots, func(i, j int) bool {
		if !snapshots[i].modTime.Equal(snapshots[j].modTime) {
			return snapshots[i].modTime.Before(snapshots[j].modTime)
		}
		return snapshots[i].name < snapshots[j].name
	})
	if len(snapshots) <= keep {
		return nil, nil
	}
	var removed []string
	for _, snapshot := range snapshots[:len(snapshots)-keep] {
		path := filepath.Join(dir, snapshot.name)
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return removed, fmt.Errorf("snapshot: remove %s: %w", path, err)
		}
		removed = append(removed, path)
	}
	return removed, nil
}

// Restore replaces the live index with a snapshot, keeping index.db.bak.
func Restore(ctx context.Context, dataDir, source string) error {
	if dataDir == "" || source == "" {
		return fmt.Errorf("restore: data directory and snapshot are required")
	}
	live := filepath.Join(dataDir, "index.db")
	backup := live + ".bak"
	sourceAbs, err := filepath.Abs(source)
	if err != nil {
		return fmt.Errorf("restore: resolve snapshot: %w", err)
	}
	liveAbs, err := filepath.Abs(live)
	if err != nil {
		return fmt.Errorf("restore: resolve index: %w", err)
	}
	if sourceAbs == liveAbs {
		return fmt.Errorf("restore: snapshot and live index are the same file")
	}
	if _, err := os.Stat(sourceAbs); err != nil {
		return fmt.Errorf("restore: snapshot: %w", err)
	}
	check, err := index.OpenReadOnly(sourceAbs)
	if err != nil {
		return fmt.Errorf("restore: open snapshot: %w", err)
	}
	checkErr := check.QuickCheck(ctx)
	_ = check.Close()
	if checkErr != nil {
		return fmt.Errorf("restore: snapshot is not usable: %w", checkErr)
	}

	if err := os.Remove(backup); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("restore: remove previous backup: %w", err)
	}
	hadLive := true
	if err := os.Rename(live, backup); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("restore: keep previous index: %w", err)
		}
		hadLive = false
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(live + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("restore: remove %s: %w", live+suffix, err)
		}
	}
	if err := copyFile(sourceAbs, live); err != nil {
		if hadLive {
			_ = os.Rename(backup, live)
		}
		return fmt.Errorf("restore: copy snapshot: %w", err)
	}
	restored, err := index.Open(live)
	if err != nil {
		if hadLive {
			_ = os.Rename(backup, live)
		}
		return fmt.Errorf("restore: open restored index: %w", err)
	}
	checkErr = restored.QuickCheck(ctx)
	_ = restored.Close()
	if checkErr != nil {
		if hadLive {
			_ = os.Remove(live)
			_ = os.Rename(backup, live)
		}
		return fmt.Errorf("restore: restored index is not usable: %w", checkErr)
	}
	return nil
}

func copyFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
