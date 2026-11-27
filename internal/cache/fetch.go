package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/integrity"
	"github.com/mmrzaf/larder/internal/kinds"
	"github.com/mmrzaf/larder/internal/store"
	"github.com/mmrzaf/larder/internal/upstream"
)

func (e *Engine) fetchImmutable(ctx context.Context, rt *repoRuntime, plan kinds.Plan, key string, w http.ResponseWriter, r *http.Request, meta *responseMeta) flightResult {
	targets, err := e.targets(rt, plan, nil)
	if err != nil {
		return flightResult{err: err, status: http.StatusBadGateway}
	}
	expectations, verifiedKnown, err := e.expectations(e.ctx, rt.cfg.Name, key, plan.Hooks.Expect)
	if err != nil {
		return flightResult{err: err, status: http.StatusInternalServerError}
	}
	companions := plan.Hooks.Companions
	var comps []kinds.Companion
	if companions != nil {
		comps = companions(key)
	}
	tee := r.Method == http.MethodGet && len(targets) == 1 && len(expectations) == 0 && !rt.cfg.RequireVerified &&
		len(comps) == 0 && r.Header.Get("Range") == "" && plan.Hooks.Validate == nil && plan.Hooks.Verify == nil

	var object index.Object
	var used string
	var verified bool
	written := false

	_, err = e.fetcher.Fetch(e.ctx, http.MethodGet, targets, func(resp *upstream.Response) error {
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("unexpected status %d", resp.StatusCode)
		}
		stored, storedBlob, upstreamName, ok, stop, err := e.storeImmutableResponse(e.ctx, rt, plan, key, resp, expectations, verifiedKnown, comps, tee, w, r, meta)
		if storedBlob.SHA256 != "" {
			object, used, verified = stored, upstreamName, ok
		}
		if stop || (tee && w.Header().Get("X-Larder-Cache") != "") {
			written = true
		}
		if err != nil {
			if written {
				return fmt.Errorf("%w: %w", upstream.ErrStop, err)
			}
			return err
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, upstream.ErrStop) {
			return flightResult{err: err, status: failureStatus(err), cache: "MISS", written: written, object: object, upstream: used, verified: verified}
		}
		if errors.Is(err, upstream.ErrNotFound) {
			var nf *upstream.NotFoundError
			status := http.StatusNotFound
			body := []byte(nil)
			if errors.As(err, &nf) {
				if nf.Status != 0 {
					status = nf.Status
				}
				body = nf.Body
			}
			e.negative.put(rt.cfg.Name+"\x00"+key, rt.cfg.NegativeTTL.Duration(), status, body)
			return flightResult{err: err, status: status, cache: "MISS", body: body}
		}
		return flightResult{err: err, status: failureStatus(err), cache: "MISS"}
	}
	if written {
		return flightResult{cache: "MISS", upstream: used, object: object, verified: verified, written: true}
	}
	return flightResult{cache: "MISS", upstream: used, object: object, verified: verified}
}

