package transfer

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/integrity"
	"github.com/mmrzaf/larder/internal/kinds"
	"github.com/mmrzaf/larder/internal/store"
)

// IngestOptions controls local directory ingestion.
type IngestOptions struct {
	DryRun bool
	Now    time.Time
}

// IngestResult summarizes ingestion.
type IngestResult struct {
	Stored  int64    `json:"stored"`
	Bytes   int64    `json:"bytes"`
	Skipped int64    `json:"skipped"`
	Denied  int64    `json:"denied"`
	Failed  int64    `json:"failed"`
	Errors  []string `json:"errors,omitempty"`
}

// Ingest stores immutable files from a directory laid out like a client cache.
func Ingest(ctx context.Context, st *store.Store, idx *index.DB, repo config.Repo, kind kinds.Kind, dir string, opts IngestOptions) (IngestResult, error) {
	if dir == "" {
		return IngestResult{}, fmt.Errorf("ingest: directory is required")
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	info, err := os.Stat(dir)
	if err != nil {
		return IngestResult{}, err
	}
	if !info.IsDir() {
		return IngestResult{}, fmt.Errorf("ingest: %s is not a directory", dir)
	}
	var files []string
	err = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return IngestResult{}, err
	}
	sort.Strings(files)
	result := IngestResult{}
	seen := make(map[string]bool)
	for _, path := range files {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			result.Failed++
			continue
		}
		key := "/" + filepath.ToSlash(rel)
		base := filepath.Base(path)
		if base == "_remote.repositories" || base == "resolver-status.properties" || strings.HasSuffix(base, ".lastUpdated") {
			result.Skipped++
			continue
		}
		if strings.Contains(key, "\\") || seen[key] {
			result.Denied++
			continue
		}
		seen[key] = true
		plan, err := kind.Classify(kinds.Request{Method: "GET", Path: key})
		if err != nil {
			result.Denied++
			continue
		}
		switch plan.Class {
		case kinds.Immutable:
		case kinds.Mutable:
			result.Skipped++
			continue
		default:
			result.Denied++
			continue
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			result.Failed++
			continue
		}
		if info.Size() == 0 {
			result.Failed++
			result.Errors = append(result.Errors, key+": empty file")
			continue
		}
		if repo.MaxObjectSize > 0 && info.Size() > repo.MaxObjectSize.Bytes() {
			result.Failed++
			result.Errors = append(result.Errors, key+": exceeds max_object_size")
			continue
		}
		source, err := os.Open(path)
		if err != nil {
			result.Failed++
			result.Errors = append(result.Errors, key+": "+err.Error())
			continue
		}
		if plan.Hooks.Validate != nil {
			validateErr := plan.Hooks.Validate(ctx, source, info.Size())
			if validateErr != nil {
				_ = source.Close()
				result.Failed++
				result.Errors = append(result.Errors, key+": "+validateErr.Error())
				continue
			}
			if _, err := source.Seek(0, io.SeekStart); err != nil {
				_ = source.Close()
				result.Failed++
				continue
			}
		}
		expectations := localExpectations(path, key, plan)
		if opts.DryRun {
			_ = source.Close()
			result.Stored++
			result.Bytes += info.Size()
			continue
		}
		writerExpectations := make([]store.Expectation, 0, len(expectations))
		for _, expectation := range expectations {
			writerExpectations = append(writerExpectations, store.Expectation{Algo: expectation.Algo, Digest: expectation.Hex})
		}
		size := info.Size()
		writer, err := st.NewWriter(store.WriterOptions{
			Expectations: writerExpectations, ExpectedSize: &size, MaxBytes: size,
		})
		if err != nil {
			_ = source.Close()
			result.Failed++
			result.Errors = append(result.Errors, key+": "+err.Error())
			continue
		}
		_, copyErr := io.Copy(writer, source)
		closeErr := source.Close()
		if copyErr != nil {
			_ = writer.Abort()
			result.Failed++
			result.Errors = append(result.Errors, key+": "+copyErr.Error())
			continue
		}
		if closeErr != nil {
			_ = writer.Abort()
			result.Failed++
			result.Errors = append(result.Errors, key+": "+closeErr.Error())
			continue
		}
		blob, _, err := writer.Commit()
		if err != nil {
			result.Failed++
			result.Errors = append(result.Errors, key+": "+err.Error())
			continue
		}
		if err := idx.PutBlob(ctx, index.Blob{
			SHA256: blob.SHA256, RawSize: blob.RawSize, StoredSize: blob.StoredSize,
			StoredEnc: string(blob.StoredEnc), CreatedAt: opts.Now,
		}); err != nil {
			result.Failed++
			result.Errors = append(result.Errors, key+": "+err.Error())
			continue
		}
		_, err = idx.PutObject(ctx, index.PutObjectParams{
			Repo: repo.Name, Key: plan.Key, Class: "immutable", SHA256: blob.SHA256,
			ContentType: contentTypeFor(path), Upstream: "ingest",
			FetchedAt: opts.Now, ValidatedAt: opts.Now, Verified: len(expectations) > 0,
		})
		if err != nil {
			result.Failed++
			result.Errors = append(result.Errors, key+": "+err.Error())
			continue
		}
		result.Stored++
		result.Bytes += info.Size()
	}
	return result, nil
}

func localExpectations(path, key string, plan kinds.Plan) []integrity.Digest {
	var out []integrity.Digest
	if plan.Hooks.Expect != nil {
		out = append(out, plan.Hooks.Expect(key)...)
	}
	for _, suffix := range []string{".sha512", ".sha256", ".sha1", ".md5"} {
		companion := path + suffix
		digest, ok := readCompanion(companion)
		if ok {
			out = append(out, digest)
			break
		}
	}
	return out
}

func readCompanion(path string) (integrity.Digest, bool) {
	file, err := os.Open(path)
	if err != nil {
		return integrity.Digest{}, false
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(io.LimitReader(file, 4096))
	if !scanner.Scan() {
		return integrity.Digest{}, false
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) == 0 {
		return integrity.Digest{}, false
	}
	algo := strings.TrimPrefix(filepath.Ext(path), ".")
	digest, err := integrity.ParseHex(algo, fields[0])
	if err != nil {
		return integrity.Digest{}, false
	}
	return digest, true
}

func contentTypeFor(path string) string {
	if value := mime.TypeByExtension(filepath.Ext(path)); value != "" {
		return value
	}
	return "application/octet-stream"
}
