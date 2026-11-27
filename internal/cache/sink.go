package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/integrity"
	"github.com/mmrzaf/larder/internal/kinds"
	"github.com/mmrzaf/larder/internal/store"
)

func (e *Engine) ingest(ctx context.Context, rt *repoRuntime, plan kinds.Plan, object index.Object, blob index.Blob) error {
	if plan.Hooks.Ingest == nil {
		return nil
	}
	reader, err := e.store.Open(object.SHA256, store.Encoding(blob.StoredEnc))
	if err != nil {
		return fmt.Errorf("open stored document for ingest: %w", err)
	}
	sink := &engineSink{engine: e, repo: rt.cfg.Name, source: object.Key, logger: e.logger}
	hookErr := plan.Hooks.Ingest(ctx, kinds.ObjectRef{
		Repo: rt.cfg.Name, Key: object.Key, Class: plan.Class, SHA256: object.SHA256,
		ContentType: object.ContentType, Size: blob.RawSize, Verified: object.Verified,
	}, reader, sink)
	closeErr := reader.Close()
	flushErr := sink.flush(ctx)
	if hookErr != nil {
		return fmt.Errorf("ingest document: %w", hookErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close ingested document: %w", closeErr)
	}
	return flushErr
}

type engineSink struct {
	engine       *Engine
	repo         string
	source       string
	logger       *slog.Logger
	expectations []index.Expectation
	replacements []index.Expectation
	stale        []string
}

func (s *engineSink) Expect(key, algo, hexDigest string) {
	digest, err := integrity.ParseHex(algo, hexDigest)
	if err != nil {
		s.logger.Warn("ingest_expectation_invalid", "repo", s.repo, "key", key, "error", err)
		return
	}
	s.expectations = append(s.expectations, index.Expectation{
		Repo: s.repo, Key: key, Algo: digest.Algo, Digest: digest.Hex,
		Source: s.source, CreatedAt: s.engine.clock.Now(),
	})
}

// ReplaceExpectation records an authoritative digest that overrides an older row.
func (s *engineSink) ReplaceExpectation(key, algo, hexDigest string) {
	digest, err := integrity.ParseHex(algo, hexDigest)
	if err != nil {
		s.logger.Warn("ingest_expectation_invalid", "repo", s.repo, "key", key, "error", err)
		return
	}
	s.replacements = append(s.replacements, index.Expectation{
		Repo: s.repo, Key: key, Algo: digest.Algo, Digest: digest.Hex,
		Source: s.source, CreatedAt: s.engine.clock.Now(),
	})
}

func (s *engineSink) MarkStale(prefixGlob string) {
	if prefixGlob != "" {
		s.stale = append(s.stale, prefixGlob)
	}
}

func (s *engineSink) Logger() *slog.Logger { return s.logger }

func (s *engineSink) flush(ctx context.Context) error {
	if err := s.engine.index.UpsertExpectations(ctx, s.replacements); err != nil {
		return fmt.Errorf("flush authoritative expectations: %w", err)
	}
	var pending []index.Expectation
	for _, expectation := range s.expectations {
		keep, err := s.engine.applyLateExpectation(ctx, expectation)
		if err != nil {
			s.logger.Warn("late_expectation_failed", "repo", expectation.Repo, "key", expectation.Key, "error", err)
			continue
		}
		if keep {
			pending = append(pending, expectation)
		}
	}
	if err := s.engine.index.UpsertExpectations(ctx, pending); err != nil {
		return fmt.Errorf("flush expectations: %w", err)
	}
	for _, pattern := range s.stale {
		if _, err := s.engine.index.MarkStaleExcept(ctx, s.repo, pattern, s.source); err != nil {
			s.logger.Warn("mark_stale_failed", "repo", s.repo, "pattern", pattern, "error", err)
		}
	}
	return nil
}

// applyLateExpectation compares an incoming expectation with a stored object.
// It returns whether the expectation should be saved.
func (e *Engine) applyLateExpectation(ctx context.Context, expectation index.Expectation) (bool, error) {
	object, err := e.index.CurrentObject(ctx, expectation.Repo, expectation.Key)
	if err != nil {
		if errors.Is(err, index.ErrNotFound) {
			return true, nil
		}
		return false, err
	}
	existing, existingErr := e.index.GetExpectation(ctx, expectation.Repo, expectation.Key, expectation.Algo)
	if existingErr != nil && !errors.Is(existingErr, index.ErrNotFound) {
		return false, existingErr
	}
	stored, err := e.digestStored(ctx, object.SHA256, expectation.Algo)
	if err != nil {
		return false, err
	}
	matches := stored == expectation.Digest
	if existingErr == nil && existing.Digest != expectation.Digest {
		if object.Verified {
			e.logger.Error("digest_conflict",
				"repo", expectation.Repo,
				"key", expectation.Key,
				"stored", existing.Digest,
				"declared", expectation.Digest,
			)
			e.verifyFailure(expectation.Repo)
			return false, nil
		}
		return false, e.removeUnverified(ctx, object)
	}
	if matches {
		if err := e.index.SetVerified(ctx, expectation.Repo, expectation.Key, true); err != nil && !errors.Is(err, index.ErrNotFound) {
			return false, err
		}
		return true, nil
	}
	if object.Verified {
		e.logger.Error("digest_conflict",
			"repo", expectation.Repo,
			"key", expectation.Key,
			"stored", stored,
			"declared", expectation.Digest,
		)
		e.verifyFailure(expectation.Repo)
		return false, nil
	}
	return false, e.removeUnverified(ctx, object)
}

func (e *Engine) removeUnverified(ctx context.Context, object index.Object) error {
	e.logger.Error("verify_failed",
		"repo", object.Repo,
		"key", object.Key,
		"sha256", object.SHA256,
	)
	e.verifyFailure(object.Repo)
	return e.store.Reclaim(func() error {
		if err := e.store.Quarantine(object.SHA256); err != nil {
			return err
		}
		_, err := e.index.DeleteKey(ctx, object.Repo, object.Key)
		return err
	})
}

func (e *Engine) digestStored(ctx context.Context, sha, algo string) (string, error) {
	blob, err := e.index.GetBlob(ctx, sha)
	if err != nil {
		return "", err
	}
	reader, err := e.store.Open(sha, store.Encoding(blob.StoredEnc))
	if err != nil {
		return "", err
	}
	hasher, err := integrity.NewHasher(algo)
	if err != nil {
		_ = reader.Close()
		return "", err
	}
	if _, err := io.Copy(hasher, reader); err != nil {
		_ = reader.Close()
		return "", err
	}
	if err := reader.Close(); err != nil {
		return "", err
	}
	return hasher.Sum()[algo], nil
}