func (e *Engine) storeImmutableResponse(ctx context.Context, rt *repoRuntime, plan kinds.Plan, key string, resp *upstream.Response, expectations []store.Expectation, verifiedKnown bool, companions []kinds.Companion, tee bool, w http.ResponseWriter, r *http.Request, meta *responseMeta) (index.Object, index.Blob, string, bool, bool, error) {
	contentType := resp.Header.Get("Content-Type")
	if plan.Binary && isHTML(contentType) {
		return index.Object{}, index.Blob{}, "", false, false, fmt.Errorf("rejected text/html binary body")
	}
	if e.bypassActive(rt.cfg.Name) || (resp.ContentLength >= 0 && resp.ContentLength > rt.cfg.MaxObjectSize.Bytes()) {
		err := e.streamBypass(w, r, resp, meta)
		return index.Object{}, index.Blob{}, resp.Upstream, false, true, err
	}
	release, ok := e.reserve(resp.ContentLength)
	if !ok {
		e.logger.Warn("storage_reservation_refused", "repo", rt.cfg.Name, "key", key, "bytes", resp.ContentLength)
		err := e.streamBypass(w, r, resp, meta)
		return index.Object{}, index.Blob{}, resp.Upstream, false, true, err
	}
	defer release()

	algorithms := make([]string, 0, len(expectations)+len(companions))
	for _, expectation := range expectations {
		algorithms = append(algorithms, expectation.Algo)
	}
	for _, companion := range companions {
		algorithms = append(algorithms, companion.Algo)
	}
	hasher := (*integrity.Hasher)(nil)
	if len(companions) > 0 {
		var err error
		hasher, err = integrity.NewHasher(algorithms...)
		if err != nil {
			return index.Object{}, index.Blob{}, "", false, false, err
		}
	}

	compress := e.cfg.Cache.CompressMetadata && isText(contentType, plan.UpstreamPath) &&
		plan.Hooks.Validate == nil && plan.Hooks.ChecksumFor == nil
	opts := store.WriterOptions{
		Compress:     compress,
		ExtraDigests: algorithms,
		Expectations: expectations,
		MaxBytes:     rt.cfg.MaxObjectSize.Bytes(),
	}
	if resp.ContentLength >= 0 {
		expected := resp.ContentLength
		opts.ExpectedSize = &expected
	}
	writer, err := e.store.NewWriter(opts)
	if err != nil {
		return index.Object{}, index.Blob{}, "", false, false, localError(err)
	}
	abort := func() { _ = writer.Abort() }

	wrote := false
	teeWriter := (*clientTee)(nil)
	if tee {
		e.setMissHeaders(w, rt, plan, key, resp, meta)
		teeWriter = &clientTee{w: w, maxBackground: e.cfg.Limits.BackgroundCompleteMax.Bytes(), contentLength: resp.ContentLength, method: r.Method}
		wrote = true
	}

	// While a client is being served, a failure to store must not cut its
	// download short; the body is relayed in full and the failure reported
	// afterwards.
	var storeSink io.Writer = localWriter{writer}
	var deferred *deferredWriter
	if teeWriter != nil {
		deferred = &deferredWriter{w: storeSink}
		storeSink = deferred
	}
	var sinkWriters []io.Writer
	sinkWriters = append(sinkWriters, storeSink)
	if teeWriter != nil {
		sinkWriters = append(sinkWriters, teeWriter)
	}
	if hasher != nil {
		sinkWriters = append(sinkWriters, hasher)
	}
	copyBuf := make([]byte, 32*1024)
	if _, err := io.CopyBuffer(io.MultiWriter(sinkWriters...), resp.Body, copyBuf); err != nil {
		abort()
		if errors.Is(err, upstream.ErrStop) {
			return index.Object{}, index.Blob{}, resp.Upstream, false, wrote, err
		}
		err = fmt.Errorf("stream immutable body: %w", err)
		if wrote {
			err = fmt.Errorf("%w: %w", errTruncated, err)
		}
		return index.Object{}, index.Blob{}, resp.Upstream, false, wrote, err
	}
	if deferred != nil && deferred.err != nil {
		abort()
		return index.Object{}, index.Blob{}, resp.Upstream, false, wrote, deferred.err
	}
	if writer.RawSize() == 0 {
		abort()
		return index.Object{}, index.Blob{}, resp.Upstream, false, wrote, errEmptyBody
	}
	file := writer.TempFile()
	if file == nil {
		abort()
		return index.Object{}, index.Blob{}, "", false, wrote, fmt.Errorf("writer has no temp file")
	}
	size := writer.RawSize()
	if err := file.Sync(); err != nil {
		abort()
		return index.Object{}, index.Blob{}, "", false, wrote, fmt.Errorf("sync temp file: %w", err)
	}
	if plan.Hooks.Validate != nil {
		if err := plan.Hooks.Validate(ctx, file, size); err != nil {
			abort()
			return index.Object{}, index.Blob{}, "", false, wrote, fmt.Errorf("validate artifact: %w", err)
		}
	}

	verified := verifiedKnown
	verifyDigest := ""
	if plan.Hooks.Verify != nil {
		digest, err := plan.Hooks.Verify(ctx, file, size, e.verificationFetch(rt))
		switch {
		case err == nil:
			verified = true
			verifyDigest = digest
		case errors.Is(err, kinds.ErrVerificationUnavailable):
			e.logger.Debug("verification_deferred", "repo", rt.cfg.Name, "key", key, "error", err)
		default:
			abort()
			e.verifyFailure(rt.cfg.Name)
			return index.Object{}, index.Blob{}, "", false, wrote, fmt.Errorf("verify artifact: %w", err)
		}
	}

	var companionData []byte
	var companionDigest integrity.Digest
	var companionSuffix string
	companionPresent := false
	for _, companion := range companions {
		digest, data, present, err := e.fetchCompanion(ctx, resp.Target, plan.UpstreamPath, companion)
		if err != nil {
			e.logger.Debug("companion_unavailable", "repo", rt.cfg.Name, "key", key, "suffix", companion.Suffix, "error", err)
			continue
		}
		if !present {
			continue
		}
		if err := integrity.Check([]integrity.Digest{digest}, hasher.Sum()); err != nil {
			abort()
			e.verifyFailure(rt.cfg.Name)
			return index.Object{}, index.Blob{}, "", false, wrote, fmt.Errorf("companion verification: %w", err)
		}
		companionData, companionDigest, companionSuffix = data, digest, companion.Suffix
		companionPresent = true
		verified = true
		break
	}
	if rt.cfg.RequireVerified && !verified {
		abort()
		e.verifyFailure(rt.cfg.Name)
		return index.Object{}, index.Blob{}, "", false, wrote, fmt.Errorf("object is not verified")
	}

	var checksumData []byte
	if plan.Hooks.ChecksumFor != nil {
		checksumData, err = readTempFile(file, 64<<10)
		if err != nil {
			abort()
			return index.Object{}, index.Blob{}, "", false, wrote, fmt.Errorf("read checksum file: %w", err)
		}
	}

	var blob store.Blob
	var indexBlob index.Blob
	var object index.Object
	now := e.clock.Now()
	err = e.store.Publish(func() error {
		var err error
		blob, _, err = writer.Commit()
		if err != nil {
			if errors.Is(err, store.ErrDigestMismatch) {
				e.verifyFailure(rt.cfg.Name)
			}
			return err
		}
		indexBlob = index.Blob{
			SHA256:     blob.SHA256,
			RawSize:    blob.RawSize,
			StoredSize: blob.StoredSize,
			StoredEnc:  string(blob.StoredEnc),
			CreatedAt:  now,
		}
		object, err = e.index.PublishObject(ctx, indexBlob, index.PutObjectParams{
			Repo:         rt.cfg.Name,
			Key:          key,
			Class:        "immutable",
			SHA256:       blob.SHA256,
			ContentType:  contentType,
			ETag:         resp.Header.Get("ETag"),
			LastModified: resp.Header.Get("Last-Modified"),
			Upstream:     resp.Upstream,
			FetchedAt:    now,
			ValidatedAt:  now,
			Verified:     verified,
		})
		return err
	})
	if err != nil {
		return index.Object{}, index.Blob{}, "", false, wrote, localError(err)
	}
	if verifyDigest != "" {
		if err := e.index.UpsertExpectation(ctx, index.Expectation{
			Repo: rt.cfg.Name, Key: key, Algo: "h1", Digest: verifyDigest,
			Source: "sumdb", CreatedAt: now,
		}); err != nil {
			e.logger.Warn("expectation_store_failed", "repo", rt.cfg.Name, "key", key, "error", err)
		}
	}
	if companionPresent {
		if err := e.storeCompanion(ctx, rt, plan.UpstreamPath, companionSuffix, companionData, resp.Upstream, now); err != nil {
			e.logger.Warn("companion_store_failed", "repo", rt.cfg.Name, "key", key, "error", err)
		}
		if err := e.index.UpsertExpectation(ctx, index.Expectation{
			Repo: rt.cfg.Name, Key: key, Algo: companionDigest.Algo, Digest: companionDigest.Hex,
			Source: companionPath(plan.UpstreamPath, companionSuffix), CreatedAt: now,
		}); err != nil {
			e.logger.Warn("expectation_store_failed", "repo", rt.cfg.Name, "key", key, "error", err)
		}
	}
	if len(expectations) > 0 {
		known := make([]index.Expectation, 0, len(expectations))
		for _, expectation := range expectations {
			known = append(known, index.Expectation{
				Repo: rt.cfg.Name, Key: key, Algo: expectation.Algo, Digest: expectation.Digest,
				Source: plan.UpstreamPath, CreatedAt: now,
			})
		}
		if err := e.index.UpsertExpectations(ctx, known); err != nil {
			e.logger.Warn("expectation_store_failed", "repo", rt.cfg.Name, "key", key, "error", err)
		}
	}
	if plan.Hooks.ChecksumFor != nil && len(checksumData) > 0 {
		e.checkChecksumConflict(ctx, rt, plan, key, checksumData)
	}
	if err := e.ingest(ctx, rt, plan, object, indexBlob); err != nil {
		e.logger.Warn("ingest_failed", "repo", rt.cfg.Name, "key", key, "error", err)
	}
	e.pruneHistory(ctx, rt, key)
	return object, indexBlob, resp.Upstream, verified, wrote, nil
}

