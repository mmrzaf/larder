package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// BlobTotals is the number and stored bytes of every blob row.
type BlobTotals struct {
	Blobs       int64
	StoredBytes int64
}

// FreedBlob is a blob row removed together with its last referencing object.
type FreedBlob struct {
	SHA256     string
	StoredSize int64
}

// BlobTotals returns the number and stored bytes of all blob rows.
func (db *DB) BlobTotals(ctx context.Context) (BlobTotals, error) {
	var totals BlobTotals
	if err := db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(stored_size), 0) FROM blobs`,
	).Scan(&totals.Blobs, &totals.StoredBytes); err != nil {
		return BlobTotals{}, fmt.Errorf("read blob totals: %w", err)
	}
	return totals, nil
}

// DeleteObjects removes object generations and their blob rows when the
// reference count reaches zero. Blob files are unlinked by the caller after
// the transaction commits.
func (db *DB) DeleteObjects(ctx context.Context, ids []int64) ([]FreedBlob, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	unique := make([]int64, 0, len(ids))
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
	}

	var freed []FreedBlob
	err := db.withTx(ctx, func(tx *sql.Tx) error {
		var out []FreedBlob
		shas := make(map[string]bool)
		for _, id := range unique {
			var sha string
			if err := tx.QueryRowContext(ctx, `SELECT sha256 FROM objects WHERE id = ?`, id).Scan(&sha); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				return fmt.Errorf("read object %d: %w", id, err)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM objects WHERE id = ?`, id); err != nil {
				return fmt.Errorf("delete object %d: %w", id, err)
			}
			if err := decrementRefcount(ctx, tx, sha); err != nil {
				return err
			}
			shas[sha] = true
		}
		for sha := range shas {
			var refcount, storedSize int64
			if err := tx.QueryRowContext(ctx,
				`SELECT refcount, stored_size FROM blobs WHERE sha256 = ?`, sha,
			).Scan(&refcount, &storedSize); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				return fmt.Errorf("read blob %s: %w", sha, err)
			}
			if refcount > 0 {
				continue
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM blobs WHERE sha256 = ?`, sha); err != nil {
				return fmt.Errorf("delete blob %s: %w", sha, err)
			}
			out = append(out, FreedBlob{SHA256: sha, StoredSize: storedSize})
		}
		freed = out
		return nil
	})
	if err != nil {
		return nil, err
	}
	return freed, nil
}

// ZeroRefBlobs lists blob rows with no referencing objects.
func (db *DB) ZeroRefBlobs(ctx context.Context, limit int) ([]Blob, error) {
	query := `SELECT sha256, raw_size, stored_size, stored_enc, refcount, created_at
		FROM blobs WHERE refcount = 0 ORDER BY created_at ASC, sha256 ASC`
	args := []any{}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list zero-ref blobs: %w", err)
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
		return nil, fmt.Errorf("list zero-ref blobs: %w", err)
	}
	return out, nil
}

// DeleteBlobRows removes blob rows that still have no references.
func (db *DB) DeleteBlobRows(ctx context.Context, shas []string) ([]FreedBlob, error) {
	if len(shas) == 0 {
		return nil, nil
	}
	var freed []FreedBlob
	err := db.withTx(ctx, func(tx *sql.Tx) error {
		var out []FreedBlob
		for _, sha := range shas {
			var storedSize int64
			if err := tx.QueryRowContext(ctx,
				`SELECT stored_size FROM blobs WHERE sha256 = ? AND refcount = 0`, sha,
			).Scan(&storedSize); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				return fmt.Errorf("read blob %s: %w", sha, err)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM blobs WHERE sha256 = ? AND refcount = 0`, sha); err != nil {
				return fmt.Errorf("delete blob %s: %w", sha, err)
			}
			out = append(out, FreedBlob{SHA256: sha, StoredSize: storedSize})
		}
		freed = out
		return nil
	})
	if err != nil {
		return nil, err
	}
	return freed, nil
}

const candidateColumns = `SELECT o.id, o.repo, o.key, o.class, o.generation, o.current, o.sha256,
	o.content_type, o.etag, o.last_modified, o.upstream, o.fetched_at, o.validated_at,
	o.last_access, o.hits, o.verified, b.raw_size, b.stored_size, b.stored_enc
	FROM objects o JOIN blobs b ON b.sha256 = o.sha256`

func (db *DB) queryCandidates(ctx context.Context, query string, args ...any) ([]Candidate, error) {
	rows, err := db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query object candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Candidate
	for rows.Next() {
		var c Candidate
		var current, verified int
		var fetched, validated, accessed int64
		if err := rows.Scan(
			&c.ID, &c.Repo, &c.Key, &c.Class, &c.Generation, &current, &c.SHA256,
			&c.ContentType, &c.ETag, &c.LastModified, &c.Upstream,
			&fetched, &validated, &accessed, &c.Hits, &verified,
			&c.RawSize, &c.StoredSize, &c.StoredEnc,
		); err != nil {
			return nil, fmt.Errorf("scan object candidate: %w", err)
		}
		c.Current = current == 1
		c.Verified = verified == 1
		c.FetchedAt = fromUnix(fetched)
		c.ValidatedAt = fromUnix(validated)
		c.LastAccess = fromUnix(accessed)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query object candidates: %w", err)
	}
	return out, nil
}

