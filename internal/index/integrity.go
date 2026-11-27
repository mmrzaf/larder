package index

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// ObjectRef identifies an object generation without its payload.
type ObjectRef struct {
	Repo       string `json:"repo"`
	Key        string `json:"key"`
	Generation int64  `json:"generation"`
	SHA256     string `json:"sha256"`
	Current    bool   `json:"current"`
}

// AllObjects returns every object generation.
func (db *DB) AllObjects(ctx context.Context) ([]ObjectRef, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT repo, key, generation, sha256, current FROM objects ORDER BY repo, key, generation`)
	if err != nil {
		return nil, fmt.Errorf("list all objects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ObjectRef
	for rows.Next() {
		var ref ObjectRef
		var current int
		if err := rows.Scan(&ref.Repo, &ref.Key, &ref.Generation, &ref.SHA256, &current); err != nil {
			return nil, fmt.Errorf("scan object ref: %w", err)
		}
		ref.Current = current == 1
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list all objects: %w", err)
	}
	return out, nil
}

// AllBlobs returns every blob row ordered by digest.
func (db *DB) AllBlobs(ctx context.Context) ([]Blob, error) {
	return db.queryBlobs(ctx,
		`SELECT sha256, raw_size, stored_size, stored_enc, refcount, created_at FROM blobs ORDER BY sha256`)
}

// BlobPage returns one page of blob rows after a digest.
func (db *DB) BlobPage(ctx context.Context, after string, limit int) ([]Blob, error) {
	query := `SELECT sha256, raw_size, stored_size, stored_enc, refcount, created_at
		FROM blobs WHERE sha256 > ? ORDER BY sha256`
	args := []any{after}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	return db.queryBlobs(ctx, query, args...)
}

// BlobsForRepo returns the distinct blobs referenced by objects of one repo.
func (db *DB) BlobsForRepo(ctx context.Context, repo string) ([]Blob, error) {
	if repo == "" {
		return nil, fmt.Errorf("list repo blobs: repo is required")
	}
	return db.queryBlobs(ctx,
		`SELECT DISTINCT b.sha256, b.raw_size, b.stored_size, b.stored_enc, b.refcount, b.created_at
		 FROM blobs b JOIN objects o ON o.sha256 = b.sha256
		 WHERE o.repo = ? ORDER BY b.sha256`, repo)
}

func (db *DB) queryBlobs(ctx context.Context, query string, args ...any) ([]Blob, error) {
	rows, err := db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query blobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Blob
	for rows.Next() {
		blob, err := scanBlob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, blob)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query blobs: %w", err)
	}
	return out, nil
}

// BlobCount returns the number of blob rows.
func (db *DB) BlobCount(ctx context.Context) (int64, error) {
	var count int64
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM blobs`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count blobs: %w", err)
	}
	return count, nil
}