func readTempFile(file *os.File, limit int64) ([]byte, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(file, limit))
}

func (e *Engine) storeCompanion(ctx context.Context, rt *repoRuntime, artifactPath, suffix string, data []byte, upstreamName string, now time.Time) error {
	key := companionPath(artifactPath, suffix)
	writer, err := e.store.NewWriter(store.WriterOptions{MaxBytes: int64(len(data)) + 1})
	if err != nil {
		return err
	}
	if _, err := writer.Write(data); err != nil {
		_ = writer.Abort()
		return err
	}
	return e.store.Publish(func() error {
		blob, _, err := writer.Commit()
		if err != nil {
			return err
		}
		_, err = e.index.PublishObject(ctx, index.Blob{
			SHA256: blob.SHA256, RawSize: blob.RawSize, StoredSize: blob.StoredSize,
			StoredEnc: string(blob.StoredEnc), CreatedAt: now,
		}, index.PutObjectParams{
			Repo: rt.cfg.Name, Key: key, Class: "immutable", SHA256: blob.SHA256,
			ContentType: "text/plain; charset=utf-8", Upstream: upstreamName,
			FetchedAt: now, ValidatedAt: now, Verified: true,
		})
		return err
	})
}

func (e *Engine) checkChecksumConflict(ctx context.Context, rt *repoRuntime, plan kinds.Plan, key string, data []byte) {
	artifactKey, algo, ok := plan.Hooks.ChecksumFor(key)
	if !ok {
		return
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return
	}
	declared, err := integrity.ParseHex(algo, fields[0])
	if err != nil {
		return
	}
	expected := ""
	expectation, err := e.index.GetExpectation(ctx, rt.cfg.Name, artifactKey, algo)
	if err == nil {
		expected = expectation.Digest
	} else if object, objectErr := e.index.CurrentObject(ctx, rt.cfg.Name, artifactKey); objectErr == nil {
		expected, err = e.digestStored(ctx, object.SHA256, algo)
		if err != nil {
			e.logger.Debug("checksum_compare_failed", "repo", rt.cfg.Name, "key", key, "error", err)
			return
		}
	}
	if expected == "" || strings.EqualFold(expected, declared.Hex) {
		return
	}
	e.logger.Error("digest_conflict",
		"repo", rt.cfg.Name,
		"key", artifactKey,
		"algo", algo,
		"stored", expected,
		"declared", declared.Hex,
	)
	e.verifyFailure(rt.cfg.Name)
}

