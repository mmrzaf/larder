package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/kinds"
	"github.com/mmrzaf/larder/internal/store"
)

func (e *Engine) handleStored(ctx context.Context, rt *repoRuntime, plan kinds.Plan, key string, object index.Object, w http.ResponseWriter, r *http.Request, meta *responseMeta) {
	if plan.Class == kinds.Immutable {
		cache := "HIT"
		if e.offline.Load() {
			cache = "OFFLINE-HIT"
		}
		e.serveObject(ctx, rt, plan, key, object, cache, false, w, r, meta)
		return
	}

	now := e.clock.Now()
	age := time.Duration(1 << 62)
	if !object.ValidatedAt.IsZero() {
		age = now.Sub(object.ValidatedAt)
		if age < 0 {
			age = 0
		}
	}
	ttl := rt.cfg.FreshTTL.Duration()
	if plan.FreshTTL > 0 {
		ttl = plan.FreshTTL
	}
	if e.offline.Load() {
		e.serveObject(ctx, rt, plan, key, object, "OFFLINE-HIT", true, w, r, meta)
		return
	}
	if age < ttl {
		e.serveObject(ctx, rt, plan, key, object, "HIT", false, w, r, meta)
		return
	}
	if e.cfg.Cache.MaxStale > 0 && age > e.cfg.Cache.MaxStale.Duration() {
		e.revalidate(ctx, rt, plan, key, object, w, r, meta, false)
		return
	}
	if e.cfg.Cache.StaleWhileRevalidate > 0 && age-ttl < e.cfg.Cache.StaleWhileRevalidate.Duration() {
		e.serveObject(ctx, rt, plan, key, object, "STALE", true, w, r, meta)
		e.startRefresh(rt, plan, key)
		return
	}
	e.revalidate(ctx, rt, plan, key, object, w, r, meta, true)
}

func (e *Engine) serveObject(ctx context.Context, rt *repoRuntime, plan kinds.Plan, key string, object index.Object, cache string, stale bool, w http.ResponseWriter, r *http.Request, meta *responseMeta) {
	blob, err := e.index.GetBlob(ctx, object.SHA256)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "MISS", "blob metadata unavailable")
		return
	}
	if e.checkStored(rt, object, blob) != storedOK {
		writeError(w, http.StatusInternalServerError, cache, "stored object failed its size check")
		return
	}
	meta.cache = cache
	meta.upstream = object.Upstream
	meta.stale = stale
	meta.verified = object.Verified
	if cache == "GONE" {
		if meta.goneStatus == 0 {
			meta.goneStatus = http.StatusNotFound
		}
		w.Header().Set("X-Larder-Upstream-Status", strconv.Itoa(meta.goneStatus))
	} else {
		meta.goneStatus = 0
	}
	e.setStoredHeaders(w, rt, plan, key, object, cache, stale)
	e.access.Record(object.ID)

	if plan.Hooks.Rewrite != nil {
		e.serveRewritten(ctx, rt, plan, object, blob, cache, w, r, meta)
		return
	}
	if e.notModified(w, r, object, "") {
		return
	}
	e.serveBlob(w, r, object, blob, cache, stale)
}

type storedState int

const (
	storedOK storedState = iota
	// storedBroken means the blob file is gone or its size contradicts the index.
	storedBroken
	// storedUnreadable means the file could not be examined. The entry is kept
	// because the cause may be a permission or device problem rather than
	// damage to the data.
	storedUnreadable
)

func (e *Engine) checkStored(rt *repoRuntime, object index.Object, blob index.Blob) storedState {
	if err := blobSane(blob); err != nil {
		e.logger.Error("verify_failed", "repo", rt.cfg.Name, "key", object.Key,
			"sha256", object.SHA256, "reason", "metadata", "error", err)
		e.verifyFailure(rt.cfg.Name)
		return storedBroken
	}
	size, err := e.store.SizeOnDisk(object.SHA256)
	if err != nil {
		e.logger.Error("verify_failed", "repo", rt.cfg.Name, "key", object.Key,
			"sha256", object.SHA256, "reason", "unreadable", "error", err)
		e.verifyFailure(rt.cfg.Name)
		if errors.Is(err, store.ErrNotFound) {
			return storedBroken
		}
		return storedUnreadable
	}
	if size != blob.StoredSize {
		e.logger.Error("verify_failed", "repo", rt.cfg.Name, "key", object.Key,
			"sha256", object.SHA256, "reason", "size", "stored", size,
			"expected", blob.StoredSize)
		e.verifyFailure(rt.cfg.Name)
		return storedBroken
	}
	return storedOK
}

// blobSane reports whether the row describes a non-empty file whose sizes are
// self-consistent. A row that fails this check is treated as damaged even when
// the file on disk happens to match the recorded stored size.
func blobSane(blob index.Blob) error {
	if blob.RawSize <= 0 {
		return fmt.Errorf("raw size is %d", blob.RawSize)
	}
	if blob.StoredSize <= 0 {
		return fmt.Errorf("stored size is %d", blob.StoredSize)
	}
	switch blob.StoredEnc {
	case "identity":
		if blob.StoredSize != blob.RawSize {
			return fmt.Errorf("identity stored size %d differs from raw size %d", blob.StoredSize, blob.RawSize)
		}
	case "gzip":
	default:
		return fmt.Errorf("unknown encoding %q", blob.StoredEnc)
	}
	return nil
}

