package transfer

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/kinds"
	"github.com/mmrzaf/larder/internal/store"
)

// ExportOptions selects objects and the output format.
type ExportOptions struct {
	To            string
	Format        string
	Repo          string
	Match         string
	Pinned        bool
	OlderThanDays int
	BaseURL       string
	PinnedFunc    func(repo, key string) (bool, error)
	Now           time.Time
}

// ExportResult summarizes an export.
type ExportResult struct {
	Format   string `json:"format"`
	To       string `json:"to"`
	Objects  int64  `json:"objects"`
	Bytes    int64  `json:"bytes"`
	Manifest string `json:"manifest"`
}

// Export writes a static tree or tar archive with a manifest.
func Export(ctx context.Context, st *store.Store, idx *index.DB, cfg *config.Config, registry *kinds.Registry, opts ExportOptions) (ExportResult, error) {
	if opts.To == "" {
		return ExportResult{}, fmt.Errorf("export: destination is required")
	}
	format := opts.Format
	if format == "" {
		format = "tree"
	}
	if format != "tree" && format != "tar" {
		return ExportResult{}, fmt.Errorf("export: format must be tree or tar")
	}
	if opts.OlderThanDays < 0 {
		return ExportResult{}, fmt.Errorf("export: older-than-days must not be negative")
	}
	if opts.Pinned && opts.PinnedFunc == nil {
		return ExportResult{}, fmt.Errorf("export: pinned selection is unavailable")
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	base := strings.TrimSuffix(opts.BaseURL, "/")
	if base == "" {
		base = strings.TrimSuffix(cfg.PublicBaseURL, "/")
	}
	bases := make(map[string]string, len(cfg.Repos))
	for i := range cfg.Repos {
		repoBase := base
		if opts.BaseURL == "" {
			if public := strings.TrimSuffix(cfg.Repos[i].PublicURL, "/"); public != "" {
				repoBase = public
			}
		}
		bases[cfg.Repos[i].Name] = repoBase
	}
	objects, err := idx.CurrentObjects(ctx, opts.Repo)
	if err != nil {
		return ExportResult{}, err
	}
	kindsByRepo := make(map[string]kinds.Kind)
	reposByKind := make(map[string]config.Repo)
	for i := range cfg.Repos {
		repo := cfg.Repos[i]
		reposByKind[repo.Name] = repo
	}
	for name, repo := range reposByKind {
		kind, err := registry.New(repo)
		if err != nil {
			return ExportResult{}, fmt.Errorf("export: %s: %w", name, err)
		}
		kindsByRepo[name] = kind
	}
	filtered := make([]index.Candidate, 0, len(objects))
	seenRepo := make(map[string]bool)
	for _, object := range objects {
		if opts.Match != "" && !matchKey(opts.Match, object.Key) {
			continue
		}
		if opts.OlderThanDays > 0 {
			access := object.LastAccess
			if access.IsZero() {
				access = object.FetchedAt
			}
			if !access.Before(opts.Now.AddDate(0, 0, -opts.OlderThanDays)) {
				continue
			}
		}
		if opts.Pinned {
			pinned, err := opts.PinnedFunc(object.Repo, object.Key)
			if err != nil {
				return ExportResult{}, err
			}
			if !pinned {
				continue
			}
		}
		filtered = append(filtered, object)
		seenRepo[object.Repo] = true
	}

	manifest := Manifest{DataVersion: DataVersion, BaseURL: base, ExportedAt: opts.Now.UTC().Format(time.RFC3339)}
	for i := range cfg.Repos {
		if !seenRepo[cfg.Repos[i].Name] {
			continue
		}
		manifest.Repos = append(manifest.Repos, ManifestRepo{
			Name: cfg.Repos[i].Name, Kind: cfg.Repos[i].Kind, Mount: cfg.Repos[i].Mount,
		})
	}
	result := ExportResult{Format: format, To: opts.To, Manifest: ManifestName}
	if format == "tar" {
		if err := os.MkdirAll(filepath.Dir(opts.To), 0o750); err != nil {
			return ExportResult{}, fmt.Errorf("export: create directory: %w", err)
		}
		if err := exportTar(ctx, st, kindsByRepo, bases, filtered, opts, &manifest, &result); err != nil {
			return ExportResult{}, err
		}
	} else {
		if err := os.MkdirAll(opts.To, 0o750); err != nil {
			return ExportResult{}, fmt.Errorf("export: create destination: %w", err)
		}
		if err := exportTree(ctx, st, kindsByRepo, bases, filtered, opts, &manifest, &result); err != nil {
			return ExportResult{}, err
		}
	}
	return result, nil
}

func matchKey(pattern, key string) bool {
	if !strings.Contains(pattern, "#") {
		if index := strings.IndexByte(key, '#'); index >= 0 {
			key = key[:index]
		}
	}
	if index.MatchGlob(pattern, key) {
		return true
	}
	return len(key) > 1 && key[0] == '/' && index.MatchGlob(pattern, key[1:])
}

type prepared struct {
	candidate index.Candidate
	rel       string
	reader    func() (io.ReadCloser, error)
	size      int64
	digest    string
	rewritten bool
	hardlink  string
}

func prepareObject(ctx context.Context, st *store.Store, kind kinds.Kind, base string, object index.Candidate) (prepared, error) {
	rel, err := objectPath(object.Repo, object.Key)
	if err != nil {
		return prepared{}, err
	}
	item := prepared{candidate: object, rel: rel, size: object.RawSize, digest: object.SHA256}
	classifyKey := object.Key
	if index := strings.IndexByte(classifyKey, '#'); index >= 0 {
		classifyKey = classifyKey[:index]
	}
	plan, err := kind.Classify(kinds.Request{Method: "GET", Path: classifyKey, PublicBase: base})
	if err != nil {
		return prepared{}, err
	}
	if plan.Hooks.Rewrite != nil && object.Class == "mutable" && base != "" {
		reader, err := st.Open(object.SHA256, store.Encoding(object.StoredEnc))
		if err != nil {
			return prepared{}, err
		}
		raw, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil {
			return prepared{}, readErr
		}
		if closeErr != nil {
			return prepared{}, closeErr
		}
		rewritten, err := plan.Hooks.Rewrite(raw, base)
		if err != nil {
			return prepared{}, err
		}
		sum := sha256.Sum256(rewritten)
		item.size = int64(len(rewritten))
		item.digest = hex.EncodeToString(sum[:])
		item.rewritten = true
		item.reader = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(rewritten)), nil }
		return item, nil
	}
	item.reader = func() (io.ReadCloser, error) {
		return st.Open(object.SHA256, store.Encoding(object.StoredEnc))
	}
	if object.StoredEnc == "identity" && !item.rewritten {
		path, err := st.BlobPath(object.SHA256)
		if err == nil {
			item.hardlink = path
		}
	}
	return item, nil
}

