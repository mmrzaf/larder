package transfer

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/store"
)

// ImportOptions controls an import.
type ImportOptions struct {
	From    string
	Repo    string
	Replace bool
	Verify  bool
	DryRun  bool
	Now     time.Time
}

// ImportResult summarizes an import.
type ImportResult struct {
	Objects  int64 `json:"objects"`
	Blobs    int64 `json:"blobs"`
	Skipped  int64 `json:"skipped"`
	Failed   int64 `json:"failed"`
	Bytes    int64 `json:"bytes"`
	Verified int64 `json:"verified"`
}

// Import restores a tree or tar archive produced by Export.
func Import(ctx context.Context, st *store.Store, idx *index.DB, opts ImportOptions) (ImportResult, error) {
	if opts.From == "" {
		return ImportResult{}, fmt.Errorf("import: source is required")
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	info, err := os.Stat(opts.From)
	if err != nil {
		return ImportResult{}, fmt.Errorf("import: %w", err)
	}
	if info.IsDir() {
		return importTree(ctx, st, idx, opts)
	}
	return importTar(ctx, st, idx, opts)
}

func importTree(ctx context.Context, st *store.Store, idx *index.DB, opts ImportOptions) (ImportResult, error) {
	root, err := filepath.Abs(opts.From)
	if err != nil {
		return ImportResult{}, err
	}
	manifest, err := readTreeManifest(root)
	if err != nil {
		return ImportResult{}, err
	}
	if err := validateManifest(manifest); err != nil {
		return ImportResult{}, err
	}
	result := ImportResult{}
	imported := make(map[string]bool)
	seenBlobs := make(map[string]bool)
	for _, entry := range manifest.Objects {
		if opts.Repo != "" && entry.Repo != opts.Repo {
			continue
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		exists, err := currentExists(ctx, idx, entry)
		if err != nil {
			result.Failed++
			continue
		}
		if exists && !opts.Replace {
			result.Skipped++
			continue
		}
		path, err := safeTreePath(root, entry.Path)
		if err != nil {
			result.Failed++
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			result.Failed++
			continue
		}
		info, err := file.Stat()
		if err != nil || info.Size() != entry.Size {
			_ = file.Close()
			result.Failed++
			continue
		}
		if opts.DryRun {
			_ = file.Close()
			result.Objects++
			result.Bytes += entry.Size
			continue
		}
		importedOK, sha, err := importReader(ctx, st, idx, entry, file, info.Size(), opts.Now)
		_ = file.Close()
		if err != nil {
			result.Failed++
			continue
		}
		result.Objects++
		result.Bytes += entry.Size
		if importedOK {
			imported[sha] = true
			if !seenBlobs[sha] {
				seenBlobs[sha] = true
				result.Blobs++
			}
		}
	}
	if opts.Verify && !opts.DryRun {
		result.Verified = verifyImported(ctx, st, imported)
	}
	return result, nil
}

func importTar(ctx context.Context, st *store.Store, idx *index.DB, opts ImportOptions) (ImportResult, error) {
	manifest, err := readTarManifest(opts.From)
	if err != nil {
		return ImportResult{}, err
	}
	if err := validateManifest(manifest); err != nil {
		return ImportResult{}, err
	}
	byPath := make(map[string]ManifestEntry, len(manifest.Objects))
	for _, entry := range manifest.Objects {
		if opts.Repo != "" && entry.Repo != opts.Repo {
			continue
		}
		byPath[entry.Path] = entry
	}
	result := ImportResult{}
	imported := make(map[string]bool)
	seenBlobs := make(map[string]bool)
	file, err := os.Open(opts.From)
	if err != nil {
		return result, err
	}
	defer func() { _ = file.Close() }()
	reader := tar.NewReader(file)
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return result, fmt.Errorf("import: read archive: %w", err)
		}
		if err := validateRelative(header.Name); err != nil && header.Name != ManifestName {
			return result, fmt.Errorf("import: unsafe archive path %q", header.Name)
		}
		entry, ok := byPath[header.Name]
		if !ok {
			continue
		}
		delete(byPath, header.Name)
		exists, err := currentExists(ctx, idx, entry)
		if err != nil {
			result.Failed++
			continue
		}
		if exists && !opts.Replace {
			result.Skipped++
			continue
		}
		if header.Size != entry.Size {
			result.Failed++
			continue
		}
		if opts.DryRun {
			result.Objects++
			result.Bytes += entry.Size
			continue
		}
		importedOK, sha, err := importReader(ctx, st, idx, entry, reader, header.Size, opts.Now)
		if err != nil {
			result.Failed++
			continue
		}
		result.Objects++
		result.Bytes += entry.Size
		if importedOK {
			imported[sha] = true
			if !seenBlobs[sha] {
				seenBlobs[sha] = true
				result.Blobs++
			}
		}
	}
	for path, entry := range byPath {
		_ = path
		_ = entry
		result.Failed++
	}
	if opts.Verify && !opts.DryRun {
		result.Verified = verifyImported(ctx, st, imported)
	}
	return result, nil
}

