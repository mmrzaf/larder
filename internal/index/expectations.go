package index

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

// UpsertExpectation records or replaces the digest for a key and algorithm.
func (db *DB) UpsertExpectation(ctx context.Context, e Expectation) error {
	if err := validateExpectation(e); err != nil {
		return err
	}
	_, err := db.sql.ExecContext(ctx, expectationUpsert, e.Repo, e.Key, e.Algo, e.Digest, e.Source, unixTime(e.CreatedAt))
	if err != nil {
		return fmt.Errorf("upsert expectation %s/%s: %w", e.Repo, e.Key, err)
	}
	return nil
}

// UpsertExpectations records or replaces a batch of expectations in one transaction.
func (db *DB) UpsertExpectations(ctx context.Context, expectations []Expectation) error {
	for _, e := range expectations {
		if err := validateExpectation(e); err != nil {
			return err
		}
	}
	if len(expectations) == 0 {
		return nil
	}
	return db.withTx(ctx, func(tx *sql.Tx) error {
		for _, e := range expectations {
			if _, err := tx.ExecContext(ctx, expectationUpsert, e.Repo, e.Key, e.Algo, e.Digest, e.Source, unixTime(e.CreatedAt)); err != nil {
				return fmt.Errorf("upsert expectation %s/%s: %w", e.Repo, e.Key, err)
			}
		}
		return nil
	})
}

const expectationUpsert = `INSERT INTO expectations (repo, key, algo, digest, source, created_at)
	VALUES (?, ?, ?, ?, ?, ?)
	ON CONFLICT(repo, key, algo) DO UPDATE SET
	  digest = excluded.digest,
	  source = excluded.source,
	  created_at = excluded.created_at`

func validateExpectation(e Expectation) error {
	if e.Repo == "" || e.Key == "" {
		return fmt.Errorf("upsert expectation: repo and key are required")
	}
	if e.Algo == "h1" {
		if _, err := base64.StdEncoding.DecodeString(e.Digest); err != nil || e.Digest == "" {
			return fmt.Errorf("upsert expectation %s/%s: h1 digest is not base64", e.Repo, e.Key)
		}
	} else {
		if err := validateAlgo(e.Algo); err != nil {
			return fmt.Errorf("upsert expectation %s/%s: %w", e.Repo, e.Key, err)
		}
		if _, err := hex.DecodeString(e.Digest); err != nil {
			return fmt.Errorf("upsert expectation %s/%s: digest is not hexadecimal", e.Repo, e.Key)
		}
	}
	if e.Source == "" {
		return fmt.Errorf("upsert expectation %s/%s: source is required", e.Repo, e.Key)
	}
	if e.CreatedAt.IsZero() {
		return fmt.Errorf("upsert expectation %s/%s: created_at is required", e.Repo, e.Key)
	}
	return nil
}

// ListUnverified returns current unverified immutable objects in id order.
func (db *DB) ListUnverified(ctx context.Context, limit int) ([]Object, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := db.sql.QueryContext(ctx,
		objectColumns+` WHERE current = 1 AND class = 'immutable' AND verified = 0
		 ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list unverified objects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Object
	for rows.Next() {
		object, err := scanObject(rows, "unverified object")
		if err != nil {
			return nil, fmt.Errorf("list unverified objects: %w", err)
		}
		out = append(out, object)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list unverified objects: %w", err)
	}
	return out, nil
}

// MarkStale resets the validation time of current mutable objects matching a key glob.
func (db *DB) MarkStale(ctx context.Context, repo, pattern string) (int, error) {
	return db.MarkStaleExcept(ctx, repo, pattern, "")
}

// MarkStaleExcept is MarkStale with one key excluded from the update.
func (db *DB) MarkStaleExcept(ctx context.Context, repo, pattern, except string) (int, error) {
	if repo == "" || pattern == "" {
		return 0, fmt.Errorf("mark stale: repo and pattern are required")
	}
	updated := 0
	err := db.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT id, key FROM objects WHERE repo = ? AND current = 1 AND class = 'mutable'`, repo)
		if err != nil {
			return fmt.Errorf("list mutable objects %s: %w", repo, err)
		}
		type entry struct {
			id  int64
			key string
		}
		var entries []entry
		for rows.Next() {
			var e entry
			if err := rows.Scan(&e.id, &e.key); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan mutable object %s: %w", repo, err)
			}
			if e.key != except && matchGlob(pattern, e.key) {
				entries = append(entries, e)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("list mutable objects %s: %w", repo, err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close mutable objects %s: %w", repo, err)
		}
		for _, e := range entries {
			if _, err := tx.ExecContext(ctx, `UPDATE objects SET validated_at = 0 WHERE id = ?`, e.id); err != nil {
				return fmt.Errorf("mark object %d stale: %w", e.id, err)
			}
			updated++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return updated, nil
}

// LookupExpectations returns every digest recorded for a key.
func (db *DB) LookupExpectations(ctx context.Context, repo, key string) ([]Expectation, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT repo, key, algo, digest, source, created_at FROM expectations
		 WHERE repo = ? AND key = ? ORDER BY algo`, repo, key)
	if err != nil {
		return nil, fmt.Errorf("lookup expectations %s/%s: %w", repo, key, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Expectation
	for rows.Next() {
		var e Expectation
		var created int64
		if err := rows.Scan(&e.Repo, &e.Key, &e.Algo, &e.Digest, &e.Source, &created); err != nil {
			return nil, fmt.Errorf("scan expectation %s/%s: %w", repo, key, err)
		}
		e.CreatedAt = fromUnix(created)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lookup expectations %s/%s: %w", repo, key, err)
	}
	return out, nil
}

// GetExpectation returns the expectation for one algorithm.
func (db *DB) GetExpectation(ctx context.Context, repo, key, algo string) (Expectation, error) {
	var e Expectation
	var created int64
	err := db.sql.QueryRowContext(ctx,
		`SELECT repo, key, algo, digest, source, created_at FROM expectations
		 WHERE repo = ? AND key = ? AND algo = ?`, repo, key, algo,
	).Scan(&e.Repo, &e.Key, &e.Algo, &e.Digest, &e.Source, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Expectation{}, ErrNotFound
	}
	if err != nil {
		return Expectation{}, fmt.Errorf("get expectation %s/%s %s: %w", repo, key, algo, err)
	}
	e.CreatedAt = fromUnix(created)
	return e, nil
}

// ExpectationsFor returns every digest expectation stored for a key.
func (db *DB) ExpectationsFor(ctx context.Context, repo, key string) ([]Expectation, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT repo, key, algo, digest, source, created_at FROM expectations
		 WHERE repo = ? AND key = ? ORDER BY algo`, repo, key)
	if err != nil {
		return nil, fmt.Errorf("list expectations %s/%s: %w", repo, key, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Expectation
	for rows.Next() {
		var expectation Expectation
		var created int64
		if err := rows.Scan(&expectation.Repo, &expectation.Key, &expectation.Algo,
			&expectation.Digest, &expectation.Source, &created); err != nil {
			return nil, fmt.Errorf("scan expectation %s/%s: %w", repo, key, err)
		}
		expectation.CreatedAt = fromUnix(created)
		out = append(out, expectation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list expectations %s/%s: %w", repo, key, err)
	}
	return out, nil
}
