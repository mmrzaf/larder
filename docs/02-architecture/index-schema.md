# Index schema

Embedded SQLite, one file, WAL mode, `busy_timeout` 5000 ms, foreign keys on. Schema version stored in `meta`. Writers are serialised through one connection at a time; a lock held by another process is retried with bounded backoff.

```sql
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);

CREATE TABLE blobs (
  sha256        TEXT PRIMARY KEY,          -- raw bytes digest
  raw_size      INTEGER NOT NULL,
  stored_size   INTEGER NOT NULL,          -- bytes on disk
  stored_enc    TEXT NOT NULL DEFAULT 'identity',  -- identity | gzip
  refcount      INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL
);

CREATE TABLE objects (
  id            INTEGER PRIMARY KEY,
  repo          TEXT NOT NULL,
  key           TEXT NOT NULL,             -- normalized path [#variant]
  class         TEXT NOT NULL,             -- immutable | mutable
  generation    INTEGER NOT NULL DEFAULT 1,
  current       INTEGER NOT NULL DEFAULT 1,
  sha256        TEXT NOT NULL REFERENCES blobs(sha256),
  content_type  TEXT NOT NULL DEFAULT '',
  etag          TEXT NOT NULL DEFAULT '',
  last_modified TEXT NOT NULL DEFAULT '',
  upstream      TEXT NOT NULL DEFAULT '',  -- host that supplied it
  fetched_at    INTEGER NOT NULL,
  validated_at  INTEGER NOT NULL,
  last_access   INTEGER NOT NULL,
  hits          INTEGER NOT NULL DEFAULT 0,
  verified      INTEGER NOT NULL DEFAULT 0 -- 0 unverified, 1 verified
);
CREATE UNIQUE INDEX objects_current ON objects(repo, key) WHERE current = 1;
CREATE INDEX objects_gen ON objects(repo, key, generation);
CREATE INDEX objects_evict ON objects(class, current, last_access);
CREATE INDEX objects_repo ON objects(repo, class);
CREATE INDEX objects_sha ON objects(sha256);

CREATE TABLE expectations (
  repo    TEXT NOT NULL,
  key     TEXT NOT NULL,                   -- key of the object the digest applies to
  algo    TEXT NOT NULL,                   -- sha256 | sha512 | sha1 | md5
  digest  TEXT NOT NULL,                   -- lowercase hex
  source  TEXT NOT NULL,                   -- key of the document that declared it
  created_at INTEGER NOT NULL,
  PRIMARY KEY (repo, key, algo)
);

CREATE TABLE pin_sets (name TEXT PRIMARY KEY, created_at INTEGER NOT NULL, note TEXT NOT NULL DEFAULT '');
CREATE TABLE pin_items (set_name TEXT NOT NULL REFERENCES pin_sets(name) ON DELETE CASCADE,
                        repo TEXT NOT NULL, key TEXT NOT NULL,
                        PRIMARY KEY (set_name, repo, key));
CREATE TABLE pin_rules (set_name TEXT NOT NULL REFERENCES pin_sets(name) ON DELETE CASCADE,
                        repo TEXT NOT NULL, pattern TEXT NOT NULL,   -- glob on key
                        PRIMARY KEY (set_name, repo, pattern));
```

## Access-time updates

`last_access` and `hits` are held in memory and flushed in one batched transaction every 30 seconds and on shutdown. Reads never write synchronously.

## Reference counting

`blobs.refcount` counts objects (all generations) pointing at the blob. It is incremented and decremented in the same transaction as the object row change. Publishing a new object inserts or corrects the blob row, writes the object and increments the count in one transaction, so an object is never visible without a complete blob row. `larder reindex --check` recomputes refcounts from `objects` and reports differences. A blob with refcount 0 is deleted by garbage collection.

## Pinned status

An object is pinned if it matches any `pin_items` row or any `pin_rules` glob. Pins apply to the current generation and to any later generation of the same key. Pinned status is computed at eviction time, not stored on `objects`.

## Expectation lifecycle

Expectations are inserted when a metadata document is ingested (`Ingest` hook) and are replaced by newer documents. When an expectation is added for a key that already has a stored object, the engine compares it immediately: a match sets `verified = 1`; a mismatch quarantines the blob and removes the object (see [integrity](integrity.md)).

## Migrations

Forward-only, numbered, applied in one transaction at startup. A database with a newer schema version than the binary refuses to start.