func importReader(ctx context.Context, st *store.Store, idx *index.DB, entry ManifestEntry, source io.Reader, size int64, now time.Time) (bool, string, error) {
	writer, err := st.NewWriter(store.WriterOptions{
		Expectations: []store.Expectation{{Algo: "sha256", Digest: entry.SHA256}},
		ExpectedSize: &size,
		MaxBytes:     size,
	})
	if err != nil {
		return false, "", err
	}
	if _, err := io.Copy(writer, io.LimitReader(source, size)); err != nil {
		_ = writer.Abort()
		return false, "", err
	}
	blob, _, err := writer.Commit()
	if err != nil {
		return false, "", err
	}
	if err := idx.PutBlob(ctx, index.Blob{
		SHA256: blob.SHA256, RawSize: blob.RawSize, StoredSize: blob.StoredSize,
		StoredEnc: string(blob.StoredEnc), CreatedAt: now,
	}); err != nil {
		return false, "", err
	}
	fetched := parseTime(entry.FetchedAt, now)
	validated := parseTime(entry.ValidatedAt, fetched)
	if _, err := idx.PutObject(ctx, index.PutObjectParams{
		Repo: entry.Repo, Key: entry.Key, Class: entry.Class, SHA256: blob.SHA256,
		ContentType: entry.ContentType, Upstream: "import",
		FetchedAt: fetched, ValidatedAt: validated, Verified: entry.Verified,
	}); err != nil {
		return false, "", err
	}
	return true, blob.SHA256, nil
}

func currentExists(ctx context.Context, idx *index.DB, entry ManifestEntry) (bool, error) {
	_, err := idx.CurrentObject(ctx, entry.Repo, entry.Key)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, index.ErrNotFound) {
		return false, nil
	}
	return false, err
}

func verifyImported(ctx context.Context, st *store.Store, imported map[string]bool) int64 {
	verified := int64(0)
	for sha := range imported {
		if err := ctx.Err(); err != nil {
			return verified
		}
		reader, err := st.Open(sha, store.EncodingIdentity)
		if err != nil {
			continue
		}
		hasher := sha256.New()
		_, copyErr := io.Copy(hasher, reader)
		closeErr := reader.Close()
		if copyErr != nil || closeErr != nil {
			continue
		}
		if hex.EncodeToString(hasher.Sum(nil)) == sha {
			verified++
		}
	}
	return verified
}

func readTreeManifest(root string) (Manifest, error) {
	file, err := os.Open(filepath.Join(root, ManifestName))
	if err != nil {
		return Manifest{}, fmt.Errorf("import: manifest: %w", err)
	}
	defer func() { _ = file.Close() }()
	var manifest Manifest
	if err := json.NewDecoder(io.LimitReader(file, 64<<20)).Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("import: parse manifest: %w", err)
	}
	return manifest, nil
}

func readTarManifest(path string) (Manifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return Manifest{}, err
	}
	defer func() { _ = file.Close() }()
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return Manifest{}, fmt.Errorf("import: manifest not found in archive")
		}
		if err != nil {
			return Manifest{}, fmt.Errorf("import: read archive: %w", err)
		}
		if err := validateRelative(header.Name); err != nil && header.Name != ManifestName {
			return Manifest{}, fmt.Errorf("import: unsafe archive path %q", header.Name)
		}
		if header.Name != ManifestName {
			continue
		}
		var manifest Manifest
		if err := json.NewDecoder(io.LimitReader(reader, 64<<20)).Decode(&manifest); err != nil {
			return Manifest{}, fmt.Errorf("import: parse manifest: %w", err)
		}
		return manifest, nil
	}
}

func validateManifest(manifest Manifest) error {
	if manifest.DataVersion != DataVersion {
		return fmt.Errorf("import: unsupported data version %d", manifest.DataVersion)
	}
	seen := make(map[string]bool)
	for _, entry := range manifest.Objects {
		expected, err := objectPath(entry.Repo, entry.Key)
		if err != nil {
			return fmt.Errorf("import: %w", err)
		}
		if entry.Path != expected {
			return fmt.Errorf("import: manifest path %q does not match %q", entry.Path, expected)
		}
		if err := validateRelative(entry.Path); err != nil {
			return fmt.Errorf("import: %w", err)
		}
		if len(entry.SHA256) != 64 {
			return fmt.Errorf("import: invalid digest for %s/%s", entry.Repo, entry.Key)
		}
		if _, err := hex.DecodeString(entry.SHA256); err != nil {
			return fmt.Errorf("import: invalid digest for %s/%s", entry.Repo, entry.Key)
		}
		if entry.Size <= 0 {
			return fmt.Errorf("import: invalid size for %s/%s", entry.Repo, entry.Key)
		}
		if seen[entry.Path] {
			return fmt.Errorf("import: duplicate path %q", entry.Path)
		}
		seen[entry.Path] = true
	}
	return nil
}

func safeTreePath(root, relative string) (string, error) {
	if err := validateRelative(relative); err != nil {
		return "", err
	}
	path := filepath.Join(root, filepath.FromSlash(relative))
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(absolute, rootAbs+string(os.PathSeparator)) {
		return "", fmt.Errorf("unsafe path %q", relative)
	}
	if evaluated, err := filepath.EvalSymlinks(absolute); err == nil {
		if !strings.HasPrefix(evaluated, rootAbs+string(os.PathSeparator)) {
			return "", fmt.Errorf("unsafe path %q", relative)
		}
	}
	return absolute, nil
}

func parseTime(value string, fallback time.Time) time.Time {
	if value == "" {
		return fallback
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return fallback
	}
	return parsed
}