func (e *Engine) verificationFetch(rt *repoRuntime) kinds.FetchFunc {
	return func(ctx context.Context, path string) ([]byte, error) {
		plan, err := rt.kind.Classify(kinds.Request{Path: path})
		if err != nil {
			return nil, err
		}
		targets, err := e.targets(rt, plan, nil)
		if err != nil {
			return nil, err
		}
		var body []byte
		_, err = e.fetcher.Fetch(ctx, http.MethodGet, targets, func(resp *upstream.Response) error {
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("unexpected lookup status %d", resp.StatusCode)
			}
			data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
			if err != nil {
				return err
			}
			body = data
			return nil
		})
		if err != nil {
			return nil, err
		}
		return body, nil
	}
}

func (e *Engine) fetchMutable(ctx context.Context, rt *repoRuntime, plan kinds.Plan, key string, w http.ResponseWriter, r *http.Request, meta *responseMeta) flightResult {
	return e.fetchMutableAttempt(ctx, rt, plan, key, w, r, meta, true)
}

func (e *Engine) fetchMutableAttempt(ctx context.Context, rt *repoRuntime, plan kinds.Plan, key string, w http.ResponseWriter, r *http.Request, meta *responseMeta, retry bool) flightResult {
	targets, err := e.targets(rt, plan, nil)
	if err != nil {
		return flightResult{err: err, status: http.StatusBadGateway}
	}
	expectations, verifiedKnown, err := e.expectations(e.ctx, rt.cfg.Name, key, plan.Hooks.Expect)
	if err != nil {
		return flightResult{err: err, status: http.StatusInternalServerError}
	}
	var object index.Object
	var used string
	var verified bool
	written := false
	_, err = e.fetcher.Fetch(e.ctx, http.MethodGet, targets, func(resp *upstream.Response) error {
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("unexpected status %d", resp.StatusCode)
		}
		release, reserved := e.reserve(resp.ContentLength)
		defer release()
		if !reserved || (resp.ContentLength >= 0 && resp.ContentLength > rt.cfg.MaxObjectSize.Bytes()) {
			used = resp.Upstream
			written = true
			if err := e.streamBypass(w, r, resp, meta); err != nil {
				return fmt.Errorf("%w: %w", upstream.ErrStop, err)
			}
			return nil
		}
		stored, _, upstreamName, ok, _, err := e.storeMutableResponse(e.ctx, rt, plan, key, resp, expectations, verifiedKnown)
		if err != nil {
			return err
		}
		object, used, verified = stored, upstreamName, ok
		return nil
	})
	if err != nil {
		if errors.Is(err, upstream.ErrStop) {
			return flightResult{err: err, status: failureStatus(err), cache: "MISS", written: written, upstream: used}
		}
		if errors.Is(err, upstream.ErrNotFound) {
			var nf *upstream.NotFoundError
			body := []byte(nil)
			status := http.StatusNotFound
			if errors.As(err, &nf) {
				body = nf.Body
				if nf.Status != 0 {
					status = nf.Status
				}
			}
			e.negative.put(rt.cfg.Name+"\x00"+key, rt.cfg.NegativeTTL.Duration(), status, body)
			return flightResult{err: err, status: status, cache: "MISS", body: body}
		}
		if errors.Is(err, store.ErrDigestMismatch) {
			if retry && plan.Hooks.RefreshOnMismatch != nil {
				e.refreshForMismatch(ctx, rt, plan, key)
				return e.fetchMutableAttempt(ctx, rt, plan, key, w, r, meta, false)
			}
			if current, currentErr := e.index.CurrentObject(ctx, rt.cfg.Name, key); currentErr == nil {
				return flightResult{cache: "STALE", object: current, upstream: current.Upstream, stale: true}
			}
		}
		return flightResult{err: err, status: failureStatus(err), cache: "MISS"}
	}
	return flightResult{cache: "MISS", upstream: used, object: object, verified: verified, written: written}
}

