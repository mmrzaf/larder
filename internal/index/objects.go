package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"
)

var shaPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// PutBlob inserts or refreshes a blob row. Existing metadata is corrected so
// a stale row cannot outlive a validated file.
func (db *DB) PutBlob(ctx context.Context, blob Blob) error {
	blob, err := validateBlob(blob)
	if err != nil {
		return err
	}
	if _, err := db.execWrite(ctx, blobUpsert,
		blob.SHA256, blob.RawSize, blob.StoredSize, blob.StoredEnc, unixTime(blob.CreatedAt),
	); err != nil {
		return fmt.Errorf("put blob %s: %w", blob.SHA256, err)
	}
	return nil
}

const blobUpsert = `INSERT INTO blobs (sha256, raw_size, stored_size, stored_enc, refcount, created_at)
	VALUES (?, ?, ?, ?, 0, ?)
	ON CONFLICT(sha256) DO UPDATE SET
	  raw_size = excluded.raw_size,
	  stored_size = excluded.stored_size,
	  stored_enc = excluded.stored_enc`

func validateBlob(blob Blob) (Blob, error) {
	if !shaPattern.MatchString(blob.SHA256) {
		return Blob{}, fmt.Errorf("put blob: invalid sha256 %q", blob.SHA256)
	}
	if blob.RawSize < 0 || blob.StoredSize < 0 {
		return Blob{}, fmt.Errorf("put blob %s: sizes must not be negative", blob.SHA256)
	}
	switch blob.StoredEnc {
	case "", "identity":
		blob.StoredEnc = "identity"
	case "gzip":
	default:
		return Blob{}, fmt.Errorf("put blob %s: invalid encoding %q", blob.SHA256, blob.StoredEnc)
	}
	if blob.CreatedAt.IsZero() {
		return Blob{}, fmt.Errorf("put blob %s: created_at is required", blob.SHA256)
	}
	if err := validateStoredMetadata(blob.RawSize, blob.StoredSize, blob.StoredEnc); err != nil {
		return Blob{}, fmt.Errorf("put blob %s: %w", blob.SHA256, err)
	}
	return blob, nil
}

func validateStoredMetadata(rawSize, storedSize int64, enc string) error {
	if rawSize < 0 || storedSize < 0 {
		return fmt.Errorf("sizes must not be negative")
	}
	if rawSize > 0 && storedSize == 0 {
		return fmt.Errorf("non-empty blob has no stored bytes")
	}
	switch enc {
	case "identity", "gzip":
		return nil
	default:
		return fmt.Errorf("invalid encoding %q", enc)
	}
}

// GetBlob returns one blob row.
func (db *DB) GetBlob(ctx context.Context, sha string) (Blob, error) {
	row := db.sql.QueryRowContext(ctx,
		`SELECT sha256, raw_size, stored_size, stored_enc, refcount, created_at FROM blobs WHERE sha256 = ?`, sha)
	return scanBlob(row)
}

// BlobRefCount returns the current refcount of a blob.
func (db *DB) BlobRefCount(ctx context.Context, sha string) (int64, error) {
	var refcount int64
	if err := db.sql.QueryRowContext(ctx, `SELECT refcount FROM blobs WHERE sha256 = ?`, sha).Scan(&refcount); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("read blob refcount %s: %w", sha, err)
	}
	return refcount, nil
}

