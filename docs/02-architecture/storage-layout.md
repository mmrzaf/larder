# Storage layout

```
<data_dir>/
  larder.lock            exclusive lock held by the server
  index.db               SQLite database (WAL mode)
  index.db-wal, index.db-shm
  blobs/aa/bb/<sha256>   content-addressed blobs (raw or gzip at rest)
  tmp/                   in-progress downloads (<random>.part)
  quarantine/            blobs that failed verification
  snapshots/             index snapshots (index-YYYYMMDD-HHMM.db)
```

All directories are created with mode 0750, files with 0640. `data_dir` must be on one filesystem (renames from `tmp/` to `blobs/` must be atomic).

## Blob naming

`blobs/<first 2 hex>/<next 2 hex>/<64 hex sha256 of the raw bytes>`. The name always describes the raw bytes, even when stored gzip-compressed (the index records `stored_encoding`).

## Writing a blob

1. Open `tmp/<random>.part`, stream the body into it while computing sha256 (and any additional digest an expectation needs, for example sha512 or sha1).
2. Check length and expectations. Failure: delete the temp file.
3. `fsync` the file.
4. If the final blob path already exists (same content stored before), delete the temp file; otherwise `rename` into place, creating directories as needed.
5. Insert the rows in one transaction.

Text-class objects (JSON, XML, HTML, plain text, TOML, apt `Packages` and `Release` files) are written gzip-compressed when `compress_metadata` is true (default). The sha256 is computed over the raw bytes before compression. Binary artifacts are never recompressed.

Serving a gzip-at-rest blob: if the client sends `Accept-Encoding: gzip`, send the compressed bytes with `Content-Encoding: gzip`; otherwise decompress on the fly. Range requests on gzip-at-rest blobs are served from the decompressed stream (metadata documents are small).

## Startup recovery

1. Take `larder.lock`.
2. Delete everything in `tmp/` older than 1 minute (all of it, since no downloads are running yet).
3. Open the database, run migrations, run `PRAGMA quick_check`. If it fails: refuse to start and print the instruction to restore from `snapshots/` or run `larder reindex --rebuild-blobs-only`.
4. Reconcile in the background: delete rows whose blob file is missing; list blobs with no rows older than 1 hour and delete them (orphans are unrecoverable because the keys live only in the index).

## Crash safety

- A blob becomes visible only by atomic rename after a successful fsync.
- Rows are written after the rename. A crash between the two leaves an orphan blob, removed by reconciliation.
- The index uses WAL with `synchronous=FULL`: a committed transaction survives an OS crash. A blob is flushed and renamed into place before any index row refers to it, so a crash leaves at worst an unreferenced blob file, which is removed once it is older than an hour. No state is ever corrupt.
- The database is snapshotted daily with `VACUUM INTO`; `snapshot_keep` (default 7) snapshots are retained. Snapshots are small compared with blobs.

## Static export

`larder export` writes a path-mirrored tree (hardlinks when on the same filesystem, copies otherwise) with kind rewriting applied for a given `--base-url`, so that a plain web server can serve the data. See [CLI](../04-cli/storage-commands.md).