func (e *Engine) storeMutableResponse(ctx context.Context, rt *repoRuntime, plan kinds.Plan, key string, resp *upstream.Response, expectations []store.Expectation, verifiedKnown bool) (index.Object, index.Blob, string, bool, bool, error) {
	contentType := resp.Header.Get("Content-Type")
	if plan.Binary && isHTML(contentType) {
		return index.Object{}, index.Blob{}, "", false, false, fmt.Errorf("rejected text/html binary body")
	}
	if e.bypassActive(rt.cfg.Name) {
		return index.Object{}, index.Blob{}, resp.Upstream, false, true, fmt.Errorf("bypass")
	}
	compress := e.cfg.Cache.CompressMetadata && isText(contentType, plan.UpstreamPath) &&
		plan.Hooks.Validate == nil && plan.Hooks.ChecksumFor == nil
	algorithms := make([]string, 0, len(expectations))
	for _, expectation := range expectations {
		algorithms = append(algorithms, expectation.Algo)
	}
	opts := store.WriterOptions{
		Compress:     compress,
		ExtraDigests: algorithms,
		Expectations: expectations,
		MaxBytes:     rt.cfg.MaxObjectSize.Bytes(),
	}
	if resp.ContentLength >= 0 {
		expected := resp.ContentLength
		opts.ExpectedSize = &expected
	}
	writer, err := e.store.NewWriter(opts)
	if err != nil {
		return index.Object{}, index.Blob{}, "", false, false, localError(err)
	}
	if _, err := io.CopyBuffer(localWriter{writer}, resp.Body, make([]byte, 32*1024)); err != nil {
		_ = writer.Abort()
		return index.Object{}, index.Blob{}, "", false, false, fmt.Errorf("stream mutable body: %w", err)
	}
	if writer.RawSize() == 0 {
		_ = writer.Abort()
		return index.Object{}, index.Blob{}, "", false, false, errEmptyBody
	}
	if plan.Hooks.Validate != nil {
		file := writer.TempFile()
		if file == nil {
			_ = writer.Abort()
			return index.Object{}, index.Blob{}, "", false, false, fmt.Errorf("validator has no temp file")
		}
		if err := file.Sync(); err != nil {
			_ = writer.Abort()
			return index.Object{}, index.Blob{}, "", false, false, err
		}
		if err := plan.Hooks.Validate(ctx, file, writer.RawSize()); err != nil {
			_ = writer.Abort()
			return index.Object{}, index.Blob{}, "", false, false, err
		}
	}
	var blob store.Blob
	var indexBlob index.Blob
	var object index.Object
	now := e.clock.Now()
	err = e.store.Publish(func() error {
		var err error
		blob, _, err = writer.Commit()
		if err != nil {
			if errors.Is(err, store.ErrDigestMismatch) {
				e.verifyFailure(rt.cfg.Name)
			}
			return err
		}
		indexBlob = index.Blob{
			SHA256: blob.SHA256, RawSize: blob.RawSize, StoredSize: blob.StoredSize,
			StoredEnc: string(blob.StoredEnc), CreatedAt: now,
		}
		object, err = e.index.PublishObject(ctx, indexBlob, index.PutObjectParams{
			Repo: rt.cfg.Name, Key: key, Class: "mutable", SHA256: blob.SHA256,
			ContentType: contentType, ETag: resp.Header.Get("ETag"),
			LastModified: resp.Header.Get("Last-Modified"), Upstream: resp.Upstream,
			FetchedAt: now, ValidatedAt: now, Verified: verifiedKnown,
		})
		return err
	})
	if err != nil {
		return index.Object{}, index.Blob{}, "", false, false, localError(err)
	}
	if err := e.ingest(ctx, rt, plan, object, indexBlob); err != nil {
		e.logger.Warn("ingest_failed", "repo", rt.cfg.Name, "key", key, "error", err)
	}
	e.pruneHistory(ctx, rt, key)
	return object, indexBlob, resp.Upstream, verifiedKnown, false, nil
}

func (e *Engine) revalidate(ctx context.Context, rt *repoRuntime, plan kinds.Plan, key string, object index.Object, w http.ResponseWriter, r *http.Request, meta *responseMeta, allowStale bool) {
	flight, leader := e.flights.begin(rt.cfg.Name, key)
	if !leader {
		e.waitAndServe(ctx, rt, plan, key, flight, w, r, meta)
		return
	}
	defer e.flights.finish(rt.cfg.Name, key, flight, flightResult{err: errLeaderFailed, status: http.StatusInternalServerError})
	result := e.revalidateLeader(ctx, rt, plan, key, object, allowStale)
	e.flights.finish(rt.cfg.Name, key, flight, result)
	e.applyFlight(rt, plan, key, result, w, r, meta, true)
}

func (e *Engine) revalidateLeader(ctx context.Context, rt *repoRuntime, plan kinds.Plan, key string, object index.Object, allowStale bool) flightResult {
	result, mismatch := e.revalidateOnce(ctx, rt, plan, key, object, allowStale)
	if mismatch && plan.Hooks.RefreshOnMismatch != nil {
		e.refreshForMismatch(ctx, rt, plan, key)
		retried, retryMismatch := e.revalidateOnce(ctx, rt, plan, key, object, allowStale)
		if !retryMismatch {
			return retried
		}
		result = retried
	}
	if mismatch && object.ID != 0 {
		return flightResult{cache: "STALE", object: object, upstream: object.Upstream, stale: true}
	}
	return result
}