// CurrentObjects returns every current object, optionally limited to one repo.
func (db *DB) CurrentObjects(ctx context.Context, repo string) ([]Candidate, error) {
	query := candidateColumns + ` WHERE o.current = 1`
	args := []any{}
	if repo != "" {
		query += ` AND o.repo = ?`
		args = append(args, repo)
	}
	query += ` ORDER BY o.repo ASC, o.key ASC`
	return db.queryCandidates(ctx, query, args...)
}

// HistoryCandidates returns retired mutable generations that are older than
// the newest keep generations of their key.
func (db *DB) HistoryCandidates(ctx context.Context, repo string, keep int) ([]Candidate, error) {
	if keep < 0 {
		return nil, fmt.Errorf("history candidates: keep must not be negative")
	}
	query := candidateColumns + `
		WHERE o.class = 'mutable' AND o.current = 0
		AND (SELECT COUNT(*) FROM objects n
		     WHERE n.repo = o.repo AND n.key = o.key AND n.generation > o.generation) >= ?`
	args := []any{keep}
	if repo != "" {
		query += ` AND o.repo = ?`
		args = append(args, repo)
	}
	query += ` ORDER BY o.repo ASC, o.key ASC, o.generation ASC`
	return db.queryCandidates(ctx, query, args...)
}

// ObjectGeneration returns one generation of a key.
func (db *DB) ObjectGeneration(ctx context.Context, repo, key string, generation int64) (Object, error) {
	row := db.sql.QueryRowContext(ctx, objectColumns+
		` WHERE repo = ? AND key = ? AND generation = ?`, repo, key, generation)
	return scanObject(row, fmt.Sprintf("%s/%s#%d", repo, key, generation))
}

// Rollback makes a generation current. A zero generation selects the newest
// retired generation below the current one.
func (db *DB) Rollback(ctx context.Context, repo, key string, generation int64) (Object, error) {
	if repo == "" || key == "" {
		return Object{}, fmt.Errorf("rollback: repo and key are required")
	}
	var result Object
	err := db.withTx(ctx, func(tx *sql.Tx) error {
		var currentGeneration int64
		if err := tx.QueryRowContext(ctx,
			`SELECT generation FROM objects WHERE repo = ? AND key = ? AND current = 1`,
			repo, key,
		).Scan(&currentGeneration); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("read current generation %s/%s: %w", repo, key, err)
		}
		target := generation
		if target == 0 {
			if err := tx.QueryRowContext(ctx,
				`SELECT COALESCE(MAX(generation), 0) FROM objects
				 WHERE repo = ? AND key = ? AND generation < ?`,
				repo, key, currentGeneration,
			).Scan(&target); err != nil {
				return fmt.Errorf("read previous generation %s/%s: %w", repo, key, err)
			}
			if target == 0 {
				return ErrNotFound
			}
		}
		var id int64
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM objects WHERE repo = ? AND key = ? AND generation = ?`,
			repo, key, target,
		).Scan(&id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("read generation %s/%s#%d: %w", repo, key, target, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE objects SET current = 0 WHERE repo = ? AND key = ? AND current = 1`,
			repo, key,
		); err != nil {
			return fmt.Errorf("retire current %s/%s: %w", repo, key, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE objects SET current = 1 WHERE id = ?`, id); err != nil {
			return fmt.Errorf("activate generation %s/%s#%d: %w", repo, key, target, err)
		}
		var err error
		result, err = objectByID(ctx, tx, id)
		return err
	})
	if err != nil {
		return Object{}, err
	}
	return result, nil
}

// ObjectCount is the number of current objects for one repo and class.
type ObjectCount struct {
	Repo    string `json:"repo"`
	Class   string `json:"class"`
	Objects int64  `json:"objects"`
}

// ObjectCounts returns current object counts per repository and class.
func (db *DB) ObjectCounts(ctx context.Context) ([]ObjectCount, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT repo, class, COUNT(*) FROM objects WHERE current = 1 GROUP BY repo, class ORDER BY repo, class`)
	if err != nil {
		return nil, fmt.Errorf("query object counts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ObjectCount
	for rows.Next() {
		var count ObjectCount
		if err := rows.Scan(&count.Repo, &count.Class, &count.Objects); err != nil {
			return nil, fmt.Errorf("scan object count: %w", err)
		}
		out = append(out, count)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query object counts: %w", err)
	}
	return out, nil
}

// RefCounts returns every blob digest and its reference count.
func (db *DB) RefCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT sha256, refcount FROM blobs`)
	if err != nil {
		return nil, fmt.Errorf("query refcounts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]int64)
	for rows.Next() {
		var sha string
		var refcount int64
		if err := rows.Scan(&sha, &refcount); err != nil {
			return nil, fmt.Errorf("scan refcount: %w", err)
		}
		out[sha] = refcount
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query refcounts: %w", err)
	}
	return out, nil
}

// KeyCandidates returns every generation of a key with blob sizes.
func (db *DB) KeyCandidates(ctx context.Context, repo, key string) ([]Candidate, error) {
	query := candidateColumns + ` WHERE o.repo = ? AND o.key = ? ORDER BY o.generation ASC`
	return db.queryCandidates(ctx, query, repo, key)
}