// DeleteBlobAndObjects removes every object referencing a blob and the blob row.
func (db *DB) DeleteBlobAndObjects(ctx context.Context, sha string) ([]ObjectRef, error) {
	var refs []ObjectRef
	err := db.withTx(ctx, func(tx *sql.Tx) error {
		var out []ObjectRef
		rows, err := tx.QueryContext(ctx,
			`SELECT repo, key, generation, sha256, current FROM objects WHERE sha256 = ? ORDER BY repo, key, generation`, sha)
		if err != nil {
			return fmt.Errorf("list objects for blob %s: %w", sha, err)
		}
		for rows.Next() {
			var ref ObjectRef
			var current int
			if err := rows.Scan(&ref.Repo, &ref.Key, &ref.Generation, &ref.SHA256, &current); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan object for blob %s: %w", sha, err)
			}
			ref.Current = current == 1
			out = append(out, ref)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("list objects for blob %s: %w", sha, err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close objects for blob %s: %w", sha, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM objects WHERE sha256 = ?`, sha); err != nil {
			return fmt.Errorf("delete objects for blob %s: %w", sha, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM blobs WHERE sha256 = ?`, sha); err != nil {
			return fmt.Errorf("delete blob %s: %w", sha, err)
		}
		refs = out
		return nil
	})
	if err != nil {
		return nil, err
	}
	return refs, nil
}

// RefcountMismatch is a blob whose stored reference count differs from its objects.
type RefcountMismatch struct {
	SHA256   string `json:"sha256"`
	Stored   int64  `json:"stored"`
	Computed int64  `json:"computed"`
}

// RefcountMismatches returns blobs whose refcount differs from the object count.
func (db *DB) RefcountMismatches(ctx context.Context) ([]RefcountMismatch, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT b.sha256, b.refcount, COUNT(o.id)
		 FROM blobs b LEFT JOIN objects o ON o.sha256 = b.sha256
		 GROUP BY b.sha256, b.refcount
		 HAVING b.refcount != COUNT(o.id)
		 ORDER BY b.sha256`)
	if err != nil {
		return nil, fmt.Errorf("query refcount mismatches: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []RefcountMismatch
	for rows.Next() {
		var mismatch RefcountMismatch
		if err := rows.Scan(&mismatch.SHA256, &mismatch.Stored, &mismatch.Computed); err != nil {
			return nil, fmt.Errorf("scan refcount mismatch: %w", err)
		}
		out = append(out, mismatch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query refcount mismatches: %w", err)
	}
	return out, nil
}

// RecomputeRefcounts sets every blob refcount from its object rows.
func (db *DB) RecomputeRefcounts(ctx context.Context) error {
	if _, err := db.execWrite(ctx,
		`UPDATE blobs SET refcount = (
			SELECT COUNT(*) FROM objects WHERE objects.sha256 = blobs.sha256
		)`); err != nil {
		return fmt.Errorf("recompute refcounts: %w", err)
	}
	return nil
}

// DuplicateCurrent is a key with more than one current generation.
type DuplicateCurrent struct {
	Repo  string `json:"repo"`
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

// DuplicateCurrents lists keys with multiple current generations.
func (db *DB) DuplicateCurrents(ctx context.Context) ([]DuplicateCurrent, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT repo, key, COUNT(*) FROM objects WHERE current = 1
		 GROUP BY repo, key HAVING COUNT(*) > 1 ORDER BY repo, key`)
	if err != nil {
		return nil, fmt.Errorf("query duplicate current objects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []DuplicateCurrent
	for rows.Next() {
		var duplicate DuplicateCurrent
		if err := rows.Scan(&duplicate.Repo, &duplicate.Key, &duplicate.Count); err != nil {
			return nil, fmt.Errorf("scan duplicate current: %w", err)
		}
		out = append(out, duplicate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query duplicate current objects: %w", err)
	}
	return out, nil
}

// FixDuplicateCurrents keeps the newest generation current for every key.
func (db *DB) FixDuplicateCurrents(ctx context.Context) error {
	return db.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT repo, key FROM objects WHERE current = 1
			 GROUP BY repo, key HAVING COUNT(*) > 1`)
		if err != nil {
			return fmt.Errorf("list duplicate current objects: %w", err)
		}
		type key struct{ repo, name string }
		var keys []key
		for rows.Next() {
			var entry key
			if err := rows.Scan(&entry.repo, &entry.name); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan duplicate current: %w", err)
			}
			keys = append(keys, entry)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("list duplicate current objects: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close duplicate current objects: %w", err)
		}
		for _, entry := range keys {
			var id int64
			if err := tx.QueryRowContext(ctx,
				`SELECT id FROM objects WHERE repo = ? AND key = ? ORDER BY generation DESC LIMIT 1`,
				entry.repo, entry.name,
			).Scan(&id); err != nil {
				return fmt.Errorf("read newest generation %s/%s: %w", entry.repo, entry.name, err)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE objects SET current = CASE WHEN id = ? THEN 1 ELSE 0 END
				 WHERE repo = ? AND key = ?`, id, entry.repo, entry.name,
			); err != nil {
				return fmt.Errorf("fix current generation %s/%s: %w", entry.repo, entry.name, err)
			}
		}
		return nil
	})
}

// ExpectationOrphanCount counts expectations without a current object.
func (db *DB) ExpectationOrphanCount(ctx context.Context) (int64, error) {
	var count int64
	if err := db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM expectations e
		 WHERE NOT EXISTS (
			SELECT 1 FROM objects o WHERE o.repo = e.repo AND o.key = e.key AND o.current = 1
		 )`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count expectation orphans: %w", err)
	}
	return count, nil
}

// DeleteExpectationOrphans removes expectations without a current object.
func (db *DB) DeleteExpectationOrphans(ctx context.Context) (int64, error) {
	res, err := db.execWrite(ctx,
		`DELETE FROM expectations
		 WHERE NOT EXISTS (
			SELECT 1 FROM objects o WHERE o.repo = expectations.repo AND o.key = expectations.key AND o.current = 1
		 )`)
	if err != nil {
		return 0, fmt.Errorf("delete expectation orphans: %w", err)
	}
	count, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count deleted expectations: %w", err)
	}
	return count, nil
}

// ExpectationMismatch is a stored object whose sha256 expectation differs.
type ExpectationMismatch struct {
	Repo      string `json:"repo"`
	Key       string `json:"key"`
	SHA256    string `json:"sha256"`
	Expect    string `json:"expected"`
	Algorithm string `json:"algo"`
}

// ExpectationMismatches returns current objects that disagree with a sha256 expectation.
func (db *DB) ExpectationMismatches(ctx context.Context, repo string) ([]ExpectationMismatch, error) {
	query := `SELECT o.repo, o.key, o.sha256, e.algo, e.digest
		FROM objects o JOIN expectations e ON e.repo = o.repo AND e.key = o.key
		WHERE o.current = 1 AND e.algo = 'sha256' AND e.digest != o.sha256`
	args := []any{}
	if repo != "" {
		query += ` AND o.repo = ?`
		args = append(args, repo)
	}
	query += ` ORDER BY o.repo, o.key`
	rows, err := db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query expectation mismatches: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ExpectationMismatch
	for rows.Next() {
		var mismatch ExpectationMismatch
		if err := rows.Scan(&mismatch.Repo, &mismatch.Key, &mismatch.SHA256, &mismatch.Algorithm, &mismatch.Expect); err != nil {
			return nil, fmt.Errorf("scan expectation mismatch: %w", err)
		}
		out = append(out, mismatch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query expectation mismatches: %w", err)
	}
	return out, nil
}

// UnverifiedCount counts current objects without a verified digest.
func (db *DB) UnverifiedCount(ctx context.Context, repo string) (int64, error) {
	query := `SELECT COUNT(*) FROM objects WHERE current = 1 AND verified = 0`
	args := []any{}
	if repo != "" {
		query += ` AND repo = ?`
		args = append(args, repo)
	}
	var count int64
	if err := db.sql.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("count unverified objects: %w", err)
	}
	return count, nil
}

// VacuumInto writes a consistent snapshot of the database to path.
func (db *DB) VacuumInto(ctx context.Context, path string) error {
	if path == "" {
		return fmt.Errorf("snapshot: destination is required")
	}
	quoted := strings.ReplaceAll(path, "'", "''")
	if _, err := db.sql.ExecContext(ctx, `VACUUM INTO '`+quoted+`'`); err != nil {
		return fmt.Errorf("snapshot index: %w", err)
	}
	return nil
}

// ExpectationRow is one expectation attached to a current object.
type ExpectationRow struct {
	Repo   string
	Key    string
	SHA256 string
	Algo   string
	Digest string
}

// ExpectationRows lists expectations for current objects.
func (db *DB) ExpectationRows(ctx context.Context, repo string) ([]ExpectationRow, error) {
	query := `SELECT o.repo, o.key, o.sha256, e.algo, e.digest
		FROM objects o JOIN expectations e ON e.repo = o.repo AND e.key = o.key
		WHERE o.current = 1`
	args := []any{}
	if repo != "" {
		query += ` AND o.repo = ?`
		args = append(args, repo)
	}
	query += ` ORDER BY o.repo, o.key, e.algo`
	rows, err := db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query expectation rows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ExpectationRow
	for rows.Next() {
		var row ExpectationRow
		if err := rows.Scan(&row.Repo, &row.Key, &row.SHA256, &row.Algo, &row.Digest); err != nil {
			return nil, fmt.Errorf("scan expectation row: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query expectation rows: %w", err)
	}
	return out, nil
}