// storedIntact reports whether the blob behind an object can be served. When
// it cannot, the object is dropped so the request is handled as a miss and the
// content is fetched again instead of failing for every later client.
func (e *Engine) storedIntact(ctx context.Context, rt *repoRuntime, object index.Object) bool {
	blob, err := e.index.GetBlob(ctx, object.SHA256)
	switch {
	case errors.Is(err, index.ErrNotFound):
		e.logger.Error("verify_failed", "repo", rt.cfg.Name, "key", object.Key,
			"sha256", object.SHA256, "reason", "blob row missing")
		e.verifyFailure(rt.cfg.Name)
	case err != nil:
		return true
	default:
		switch e.checkStored(rt, object, blob) {
		case storedOK, storedUnreadable:
			return true
		}
	}
	err = e.store.Reclaim(func() error {
		if _, err := e.index.DeleteBlobAndObjects(ctx, object.SHA256); err != nil {
			return err
		}
		return e.store.Quarantine(object.SHA256)
	})
	if err != nil {
		e.logger.Error("discard_broken_failed", "repo", rt.cfg.Name, "key", object.Key,
			"sha256", object.SHA256, "error", err)
		return true
	}
	e.logger.Warn("discarded_broken_object", "repo", rt.cfg.Name, "key", object.Key, "sha256", object.SHA256)
	return false
}