func (e *Engine) revalidateOnce(ctx context.Context, rt *repoRuntime, plan kinds.Plan, key string, object index.Object, allowStale bool) (flightResult, bool) {
	headers := make(http.Header)
	if object.ETag != "" {
		headers.Set("If-None-Match", object.ETag)
	}
	if object.LastModified != "" {
		headers.Set("If-Modified-Since", object.LastModified)
	}
	targets, err := e.targets(rt, plan, headers)
	if err != nil {
		return flightResult{err: err, status: http.StatusBadGateway}, false
	}
	expectations, verifiedKnown, err := e.expectations(e.ctx, rt.cfg.Name, key, plan.Hooks.Expect)
	if err != nil {
		return flightResult{err: err, status: http.StatusInternalServerError}, false
	}
	revalidateCtx := e.ctx
	cancel := func() {}
	if rt.cfg.RevalidateTimeout > 0 {
		revalidateCtx, cancel = context.WithTimeout(e.ctx, rt.cfg.RevalidateTimeout.Duration())
	}
	defer cancel()

	var current index.Object
	notModified := false
	var used string
	_, err = e.fetcher.Fetch(revalidateCtx, http.MethodGet, targets, func(resp *upstream.Response) error {
		used = resp.Upstream
		if resp.NotModified || resp.StatusCode == http.StatusNotModified {
			now := e.clock.Now()
			if err := e.index.TouchValidated(e.ctx, rt.cfg.Name, key, now); err != nil {
				return err
			}
			current = object
			current.ValidatedAt = now
			notModified = true
			return nil
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("unexpected status %d", resp.StatusCode)
		}
		stored, _, _, verified, _, err := e.storeMutableResponse(e.ctx, rt, plan, key, resp, expectations, verifiedKnown)
		if err != nil {
			return err
		}
		current = stored
		current.Verified = verified
		return nil
	})
	if err != nil {
		if errors.Is(err, upstream.ErrNotFound) {
			var nf *upstream.NotFoundError
			status := http.StatusNotFound
			if errors.As(err, &nf) && nf.Status != 0 {
				status = nf.Status
			}
			if rt.cfg.ServeGone {
				return flightResult{cache: "GONE", object: object, upstream: object.Upstream, goneStatus: status}, false
			}
			return flightResult{err: err, status: http.StatusNotFound, cache: "MISS"}, false
		}
		if errors.Is(err, store.ErrDigestMismatch) {
			return flightResult{err: err, status: http.StatusBadGateway}, true
		}
		if allowStale {
			return flightResult{cache: "STALE", object: object, upstream: object.Upstream, stale: true}, false
		}
		return flightResult{err: err, status: failureStatus(err)}, false
	}
	if notModified {
		return flightResult{cache: "REVALIDATED", object: current, upstream: used, verified: current.Verified}, false
	}
	if current.ID == 0 {
		current = object
	}
	return flightResult{cache: "REVALIDATED", object: current, upstream: used, verified: current.Verified}, false
}

func (e *Engine) refreshForMismatch(ctx context.Context, rt *repoRuntime, plan kinds.Plan, key string) {
	if plan.Hooks.RefreshOnMismatch == nil {
		return
	}
	for _, refreshKey := range plan.Hooks.RefreshOnMismatch(key) {
		object, err := e.index.CurrentObject(ctx, rt.cfg.Name, refreshKey)
		if err != nil {
			continue
		}
		refreshPlan, err := rt.kind.Classify(kinds.Request{Path: refreshKey})
		if err != nil || refreshPlan.Class != kinds.Mutable {
			continue
		}
		if _, mismatch := e.revalidateOnce(ctx, rt, refreshPlan, refreshKey, object, false); mismatch {
			e.logger.Warn("refresh_mismatch", "repo", rt.cfg.Name, "key", refreshKey)
		}
	}
}

func (e *Engine) startRefresh(rt *repoRuntime, plan kinds.Plan, key string) {
	if e.offline.Load() {
		return
	}
	flight, leader := e.flights.begin(rt.cfg.Name, key)
	if !leader {
		return
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer e.flights.finish(rt.cfg.Name, key, flight, flightResult{err: errLeaderFailed, status: http.StatusInternalServerError})
		var result flightResult
		object, err := e.index.CurrentObject(e.ctx, rt.cfg.Name, key)
		if err == nil {
			result = e.revalidateLeader(e.ctx, rt, plan, key, object, true)
		} else {
			result = flightResult{err: err}
		}
		e.flights.finish(rt.cfg.Name, key, flight, result)
	}()
}

func (e *Engine) applyFlight(rt *repoRuntime, plan kinds.Plan, key string, result flightResult, w http.ResponseWriter, r *http.Request, meta *responseMeta, leader bool) {
	if result.written && leader {
		if errors.Is(result.err, errTruncated) {
			abortResponse()
		}
		return
	}
	if result.object.ID != 0 {
		cache := result.cache
		if cache == "" {
			cache = "MISS"
		}
		meta.goneStatus = result.goneStatus
		e.serveObject(context.Background(), rt, plan, key, result.object, cache, result.stale, w, r, meta)
		return
	}
	if result.err != nil {
		status := result.status
		if status == 0 {
			status = http.StatusBadGateway
		}
		if len(result.body) > 0 {
			writeErrorBody(w, status, "MISS", result.body)
		} else {
			writeError(w, status, "MISS", result.err.Error())
		}
		return
	}
	writeError(w, http.StatusBadGateway, "MISS", "upstream request failed")
}