// ObjectCountForBlob returns the number of object generations that reference a blob.
func (db *DB) ObjectCountForBlob(ctx context.Context, sha string) (int64, error) {
	var count int64
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM objects WHERE sha256 = ?`, sha).Scan(&count); err != nil {
		return 0, fmt.Errorf("count objects for blob %s: %w", sha, err)
	}
	return count, nil
}

// PublishObject inserts or refreshes a blob row and makes an object reference
// it in one transaction. The object never becomes visible without a blob row
// that describes a non-empty, internally consistent file.
func (db *DB) PublishObject(ctx context.Context, blob Blob, p PutObjectParams) (Object, error) {
	blob, err := validateBlob(blob)
	if err != nil {
		return Object{}, err
	}
	if err := validateObjectParams(p); err != nil {
		return Object{}, err
	}
	var result Object
	err = db.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, blobUpsert,
			blob.SHA256, blob.RawSize, blob.StoredSize, blob.StoredEnc, unixTime(blob.CreatedAt),
		); err != nil {
			return fmt.Errorf("put blob %s: %w", blob.SHA256, err)
		}
		object, err := putObjectTx(ctx, tx, p)
		if err != nil {
			return err
		}
		result = object
		return nil
	})
	if err != nil {
		return Object{}, err
	}
	return result, nil
}

// PutObject inserts the next generation or refreshes the current row when the digest is unchanged.
func (db *DB) PutObject(ctx context.Context, p PutObjectParams) (Object, error) {
	if err := validateObjectParams(p); err != nil {
		return Object{}, err
	}
	var result Object
	err := db.withTx(ctx, func(tx *sql.Tx) error {
		var rawSize, storedSize int64
		var enc string
		if err := tx.QueryRowContext(ctx,
			`SELECT raw_size, stored_size, stored_enc FROM blobs WHERE sha256 = ?`, p.SHA256,
		).Scan(&rawSize, &storedSize, &enc); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("put object %s/%s: blob %s is not in the index", p.Repo, p.Key, p.SHA256)
			}
			return fmt.Errorf("check blob %s: %w", p.SHA256, err)
		}
		if err := validateStoredMetadata(rawSize, storedSize, enc); err != nil {
			return fmt.Errorf("put object %s/%s: blob %s: %w", p.Repo, p.Key, p.SHA256, err)
		}
		object, err := putObjectTx(ctx, tx, p)
		if err != nil {
			return err
		}
		result = object
		return nil
	})
	if err != nil {
		return Object{}, err
	}
	return result, nil
}

func validateObjectParams(p PutObjectParams) error {
	if p.Repo == "" || p.Key == "" {
		return fmt.Errorf("put object: repo and key are required")
	}
	if err := validateClass(p.Class); err != nil {
		return fmt.Errorf("put object %s/%s: %w", p.Repo, p.Key, err)
	}
	if !shaPattern.MatchString(p.SHA256) {
		return fmt.Errorf("put object %s/%s: invalid sha256 %q", p.Repo, p.Key, p.SHA256)
	}
	if p.FetchedAt.IsZero() || p.ValidatedAt.IsZero() {
		return fmt.Errorf("put object %s/%s: fetch and validation times are required", p.Repo, p.Key)
	}
	return nil
}

func putObjectTx(ctx context.Context, tx *sql.Tx, p PutObjectParams) (Object, error) {
	var currentID int64
	var currentSHA string
	var currentVerified int
	err := tx.QueryRowContext(ctx,
		`SELECT id, sha256, verified FROM objects WHERE repo = ? AND key = ? AND current = 1`,
		p.Repo, p.Key,
	).Scan(&currentID, &currentSHA, &currentVerified)
	hasCurrent := true
	if errors.Is(err, sql.ErrNoRows) {
		hasCurrent = false
	} else if err != nil {
		return Object{}, fmt.Errorf("read current object %s/%s: %w", p.Repo, p.Key, err)
	}

	if hasCurrent && currentSHA == p.SHA256 {
		verified := currentVerified == 1 || p.Verified
		if _, err := tx.ExecContext(ctx,
			`UPDATE objects SET content_type = ?, etag = ?, last_modified = ?, upstream = ?,
			 fetched_at = ?, validated_at = ?, verified = ? WHERE id = ?`,
			p.ContentType, p.ETag, p.LastModified, p.Upstream,
			unixTime(p.FetchedAt), unixTime(p.ValidatedAt), boolInt(verified), currentID,
		); err != nil {
			return Object{}, fmt.Errorf("refresh object %s/%s: %w", p.Repo, p.Key, err)
		}
		return objectByID(ctx, tx, currentID)
	}

	var generation int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(generation), 0) FROM objects WHERE repo = ? AND key = ?`,
		p.Repo, p.Key,
	).Scan(&generation); err != nil {
		return Object{}, fmt.Errorf("read generation %s/%s: %w", p.Repo, p.Key, err)
	}
	if hasCurrent {
		if _, err := tx.ExecContext(ctx, `UPDATE objects SET current = 0 WHERE id = ?`, currentID); err != nil {
			return Object{}, fmt.Errorf("retire current object %s/%s: %w", p.Repo, p.Key, err)
		}
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO objects
		 (repo, key, class, generation, current, sha256, content_type, etag, last_modified, upstream,
		  fetched_at, validated_at, last_access, hits, verified)
		 VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
		p.Repo, p.Key, p.Class, generation+1, p.SHA256, p.ContentType, p.ETag, p.LastModified,
		p.Upstream, unixTime(p.FetchedAt), unixTime(p.ValidatedAt), unixTime(p.FetchedAt), boolInt(p.Verified),
	)
	if err != nil {
		return Object{}, fmt.Errorf("insert object %s/%s: %w", p.Repo, p.Key, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Object{}, fmt.Errorf("read object id %s/%s: %w", p.Repo, p.Key, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE blobs SET refcount = refcount + 1 WHERE sha256 = ?`, p.SHA256); err != nil {
		return Object{}, fmt.Errorf("increment blob refcount: %w", err)
	}
	return objectByID(ctx, tx, id)
}

// CurrentObject returns the current generation for a key.
func (db *DB) CurrentObject(ctx context.Context, repo, key string) (Object, error) {
	row := db.sql.QueryRowContext(ctx, objectColumns+
		` WHERE repo = ? AND key = ? AND current = 1`, repo, key)
	return scanObject(row, fmt.Sprintf("%s/%s", repo, key))
}

// Generations lists every generation, newest first.
func (db *DB) Generations(ctx context.Context, repo, key string) ([]Object, error) {
	rows, err := db.sql.QueryContext(ctx, objectColumns+
		` WHERE repo = ? AND key = ? ORDER BY generation DESC`, repo, key)
	if err != nil {
		return nil, fmt.Errorf("list generations %s/%s: %w", repo, key, err)
	}
	defer func() { _ = rows.Close() }()
	var objects []Object
	for rows.Next() {
		obj, err := scanObject(rows, fmt.Sprintf("%s/%s", repo, key))
		if err != nil {
			return nil, err
		}
		objects = append(objects, obj)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list generations %s/%s: %w", repo, key, err)
	}
	return objects, nil
}

// TouchValidated updates the current row's validation time.
func (db *DB) TouchValidated(ctx context.Context, repo, key string, at time.Time) error {
	if at.IsZero() {
		return fmt.Errorf("touch validated %s/%s: time is required", repo, key)
	}
	res, err := db.execWrite(ctx,
		`UPDATE objects SET validated_at = ? WHERE repo = ? AND key = ? AND current = 1`,
		unixTime(at), repo, key)
	if err != nil {
		return fmt.Errorf("touch validated %s/%s: %w", repo, key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("touch validated rows %s/%s: %w", repo, key, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetVerified marks the current generation as verified.
func (db *DB) SetVerified(ctx context.Context, repo, key string, verified bool) error {
	res, err := db.execWrite(ctx,
		`UPDATE objects SET verified = ? WHERE repo = ? AND key = ? AND current = 1`,
		boolInt(verified), repo, key)
	if err != nil {
		return fmt.Errorf("set verified %s/%s: %w", repo, key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set verified rows %s/%s: %w", repo, key, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteObject deletes one generation and decrements its blob refcount.
func (db *DB) DeleteObject(ctx context.Context, id int64) error {
	return db.withTx(ctx, func(tx *sql.Tx) error {
		var sha string
		if err := tx.QueryRowContext(ctx, `SELECT sha256 FROM objects WHERE id = ?`, id).Scan(&sha); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("read object %d: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM objects WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete object %d: %w", id, err)
		}
		if err := decrementRefcount(ctx, tx, sha); err != nil {
			return err
		}
		return nil
	})
}

// DeleteKey deletes every generation of a key and decrements their refcounts.
func (db *DB) DeleteKey(ctx context.Context, repo, key string) (int, error) {
	deleted := 0
	err := db.withTx(ctx, func(tx *sql.Tx) error {
		count := 0
		rows, err := tx.QueryContext(ctx, `SELECT id, sha256 FROM objects WHERE repo = ? AND key = ?`, repo, key)
		if err != nil {
			return fmt.Errorf("list objects %s/%s: %w", repo, key, err)
		}
		type entry struct {
			id  int64
			sha string
		}
		var entries []entry
		for rows.Next() {
			var e entry
			if err := rows.Scan(&e.id, &e.sha); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan object %s/%s: %w", repo, key, err)
			}
			entries = append(entries, e)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("list objects %s/%s: %w", repo, key, err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close objects %s/%s: %w", repo, key, err)
		}
		for _, e := range entries {
			if _, err := tx.ExecContext(ctx, `DELETE FROM objects WHERE id = ?`, e.id); err != nil {
				return fmt.Errorf("delete object %d: %w", e.id, err)
			}
			if err := decrementRefcount(ctx, tx, e.sha); err != nil {
				return err
			}
			count++
		}
		deleted = count
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// EvictionCandidates returns current objects of one class, oldest access first.
func (db *DB) EvictionCandidates(ctx context.Context, class string, accessedBefore time.Time, limit int) ([]Candidate, error) {
	if err := validateClass(class); err != nil {
		return nil, err
	}
	query := `SELECT o.id, o.repo, o.key, o.class, o.generation, o.current, o.sha256, o.content_type,
		o.etag, o.last_modified, o.upstream, o.fetched_at, o.validated_at, o.last_access, o.hits, o.verified,
		b.raw_size, b.stored_size, b.stored_enc
		FROM objects o JOIN blobs b ON b.sha256 = o.sha256
		WHERE o.current = 1 AND o.class = ?`
	args := []any{class}
	if !accessedBefore.IsZero() {
		query += ` AND o.last_access < ?`
		args = append(args, unixTime(accessedBefore))
	}
	query += ` ORDER BY o.last_access ASC, o.id ASC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query eviction candidates: %w", err)
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
			return nil, fmt.Errorf("scan eviction candidate: %w", err)
		}
		c.Current = current == 1
		c.Verified = verified == 1
		c.FetchedAt = fromUnix(fetched)
		c.ValidatedAt = fromUnix(validated)
		c.LastAccess = fromUnix(accessed)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query eviction candidates: %w", err)
	}
	return out, nil
}