func exportTree(ctx context.Context, st *store.Store, kindsByRepo map[string]kinds.Kind, bases map[string]string, objects []index.Candidate, opts ExportOptions, manifest *Manifest, result *ExportResult) error {
	for _, object := range objects {
		if err := ctx.Err(); err != nil {
			return err
		}
		kind := kindsByRepo[object.Repo]
		item, err := prepareObject(ctx, st, kind, bases[object.Repo], object)
		if err != nil {
			return fmt.Errorf("export %s/%s: %w", object.Repo, object.Key, err)
		}
		destination := filepath.Join(opts.To, filepath.FromSlash(item.rel))
		if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
			return fmt.Errorf("export: create %s: %w", filepath.Dir(destination), err)
		}
		if err := os.Remove(destination); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("export: replace %s: %w", destination, err)
		}
		if item.hardlink != "" && !item.rewritten {
			if err := os.Link(item.hardlink, destination); err != nil {
				if err := copyToFile(item.reader, destination, item.size); err != nil {
					return err
				}
			}
		} else {
			if err := copyToFile(item.reader, destination, item.size); err != nil {
				return err
			}
		}
		manifest.Objects = append(manifest.Objects, manifestEntry(item))
		result.Objects++
		result.Bytes += item.size
	}
	return writeManifestFile(filepath.Join(opts.To, ManifestName), manifest)
}