func (e *Engine) handleBypass(ctx context.Context, rt *repoRuntime, plan kinds.Plan, w http.ResponseWriter, r *http.Request, meta *responseMeta) {
	targets, err := e.targets(rt, plan, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "BYPASS", err.Error())
		return
	}
	started := false
	_, err = e.fetcher.Fetch(e.ctx, r.Method, targets, func(resp *upstream.Response) error {
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("unexpected status %d", resp.StatusCode)
		}
		started = true
		if err := e.streamBypass(w, r, resp, meta); err != nil {
			return fmt.Errorf("%w: %w", upstream.ErrStop, err)
		}
		return nil
	})
	if err != nil && started {
		abortResponse()
	}
	if err != nil && !started {
		status := http.StatusBadGateway
		if upstream.IsNotFound(err) {
			status = http.StatusNotFound
		}
		writeError(w, status, "BYPASS", "upstream request failed")
	}
}

func (e *Engine) handleHeadMiss(ctx context.Context, rt *repoRuntime, plan kinds.Plan, w http.ResponseWriter, r *http.Request, meta *responseMeta) {
	targets, err := e.targets(rt, plan, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "MISS", err.Error())
		return
	}
	started := false
	_, err = e.fetcher.Fetch(e.ctx, http.MethodHead, targets, func(resp *upstream.Response) error {
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("unexpected status %d", resp.StatusCode)
		}
		started = true
		meta.cache = "MISS"
		meta.upstream = resp.Upstream
		w.Header().Set("X-Larder-Cache", "MISS")
		w.Header().Set("X-Larder-Upstream", resp.Upstream)
		if resp.ContentLength >= 0 {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", resp.ContentLength))
		}
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		if plan.Class == kinds.Immutable {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
		}
		w.WriteHeader(http.StatusOK)
		return nil
	})
	if err != nil && !started {
		status := http.StatusBadGateway
		if upstream.IsNotFound(err) {
			status = http.StatusNotFound
		}
		writeError(w, status, "MISS", "upstream request failed")
	}
}

// streamBypass relays an upstream body without storing it. An error means
// the response is already partly written and cannot be completed.
func (e *Engine) streamBypass(w http.ResponseWriter, r *http.Request, resp *upstream.Response, meta *responseMeta) error {
	meta.cache = "BYPASS"
	meta.upstream = resp.Upstream
	w.Header().Set("X-Larder-Cache", "BYPASS")
	w.Header().Set("X-Larder-Upstream", resp.Upstream)
	w.Header().Set("Cache-Control", "no-store")
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	if resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", resp.ContentLength))
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		_, err := io.Copy(io.Discard, resp.Body)
		return err
	}
	_, err := io.Copy(w, resp.Body)
	if err != nil {
		return fmt.Errorf("%w: %w", errTruncated, err)
	}
	return nil
}

func (e *Engine) setMissHeaders(w http.ResponseWriter, rt *repoRuntime, plan kinds.Plan, key string, resp *upstream.Response, meta *responseMeta) {
	meta.cache = "MISS"
	meta.upstream = resp.Upstream
	w.Header().Set("X-Larder-Cache", "MISS")
	w.Header().Set("X-Larder-Key", rt.cfg.Name+":"+key)
	w.Header().Set("X-Larder-Upstream", resp.Upstream)
	w.Header().Set("X-Larder-Verified", "0")
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", contentTypeFor(plan, ""))
	}
	if etag := resp.Header.Get("ETag"); etag != "" {
		w.Header().Set("ETag", etag)
	}
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		w.Header().Set("Last-Modified", lm)
	}
	if resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", resp.ContentLength))
	}
	if plan.Class == kinds.Immutable {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
	}
	w.WriteHeader(http.StatusOK)
}

func (e *Engine) expectations(ctx context.Context, repo, key string, hook func(string) []integrity.Digest) ([]store.Expectation, bool, error) {
	rows, err := e.index.LookupExpectations(ctx, repo, key)
	if err != nil {
		return nil, false, err
	}
	out := make([]store.Expectation, 0, len(rows))
	for _, row := range rows {
		if row.Algo == "h1" {
			continue
		}
		out = append(out, store.Expectation{Algo: row.Algo, Digest: row.Digest})
	}
	if hook != nil {
		for _, digest := range hook(key) {
			out = append(out, store.Expectation{Algo: digest.Algo, Digest: digest.Hex})
		}
	}
	return out, len(out) > 0, nil
}

func (e *Engine) fetchCompanion(ctx context.Context, target upstream.Target, pathValue string, companion kinds.Companion) (integrity.Digest, []byte, bool, error) {
	companionPath := companionPath(pathValue, companion.Suffix)
	derived, err := target.WithPath(companionPath, nil)
	if err != nil {
		return integrity.Digest{}, nil, false, err
	}
	var digest integrity.Digest
	var body []byte
	_, err = e.fetcher.Fetch(ctx, http.MethodGet, []upstream.Target{derived}, func(resp *upstream.Response) error {
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("unexpected companion status %d", resp.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return err
		}
		fields := strings.Fields(string(data))
		if len(fields) == 0 {
			return fmt.Errorf("empty companion")
		}
		parsed, err := integrity.ParseHex(companion.Algo, fields[0])
		if err != nil {
			return err
		}
		digest = parsed
		body = data
		return nil
	})
	if err != nil {
		if errors.Is(err, upstream.ErrNotFound) {
			return integrity.Digest{}, nil, false, nil
		}
		return integrity.Digest{}, nil, false, err
	}
	return digest, body, true, nil
}