// RepoClassTotals returns distinct blob counts and stored bytes per repository and class.
func (db *DB) RepoClassTotals(ctx context.Context) ([]RepoClassTotal, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT repo, class, COUNT(*), COALESCE(SUM(stored_size), 0)
		FROM (
			SELECT o.repo AS repo, o.class AS class, o.sha256 AS sha256, MAX(b.stored_size) AS stored_size
			FROM objects o JOIN blobs b ON b.sha256 = o.sha256
			WHERE o.current = 1
			GROUP BY o.repo, o.class, o.sha256
		)
		GROUP BY repo, class
		ORDER BY repo, class`)
	if err != nil {
		return nil, fmt.Errorf("query repo totals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []RepoClassTotal
	for rows.Next() {
		var total RepoClassTotal
		if err := rows.Scan(&total.Repo, &total.Class, &total.Blobs, &total.StoredBytes); err != nil {
			return nil, fmt.Errorf("scan repo total: %w", err)
		}
		out = append(out, total)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query repo totals: %w", err)
	}
	return out, nil
}

const objectColumns = `SELECT id, repo, key, class, generation, current, sha256, content_type,
	etag, last_modified, upstream, fetched_at, validated_at, last_access, hits, verified FROM objects`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanBlob(row rowScanner) (Blob, error) {
	var blob Blob
	var created int64
	if err := row.Scan(&blob.SHA256, &blob.RawSize, &blob.StoredSize, &blob.StoredEnc, &blob.RefCount, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Blob{}, ErrNotFound
		}
		return Blob{}, fmt.Errorf("scan blob: %w", err)
	}
	blob.CreatedAt = fromUnix(created)
	return blob, nil
}

func scanObject(row rowScanner, label string) (Object, error) {
	var obj Object
	var current, verified int
	var fetched, validated, accessed int64
	if err := row.Scan(
		&obj.ID, &obj.Repo, &obj.Key, &obj.Class, &obj.Generation, &current, &obj.SHA256,
		&obj.ContentType, &obj.ETag, &obj.LastModified, &obj.Upstream,
		&fetched, &validated, &accessed, &obj.Hits, &verified,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Object{}, ErrNotFound
		}
		return Object{}, fmt.Errorf("scan object %s: %w", label, err)
	}
	obj.Current = current == 1
	obj.Verified = verified == 1
	obj.FetchedAt = fromUnix(fetched)
	obj.ValidatedAt = fromUnix(validated)
	obj.LastAccess = fromUnix(accessed)
	return obj, nil
}

func objectByID(ctx context.Context, tx *sql.Tx, id int64) (Object, error) {
	row := tx.QueryRowContext(ctx, objectColumns+` WHERE id = ?`, id)
	return scanObject(row, fmt.Sprintf("id %d", id))
}

func decrementRefcount(ctx context.Context, tx *sql.Tx, sha string) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE blobs SET refcount = CASE WHEN refcount > 0 THEN refcount - 1 ELSE 0 END WHERE sha256 = ?`,
		sha,
	); err != nil {
		return fmt.Errorf("decrement blob refcount %s: %w", sha, err)
	}
	return nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
