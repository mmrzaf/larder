package integrity

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/store"
)

const orphanGrace = time.Hour

// OrphanFile is a blob file without an index row.
type OrphanFile struct {
	Path    string    `json:"path"`
	SHA256  string    `json:"sha256"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// ReindexFix summarizes the changes made by --fix.
type ReindexFix struct {
	ObjectsRemoved      int64 `json:"objects_removed"`
	BlobRowsRemoved     int64 `json:"blob_rows_removed"`
	OrphanFilesRemoved  int64 `json:"orphan_files_removed"`
	RefcountsRecomputed int64 `json:"refcounts_recomputed"`
	DuplicateKeysFixed  int64 `json:"duplicate_keys_fixed"`
	ExpectationsRemoved int64 `json:"expectations_removed"`
}

// ReindexReport describes index and blob-store disagreement.
type ReindexReport struct {
	ObjectsMissingBlobs []index.ObjectRef        `json:"objects_missing_blobs,omitempty"`
	BlobsWithoutObjects []index.Blob             `json:"blobs_without_objects,omitempty"`
	OrphanFiles         []OrphanFile             `json:"orphan_files,omitempty"`
	RefcountMismatches  []index.RefcountMismatch `json:"refcount_mismatches,omitempty"`
	DuplicateCurrents   []index.DuplicateCurrent `json:"duplicate_currents,omitempty"`
	ExpectationOrphans  int64                    `json:"expectation_orphans"`
	Repair              ReindexFix               `json:"repair"`
}

// Problems reports whether the snapshot found something to repair.
func (r ReindexReport) Problems() int {
	return len(r.ObjectsMissingBlobs) + len(r.BlobsWithoutObjects) + len(r.OrphanFiles) +
		len(r.RefcountMismatches) + len(r.DuplicateCurrents) + int(r.ExpectationOrphans)
}

// Reindex checks the index against the blob tree.
func Reindex(ctx context.Context, st *store.Store, idx *index.DB) (ReindexReport, error) {
	var report ReindexReport
	blobs, err := idx.AllBlobs(ctx)
	if err != nil {
		return report, err
	}
	objects, err := idx.AllObjects(ctx)
	if err != nil {
		return report, err
	}
	bySHA := make(map[string][]index.ObjectRef)
	for _, object := range objects {
		bySHA[object.SHA256] = append(bySHA[object.SHA256], object)
	}
	for _, blob := range blobs {
		path, err := st.BlobPath(blob.SHA256)
		if err != nil {
			return report, err
		}
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				report.ObjectsMissingBlobs = append(report.ObjectsMissingBlobs, bySHA[blob.SHA256]...)
				continue
			}
			return report, fmt.Errorf("stat blob %s: %w", blob.SHA256, err)
		}
		if len(bySHA[blob.SHA256]) == 0 {
			report.BlobsWithoutObjects = append(report.BlobsWithoutObjects, blob)
		}
	}
	files, err := blobFiles(st)
	if err != nil {
		return report, err
	}
	known := make(map[string]bool, len(blobs))
	for _, blob := range blobs {
		known[blob.SHA256] = true
	}
	for _, file := range files {
		if known[file.SHA256] {
			continue
		}
		report.OrphanFiles = append(report.OrphanFiles, file)
	}
	report.RefcountMismatches, err = idx.RefcountMismatches(ctx)
	if err != nil {
		return report, err
	}
	report.DuplicateCurrents, err = idx.DuplicateCurrents(ctx)
	if err != nil {
		return report, err
	}
	report.ExpectationOrphans, err = idx.ExpectationOrphanCount(ctx)
	if err != nil {
		return report, err
	}
	return report, nil
}

// Fix repairs what can be repaired without inventing content.
func Fix(ctx context.Context, st *store.Store, idx *index.DB, logger *slog.Logger) (ReindexReport, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	report, err := Reindex(ctx, st, idx)
	if err != nil {
		return report, err
	}
	now := time.Now()
	missing := make(map[string]bool)
	for _, object := range report.ObjectsMissingBlobs {
		if missing[object.SHA256] {
			continue
		}
		missing[object.SHA256] = true
		if _, err := idx.DeleteBlobAndObjects(ctx, object.SHA256); err != nil {
			return report, err
		}
		report.Repair.ObjectsRemoved++
		report.Repair.BlobRowsRemoved++
	}
	for _, blob := range report.BlobsWithoutObjects {
		if missing[blob.SHA256] {
			continue
		}
		path, err := st.BlobPath(blob.SHA256)
		if err != nil {
			return report, err
		}
		info, statErr := os.Stat(path)
		if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
			return report, statErr
		}
		if statErr == nil && now.Sub(info.ModTime()) < orphanGrace {
			continue
		}
		freed, err := idx.DeleteBlobRows(ctx, []string{blob.SHA256})
		if err != nil {
			return report, err
		}
		if len(freed) > 0 {
			if err := st.Delete(blob.SHA256); err != nil {
				logger.Warn("blob_unlink_failed", "sha256", blob.SHA256, "error", err)
			}
			report.Repair.BlobRowsRemoved++
			report.Repair.OrphanFilesRemoved++
		}
	}
	for _, file := range report.OrphanFiles {
		if now.Sub(file.ModTime) < orphanGrace {
			continue
		}
		if err := os.Remove(file.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return report, fmt.Errorf("remove orphan blob %s: %w", file.Path, err)
		}
		report.Repair.OrphanFilesRemoved++
	}
	if len(report.RefcountMismatches) > 0 {
		if err := idx.RecomputeRefcounts(ctx); err != nil {
			return report, err
		}
		report.Repair.RefcountsRecomputed = int64(len(report.RefcountMismatches))
	}
	if len(report.DuplicateCurrents) > 0 {
		if err := idx.FixDuplicateCurrents(ctx); err != nil {
			return report, err
		}
		report.Repair.DuplicateKeysFixed = int64(len(report.DuplicateCurrents))
	}
	if report.ExpectationOrphans > 0 {
		removed, err := idx.DeleteExpectationOrphans(ctx)
		if err != nil {
			return report, err
		}
		report.Repair.ExpectationsRemoved = removed
	}
	after, err := Reindex(ctx, st, idx)
	if err != nil {
		return report, err
	}
	after.Repair = report.Repair
	return after, nil
}

func blobFiles(st *store.Store) ([]OrphanFile, error) {
	root := st.BlobsDir()
	var files []OrphanFile
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if len(name) != 64 {
			return nil
		}
		if _, err := hex.DecodeString(name); err != nil {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		files = append(files, OrphanFile{
			Path: path, SHA256: name, Size: info.Size(), ModTime: info.ModTime(),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk blob tree: %w", err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// RebuildResult summarizes a blobs-only rebuild.
type RebuildResult struct {
	Blobs    int64    `json:"blobs"`
	Warnings []string `json:"warnings,omitempty"`
}

// RebuildBlobsOnly creates blob rows from the file tree, losing every key.
func RebuildBlobsOnly(ctx context.Context, st *store.Store, idx *index.DB) (RebuildResult, error) {
	var result RebuildResult
	count, err := idx.BlobCount(ctx)
	if err != nil {
		return result, err
	}
	objects, err := idx.AllObjects(ctx)
	if err != nil {
		return result, err
	}
	if count > 0 || len(objects) > 0 {
		return result, fmt.Errorf("rebuild-blobs-only: index already contains data; restore a snapshot or remove index.db first")
	}
	files, err := blobFiles(st)
	if err != nil {
		return result, err
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		info, err := os.Stat(file.Path)
		if err != nil {
			result.Warnings = append(result.Warnings, file.Path+": "+err.Error())
			continue
		}
		enc, rawSize, digest, err := inspectBlob(file.Path)
		if err != nil {
			result.Warnings = append(result.Warnings, file.Path+": "+err.Error())
			continue
		}
		if digest != file.SHA256 {
			result.Warnings = append(result.Warnings, file.Path+": content digest does not match the file name")
			continue
		}
		if err := idx.PutBlob(ctx, index.Blob{
			SHA256: file.SHA256, RawSize: rawSize, StoredSize: info.Size(),
			StoredEnc: string(enc), CreatedAt: info.ModTime(),
		}); err != nil {
			return result, err
		}
		result.Blobs++
	}
	return result, nil
}

func inspectBlob(path string) (store.Encoding, int64, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, "", err
	}
	defer func() { _ = file.Close() }()
	header := make([]byte, 2)
	if _, err := io.ReadFull(file, header); err != nil {
		return "", 0, "", err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", 0, "", err
	}
	hasher := sha256.New()
	var source io.Reader = file
	enc := store.EncodingIdentity
	if header[0] == 0x1f && header[1] == 0x8b {
		enc = store.EncodingGzip
		gz, err := gzip.NewReader(file)
		if err != nil {
			return "", 0, "", err
		}
		defer func() { _ = gz.Close() }()
		source = gz
	}
	size, err := io.Copy(hasher, source)
	if err != nil {
		return "", 0, "", err
	}
	return enc, size, hex.EncodeToString(hasher.Sum(nil)), nil
}

// ReconcileResult summarizes startup reconciliation.
type ReconcileResult struct {
	ObjectsRemoved     int64 `json:"objects_removed"`
	BlobRowsRemoved    int64 `json:"blob_rows_removed"`
	OrphanFilesRemoved int64 `json:"orphan_files_removed"`
}

// Reconcile removes rows with missing files and old files without rows.
func Reconcile(ctx context.Context, st *store.Store, idx *index.DB, logger *slog.Logger) (ReconcileResult, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	var result ReconcileResult
	blobs, err := idx.AllBlobs(ctx)
	if err != nil {
		return result, err
	}
	known := make(map[string]bool, len(blobs))
	for _, blob := range blobs {
		known[blob.SHA256] = true
		path, err := st.BlobPath(blob.SHA256)
		if err != nil {
			return result, err
		}
		if _, err := os.Stat(path); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return result, err
			}
			removed, err := idx.DeleteBlobAndObjects(ctx, blob.SHA256)
			if err != nil {
				return result, err
			}
			result.ObjectsRemoved += int64(len(removed))
			result.BlobRowsRemoved++
			logger.Warn("reconcile_missing_blob", "sha256", blob.SHA256, "objects", len(removed))
		}
	}
	files, err := blobFiles(st)
	if err != nil {
		return result, err
	}
	now := time.Now()
	for _, file := range files {
		if known[file.SHA256] || now.Sub(file.ModTime) < orphanGrace {
			continue
		}
		// A request may have stored the same content since the listing above
		// was taken, so the decision is repeated while publication is paused.
		err := st.Reclaim(func() error {
			if _, err := idx.GetBlob(ctx, file.SHA256); err == nil {
				return nil
			} else if !errors.Is(err, index.ErrNotFound) {
				return err
			}
			if err := os.Remove(file.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove orphan blob %s: %w", file.Path, err)
			}
			result.OrphanFilesRemoved++
			return nil
		})
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

// Warning returns the operator warning printed by rebuild-blobs-only.
func Warning(indexPath string) string {
	return fmt.Sprintf("warning: %s will contain only blob rows; keys are lost. "+
		"Recover content with larder import or by warming again.", indexPath)
}