func companionPath(pathValue, suffix string) string {
	if i := strings.IndexByte(pathValue, '?'); i >= 0 {
		return pathValue[:i] + suffix + pathValue[i:]
	}
	return pathValue + suffix
}

func (e *Engine) verifyFailure(repo string) {
	if e.metrics != nil {
		e.metrics.VerifyFailures.With(repo).Inc()
	}
}

func (e *Engine) pruneHistory(ctx context.Context, rt *repoRuntime, key string) {
	generations, err := e.index.Generations(ctx, rt.cfg.Name, key)
	if err != nil {
		e.logger.Warn("history_prune_failed", "repo", rt.cfg.Name, "key", key, "error", err)
		return
	}
	keep := e.cfg.Cache.MetadataHistory + 1
	if len(generations) <= keep {
		return
	}
	for _, object := range generations[keep:] {
		if err := e.index.DeleteObject(ctx, object.ID); err != nil {
			e.logger.Warn("history_prune_failed", "repo", rt.cfg.Name, "key", key, "error", err)
		}
	}
}

func isHTML(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "text/html")
	}
	return strings.EqualFold(mediaType, "text/html")
}

func isText(contentType, requestPath string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err == nil {
		mediaType = strings.ToLower(mediaType)
		if strings.HasPrefix(mediaType, "text/") ||
			mediaType == "application/json" || mediaType == "application/xml" ||
			mediaType == "application/xhtml+xml" || mediaType == "application/toml" ||
			mediaType == "application/yaml" || mediaType == "application/x-yaml" ||
			strings.HasSuffix(mediaType, "+json") || strings.HasSuffix(mediaType, "+xml") {
			return true
		}
	}
	if i := strings.IndexByte(requestPath, '?'); i >= 0 {
		requestPath = requestPath[:i]
	}
	clean := path.Base(requestPath)
	switch strings.ToLower(path.Ext(clean)) {
	case ".json", ".xml", ".html", ".htm", ".txt", ".toml", ".yaml", ".yml", ".lock":
		return true
	}
	switch clean {
	case "Packages", "Release", "InRelease":
		return true
	}
	return false
}

type clientTee struct {
	w             io.Writer
	maxBackground int64
	contentLength int64
	method        string
	total         int64
	clientFailed  bool
}

func (t *clientTee) Write(p []byte) (int, error) {
	t.total += int64(len(p))
	if t.method == http.MethodHead {
		return len(p), nil
	}
	if !t.clientFailed {
		if _, err := t.w.Write(p); err != nil {
			t.clientFailed = true
		}
	}
	if t.clientFailed && (t.maxBackground >= 0 && (t.total > t.maxBackground || t.contentLength > t.maxBackground)) {
		if t.contentLength > t.maxBackground {
			return len(p), upstream.ErrStop
		}
		if t.total > t.maxBackground {
			return len(p), upstream.ErrStop
		}
	}
	return len(p), nil
}

// errTruncated marks a failure after response bytes were sent, when the client
// holds an incomplete body that must not look like a finished one.
var errTruncated = errors.New("response body truncated")

// errEmptyBody reports a 200 response without content, which is never a
// usable cacheable artifact.
var errEmptyBody = errors.New("empty response body")

// abortResponse terminates the client connection without a clean ending. A
// chunked response that stopped early would otherwise be indistinguishable
// from a complete one.
func abortResponse() { panic(http.ErrAbortHandler) }

// localWriter tags write failures as local so the upstream is not blamed.
type localWriter struct{ w io.Writer }

func (l localWriter) Write(p []byte) (int, error) {
	n, err := l.w.Write(p)
	if err != nil && !errors.Is(err, store.ErrTooLarge) {
		err = &upstream.LocalError{Err: err}
	}
	return n, err
}

// localError tags an error from storing a response as local unless the
// response itself was at fault.
func localError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrDigestMismatch), errors.Is(err, store.ErrLengthMismatch),
		errors.Is(err, store.ErrTooLarge), errors.Is(err, store.ErrEmpty), errors.Is(err, errEmptyBody):
		return err
	}
	return &upstream.LocalError{Err: err}
}

// failureStatus maps a failed fetch to the status the client should see.
func failureStatus(err error) int {
	var local *upstream.LocalError
	switch {
	case errors.As(err, &local):
		if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
			return http.StatusInsufficientStorage
		}
		return http.StatusInternalServerError
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

// deferredWriter remembers the first write failure and discards later writes,
// so a failing destination does not interrupt the writers beside it.
type deferredWriter struct {
	w   io.Writer
	err error
}

func (d *deferredWriter) Write(p []byte) (int, error) {
	if d.err == nil {
		if _, err := d.w.Write(p); err != nil {
			d.err = err
		}
	}
	return len(p), nil
}

// reserve claims disk space for a body of the given length, when known.
func (e *Engine) reserve(size int64) (func(), bool) {
	if reserver, ok := e.bypass.(Reserver); ok && size > 0 {
		if release, granted := reserver.Reserve(size); granted && release != nil {
			return release, true
		}
		return func() {}, false
	}
	return func() {}, true
}