func exportTar(ctx context.Context, st *store.Store, kindsByRepo map[string]kinds.Kind, bases map[string]string, objects []index.Candidate, opts ExportOptions, manifest *Manifest, result *ExportResult) error {
	file, err := os.OpenFile(opts.To, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return fmt.Errorf("export: create archive: %w", err)
	}
	defer func() { _ = file.Close() }()
	writer := tar.NewWriter(file)
	defer func() { _ = writer.Close() }()
	for _, object := range objects {
		if err := ctx.Err(); err != nil {
			return err
		}
		kind := kindsByRepo[object.Repo]
		item, err := prepareObject(ctx, st, kind, bases[object.Repo], object)
		if err != nil {
			return fmt.Errorf("export %s/%s: %w", object.Repo, object.Key, err)
		}
		header := &tar.Header{
			Name: item.rel, Mode: 0o640, Size: item.size,
			ModTime: object.FetchedAt, Typeflag: tar.TypeReg,
		}
		if err := writer.WriteHeader(header); err != nil {
			return fmt.Errorf("export: write header: %w", err)
		}
		reader, err := item.reader()
		if err != nil {
			return err
		}
		written, copyErr := io.Copy(writer, reader)
		closeErr := reader.Close()
		if copyErr != nil {
			return fmt.Errorf("export: write %s: %w", item.rel, copyErr)
		}
		if closeErr != nil {
			return closeErr
		}
		if written != item.size {
			return fmt.Errorf("export: %s has %d bytes, expected %d", item.rel, written, item.size)
		}
		manifest.Objects = append(manifest.Objects, manifestEntry(item))
		result.Objects++
		result.Bytes += item.size
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("export: encode manifest: %w", err)
	}
	data = append(data, '\n')
	header := &tar.Header{Name: ManifestName, Mode: 0o640, Size: int64(len(data)), ModTime: opts.Now, Typeflag: tar.TypeReg}
	if err := writer.WriteHeader(header); err != nil {
		return fmt.Errorf("export: write manifest header: %w", err)
	}
	if _, err := writer.Write(data); err != nil {
		return fmt.Errorf("export: write manifest: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("export: close archive: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("export: sync archive: %w", err)
	}
	return nil
}

func manifestEntry(item prepared) ManifestEntry {
	return ManifestEntry{
		Repo: item.candidate.Repo, Key: item.candidate.Key, Path: item.rel,
		Class: item.candidate.Class, SHA256: item.digest, Size: item.size,
		ContentType: item.candidate.ContentType, Verified: item.candidate.Verified,
		FetchedAt:   item.candidate.FetchedAt.UTC().Format(time.RFC3339),
		ValidatedAt: item.candidate.ValidatedAt.UTC().Format(time.RFC3339),
	}
}

func copyToFile(open func() (io.ReadCloser, error), destination string, size int64) error {
	reader, err := open()
	if err != nil {
		return fmt.Errorf("export: open blob: %w", err)
	}
	defer func() { _ = reader.Close() }()
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return fmt.Errorf("export: create %s: %w", destination, err)
	}
	written, err := io.Copy(file, reader)
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("export: write %s: %w", destination, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("export: sync %s: %w", destination, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("export: close %s: %w", destination, err)
	}
	if written != size {
		return fmt.Errorf("export: %s has %d bytes, expected %d", destination, written, size)
	}
	return nil
}

func writeManifestFile(path string, manifest *Manifest) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return fmt.Errorf("export: create manifest: %w", err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(manifest); err != nil {
		_ = file.Close()
		return fmt.Errorf("export: write manifest: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("export: sync manifest: %w", err)
	}
	return file.Close()
}