func (e *Engine) serveRewritten(ctx context.Context, rt *repoRuntime, plan kinds.Plan, object index.Object, blob index.Blob, cache string, w http.ResponseWriter, r *http.Request, meta *responseMeta) {
	reader, err := e.store.Open(object.SHA256, store.Encoding(blob.StoredEnc))
	if err != nil {
		writeError(w, http.StatusInternalServerError, cache, "blob unavailable")
		return
	}
	if reader.StoredSize() != blob.StoredSize {
		_ = reader.Close()
		writeError(w, http.StatusInternalServerError, cache, "stored object size changed")
		return
	}
	limit := e.cfg.Limits.MaxParseBytes.Bytes()
	raw, err := io.ReadAll(io.LimitReader(reader, limit+1))
	closeErr := reader.Close()
	if err != nil || closeErr != nil {
		writeError(w, http.StatusInternalServerError, cache, "blob read failed")
		return
	}
	if int64(len(raw)) > limit {
		writeError(w, http.StatusBadGateway, cache, "document exceeds parse limit")
		return
	}
	publicBase := e.publicBase(ctx, rt.cfg, r)
	rewritten, err := plan.Hooks.Rewrite(raw, publicBase)
	if err != nil {
		writeError(w, http.StatusBadGateway, cache, "document rewrite failed")
		return
	}
	sum := sha256.Sum256(rewritten)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	if ct := w.Header().Get("Content-Type"); ct == "" {
		w.Header().Set("Content-Type", contentTypeFor(plan, object.ContentType))
	}
	w.Header().Set("ETag", etag)
	if e.notModified(w, r, object, etag) {
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(rewritten)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if n, err := w.Write(rewritten); err != nil || n != len(rewritten) {
		panic(http.ErrAbortHandler)
	}
}

func (e *Engine) serveBlob(w http.ResponseWriter, r *http.Request, object index.Object, blob index.Blob, cache string, stale bool) {
	rangeHeader := r.Header.Get("Range")
	if rangeHeader != "" {
		start, length, ok, unsat := parseRange(rangeHeader, blob.RawSize)
		if unsat {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", blob.RawSize))
			w.Header().Set("Content-Length", "0")
			w.Header().Set("X-Larder-Cache", cache)
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if ok {
			reader, err := e.store.OpenRange(object.SHA256, store.Encoding(blob.StoredEnc), start, length)
			if err != nil {
				writeError(w, http.StatusInternalServerError, cache, "range unavailable")
				return
			}
			defer func() { _ = reader.Close() }()
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+length-1, blob.RawSize))
			w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("X-Larder-Cache", cache)
			w.WriteHeader(http.StatusPartialContent)
			if r.Method == http.MethodHead {
				return
			}
			n, err := io.Copy(w, reader)
			if err != nil || n != length {
				panic(http.ErrAbortHandler)
			}
			return
		}
	}

	if blob.StoredEnc == "gzip" && acceptsGzip(r) {
		file, err := e.store.OpenStored(object.SHA256)
		if err != nil {
			writeError(w, http.StatusInternalServerError, cache, "blob unavailable")
			return
		}
		defer func() { _ = file.Close() }()
		info, err := file.Stat()
		if err != nil || info.Size() != blob.StoredSize {
			writeError(w, http.StatusInternalServerError, cache, "stored object size changed")
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		addVary(w.Header(), "Accept-Encoding")
		w.Header().Set("Content-Length", strconv.FormatInt(blob.StoredSize, 10))
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("X-Larder-Cache", cache)
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		n, err := io.Copy(w, file)
		if err != nil || n != blob.StoredSize {
			panic(http.ErrAbortHandler)
		}
		return
	}

	reader, err := e.store.Open(object.SHA256, store.Encoding(blob.StoredEnc))
	if err != nil {
		writeError(w, http.StatusInternalServerError, cache, "blob unavailable")
		return
	}
	defer func() { _ = reader.Close() }()
	if reader.StoredSize() != blob.StoredSize {
		writeError(w, http.StatusInternalServerError, cache, "stored object size changed")
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(blob.RawSize, 10))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("X-Larder-Cache", cache)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	n, err := io.Copy(w, reader)
	if err != nil || n != blob.RawSize {
		panic(http.ErrAbortHandler)
	}
}

func (e *Engine) setStoredHeaders(w http.ResponseWriter, rt *repoRuntime, plan kinds.Plan, key string, object index.Object, cache string, stale bool) {
	w.Header().Set("X-Larder-Cache", cache)
	w.Header().Set("X-Larder-Key", rt.cfg.Name+":"+key)
	w.Header().Set("X-Larder-Verified", boolString(object.Verified))
	if object.Upstream != "" {
		w.Header().Set("X-Larder-Upstream", object.Upstream)
	}
	if stale {
		w.Header().Set("X-Larder-Stale", "1")
	}
	age := int64(0)
	if !object.ValidatedAt.IsZero() {
		age = int64(e.clock.Now().Sub(object.ValidatedAt).Seconds())
		if age < 0 {
			age = 0
		}
	}
	w.Header().Set("X-Larder-Age", strconv.FormatInt(age, 10))
	if object.ETag != "" {
		w.Header().Set("ETag", object.ETag)
	}
	if object.LastModified != "" {
		w.Header().Set("Last-Modified", object.LastModified)
	}
	w.Header().Set("Content-Type", contentTypeFor(plan, object.ContentType))
	if plan.Variant != "" {
		addVary(w.Header(), "Accept")
	}
	if plan.Class == kinds.Immutable {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
	}
}

func (e *Engine) notModified(w http.ResponseWriter, r *http.Request, object index.Object, derivedETag string) bool {
	etag := object.ETag
	if derivedETag != "" {
		etag = derivedETag
	}
	if etag != "" && etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	if ims := r.Header.Get("If-Modified-Since"); ims != "" && object.LastModified != "" {
		if since, err := http.ParseTime(ims); err == nil {
			if modified, err := http.ParseTime(object.LastModified); err == nil && !modified.After(since) {
				w.WriteHeader(http.StatusNotModified)
				return true
			}
		}
	}
	return false
}

func addVary(header http.Header, value string) {
	for _, existing := range header.Values("Vary") {
		for _, item := range strings.Split(existing, ",") {
			if strings.EqualFold(strings.TrimSpace(item), value) {
				return
			}
		}
	}
	header.Add("Vary", value)
}

func contentTypeFor(plan kinds.Plan, stored string) string {
	if stored != "" {
		return stored
	}
	if plan.Binary {
		return "application/octet-stream"
	}
	return "text/plain; charset=utf-8"
}

func boolString(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		part = strings.TrimSpace(part)
		encoding, params, _ := strings.Cut(part, ";")
		if !strings.EqualFold(strings.TrimSpace(encoding), "gzip") {
			continue
		}
		for _, param := range strings.Split(params, ";") {
			param = strings.ReplaceAll(strings.TrimSpace(param), " ", "")
			if value, ok := strings.CutPrefix(param, "q="); ok {
				if weight, err := strconv.ParseFloat(value, 64); err == nil && weight <= 0 {
					return false
				}
			}
		}
		return true
	}
	return false
}

func etagMatches(header, etag string) bool {
	if header == "" {
		return false
	}
	for _, item := range strings.Split(header, ",") {
		item = strings.TrimSpace(item)
		if item == "*" || strings.TrimPrefix(item, "W/") == etag {
			return true
		}
	}
	return false
}

func parseRange(value string, size int64) (start, length int64, ok, unsatisfiable bool) {
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		return 0, 0, false, false
	}
	spec := strings.TrimSpace(strings.TrimPrefix(value, "bytes="))
	first, last, hasLast := strings.Cut(spec, "-")
	if !hasLast {
		return 0, 0, false, false
	}
	if first == "" {
		suffix, err := strconv.ParseInt(last, 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, false, true
		}
		if suffix > size {
			suffix = size
		}
		return size - suffix, suffix, true, false
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 {
		return 0, 0, false, true
	}
	if start >= size {
		return 0, 0, false, true
	}
	end := size - 1
	if last != "" {
		end, err = strconv.ParseInt(last, 10, 64)
		if err != nil || end < start {
			return 0, 0, false, true
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end - start + 1, true, false
}
